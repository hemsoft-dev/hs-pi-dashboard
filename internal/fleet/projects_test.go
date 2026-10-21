package fleet

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/HemSoft/hs-pi-dashboard/internal/projects"
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
