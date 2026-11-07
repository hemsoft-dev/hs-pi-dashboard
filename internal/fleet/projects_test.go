package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/projects"
	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
)

func TestProjectsFetchAndFailureRetainsCache(t *testing.T) {
	data, e := os.ReadFile("../projects/testdata/gh-x-v0.22.0.json")
	if e != nil {
		t.Fatal(e)
	}
	doc, e := projects.Parse(data)
	if e != nil {
		t.Fatal(e)
	}
	fail := false
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/projects" {
			t.Errorf("wrong path %s", r.URL.Path)
		}
		if fail {
			http.Error(w, "no", 500)
			return
		}
		writeJSON(w, 200, projects.Snapshot{Results: []projects.Result{{Path: "/repo", Document: doc}}})
	}))
	defer agent.Close()
	target := Target{Name: "mini", URL: agent.URL}
	s := NewServer([]Target{target}, 0)
	snap := s.fetchProjects(context.Background(), target)
	if len(snap.Results) != 1 || snap.Error != "" {
		t.Fatal(snap)
	}
	s.cache["mini"] = MachineState{Online: true, Projects: snap}
	fail = true
	stale := s.fetchProjects(context.Background(), target)
	if stale.Error == "" || len(stale.Results) != 1 || snap.Error != "" {
		t.Fatal("failure hidden or mutated snapshot")
	}
	s.cache["mini"] = MachineState{Online: true, Projects: stale}
	view := s.cachedFleet()
	if len(view.Projects.Repositories) != 1 || len(view.Projects.Errors) != 1 {
		t.Fatal("merge not exposed", view.Projects)
	}
	fail = false
	if recovered := s.fetchProjects(context.Background(), target); recovered.Error != "" {
		t.Fatal("did not recover")
	}
}

// failedFirstSweep is the result a fresh projects.Store publishes when its
// first gh-x run fails: an error and no document. internal/projects
// TestFreshStoreFailedFirstSweepHasNoDocument pins this output.
const failedFirstSweep = "gh x status --json failed; check gh-x installation, checkout and host authentication"

func projectFixture(t *testing.T) *projects.Document {
	t.Helper()
	data, err := os.ReadFile("../projects/testdata/gh-x-v0.22.0.json")
	if err != nil {
		t.Fatal(err)
	}
	doc, err := projects.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// projectsView merges the cached machine as cachedFleet does, without reading
// the host's Hermes database.
func projectsView(s *Server, name string) projects.View {
	s.mu.Lock()
	state := s.cache[name]
	s.mu.Unlock()
	return projects.Merge([]projects.Machine{{Name: name, Online: state.Online, Snapshot: state.Projects}})
}

func findRepository(view projects.View, name string) *projects.Repository {
	for i := range view.Repositories {
		if view.Repositories[i].Name == name {
			return &view.Repositories[i]
		}
	}
	return nil
}

// A restarted agent's pending or failed first sweep must not drop the
// document the fleet already holds for that path (#122).
func TestRestartedAgentKeepsLastProjectsDocument(t *testing.T) {
	doc := projectFixture(t)
	repoName := doc.Repository.Name
	// projects.New cleans paths, so a Windows agent reports \repo.
	repoPath := filepath.Clean("/repo")
	var reply atomic.Pointer[projects.Snapshot]
	var failing atomic.Bool
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sessions":
			writeJSON(w, 200, sessions.Snapshot{Machine: "mini", GeneratedAt: time.Now(), SessionsDir: "/pi", Sessions: []sessions.Summary{}})
		case "/projects":
			if failing.Load() {
				http.Error(w, "no", 500)
				return
			}
			writeJSON(w, 200, reply.Load())
		default:
			http.NotFound(w, r)
		}
	}))
	defer agentServer.Close()
	s := NewServer([]Target{{Name: "mini", URL: agentServer.URL}}, time.Hour)
	poll := func(snap projects.Snapshot) projects.View {
		t.Helper()
		reply.Store(&snap)
		s.poll(context.Background())
		return projectsView(s, "mini")
	}
	local := func(phase string, view projects.View, wantDoc *projects.Document, wantError string) {
		t.Helper()
		repo := findRepository(view, repoName)
		if repo == nil || len(view.Repositories) != 1 || len(repo.Locals) != 1 {
			t.Fatalf("%s: repository %q not grouped: %+v", phase, repoName, view.Repositories)
		}
		if !sameDocument(repo.GitHub, wantDoc) || !sameDocument(repo.Locals[0].Document, wantDoc) || repo.Locals[0].Error != wantError || repo.Locals[0].Path != repoPath {
			t.Fatalf("%s: github=%v local=%+v, want document at %s and error %q", phase, repo.GitHub != nil, repo.Locals[0].Result, wantDoc.GeneratedAt, wantError)
		}
	}

	// Positive control: a normal sweep.
	normal := poll(projects.Snapshot{GeneratedAt: time.Now(), Results: []projects.Result{{Path: repoPath, Document: doc}}})
	local("normal sweep", normal, doc, "")

	// Positive control: an endpoint failure keeps the cached snapshot with the
	// endpoint message.
	failing.Store(true)
	s.poll(context.Background())
	unavailable := projectsView(s, "mini")
	local("endpoint failure", unavailable, doc, "")
	if len(unavailable.Errors) != 1 || unavailable.Errors[0] != "mini: "+projectsUnavailable {
		t.Fatalf("endpoint failure errors = %v", unavailable.Errors)
	}
	failing.Store(false)

	// Restart, first sweep still pending: the real startup snapshot.
	// 807e0d2 grouped this as mini:/repo with no document.
	pending := poll(projects.New([]string{repoPath}, 0).Snapshot())
	local("first sweep pending", pending, doc, "initial collection pending")
	if len(pending.Errors) != 0 {
		t.Fatalf("pending sweep errors = %v", pending.Errors)
	}

	// Restart, first sweep failed: an error result with no document.
	failed := poll(projects.Snapshot{GeneratedAt: time.Now(), Results: []projects.Result{{Path: repoPath, Error: failedFirstSweep, AttemptedAt: time.Now()}}})
	local("first sweep failed", failed, doc, failedFirstSweep)

	// Recovery: a newer document replaces the retained one.
	newer := *doc
	newer.GeneratedAt = doc.GeneratedAt.Add(time.Hour)
	recovered := poll(projects.Snapshot{GeneratedAt: time.Now(), Results: []projects.Result{{Path: repoPath, Document: &newer}}})
	local("newer sweep", recovered, &newer, "")

	// Out of order: an older document never replaces a newer one.
	older := *doc
	older.GeneratedAt = doc.GeneratedAt.Add(-time.Hour)
	outOfOrder := poll(projects.Snapshot{GeneratedAt: time.Now(), Results: []projects.Result{{Path: repoPath, Document: &older}}})
	local("older sweep", outOfOrder, &newer, "")
}

// Retention is per path. A path the fleet has never held a document for keeps
// the agent's own pending or failed result, and the cached snapshot the retained
// document came from is never mutated.
func TestProjectsRetentionIsPerPath(t *testing.T) {
	doc := projectFixture(t)
	previous := &projects.Snapshot{Results: []projects.Result{{Path: "/repo", Document: doc}, {Path: "/failed", Error: failedFirstSweep}}}
	results := []projects.Result{
		{Path: "/repo", Error: "initial collection pending"},
		{Path: "/new", Error: "initial collection pending"},
		{Path: "/failed", Error: failedFirstSweep},
	}
	keepNewerDocuments(results, previous)
	if results[0].Document != doc || results[0].Error != "initial collection pending" {
		t.Fatalf("seen path = %+v", results[0])
	}
	if results[1].Document != nil || results[2].Document != nil {
		t.Fatalf("never-seen paths gained a document: %+v", results[1:])
	}
	if previous.Results[0].Error != "" || previous.Results[0].Document != doc || previous.Results[1].Document != nil {
		t.Fatalf("cached snapshot mutated: %+v", previous.Results)
	}
	view := projects.Merge([]projects.Machine{{Name: "mini", Online: true, Snapshot: &projects.Snapshot{Results: results}}})
	for _, name := range []string{"mini:/new", "mini:/failed"} {
		repo := findRepository(view, name)
		if repo == nil || repo.GitHub != nil || repo.Locals[0].Error == "" {
			t.Fatalf("never-seen path %s = %+v", name, repo)
		}
	}
	if repo := findRepository(view, doc.Repository.Name); repo == nil || repo.GitHub != doc {
		t.Fatalf("retained path not grouped: %+v", view.Repositories)
	}
	// No cache yet, as on the fleet's own first poll: the reply stands.
	fresh := []projects.Result{{Path: "/repo", Error: "initial collection pending"}}
	keepNewerDocuments(fresh, nil)
	if fresh[0].Document != nil {
		t.Fatalf("document without a cache: %+v", fresh[0])
	}
}

// sameDocument compares decoded documents, which never share a pointer with
// the ones the fake agent encoded.
func sameDocument(got, want *projects.Document) bool {
	return got != nil && want != nil && got.GeneratedAt.Equal(want.GeneratedAt) && got.Repository.Name == want.Repository.Name
}
