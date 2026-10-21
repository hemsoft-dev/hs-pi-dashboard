// Package projects collects gh-x's versioned status contract without duplicating
// its health, review, or check rules.
package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/HemSoft/hs-pi-dashboard/internal/githubcli"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Document preserves producer-owned verdicts and table fields, including unknown values.
type Document struct {
	SchemaVersion int       `json:"schemaVersion"`
	GeneratedAt   time.Time `json:"generatedAt"`
	Repository    struct {
		Name          string `json:"nameWithOwner"`
		Root          string `json:"rootPath"`
		DefaultBranch string `json:"defaultBranch"`
	} `json:"repository"`
	Health       map[string]json.RawMessage `json:"health"`
	Issues       []map[string]any           `json:"issues"`
	PullRequests struct {
		Open        []map[string]any `json:"open"`
		Merged      []map[string]any `json:"merged"`
		MergedLimit int              `json:"mergedLimit"`
	} `json:"pullRequests"`
	WorkflowRuns        []map[string]any `json:"workflowRuns"`
	WorkflowRunsPerfect bool             `json:"workflowRunsPerfect"`
	Sections            []Section        `json:"sections"`
}

type Section struct {
	Name            string     `json:"name"`
	Status          string     `json:"status"`
	Reason          string     `json:"reason"`
	Cached          bool       `json:"cached"`
	FetchedAt       *time.Time `json:"fetchedAt"`
	CacheAgeSeconds float64    `json:"cacheAgeSeconds"`
}

func Parse(data []byte) (*Document, error) {
	var d Document
	if json.Unmarshal(data, &d) != nil || d.SchemaVersion != 1 || d.Repository.Root == "" || d.GeneratedAt.IsZero() || len(d.Health) != 5 || len(d.Sections) != 4 {
		return nil, errors.New("unsupported or invalid gh-x status document; schemaVersion 1 required")
	}
	for _, key := range []string{"defaultBranch", "currentBranch", "branches", "worktrees", "stashes"} {
		var row struct {
			Status string `json:"status"`
			Clean  *bool  `json:"clean"`
		}
		if json.Unmarshal(d.Health[key], &row) != nil || row.Clean == nil || (row.Status != "available" && row.Status != "unavailable") {
			return nil, errors.New("invalid gh-x health row")
		}
	}
	for i, key := range []string{"issues", "pullRequests.open", "pullRequests.merged", "workflowRuns"} {
		s := d.Sections[i]
		if s.Name != key || (s.Status != "available" && s.Status != "unavailable" && s.Status != "disabled") {
			return nil, errors.New("invalid gh-x section metadata")
		}
	}
	return &d, nil
}

type Result struct {
	Path        string    `json:"path"`
	Error       string    `json:"error,omitempty"`
	AttemptedAt time.Time `json:"attemptedAt"`
	Document    *Document `json:"document,omitempty"`
}
type Snapshot struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Error       string    `json:"error,omitempty"`
	Results     []Result  `json:"results"`
}

// DefaultPaths discovers primary checkouts in ~/github/<repo> or ~/github/<owner>/<repo>.
// It never recursively walks source/dependency trees or includes linked worktrees.
func DefaultPaths() []string {
	home, _ := os.UserHomeDir()
	var paths []string
	for _, pattern := range []string{filepath.Join(home, "github", "*", ".git"), filepath.Join(home, "github", "*", "*", ".git")} {
		matches, _ := filepath.Glob(pattern)
		for _, match := range matches {
			if st, err := os.Stat(match); err == nil && st.IsDir() {
				paths = append(paths, filepath.Dir(match))
			}
		}
	}
	if len(paths) == 0 {
		if cwd, err := os.Getwd(); err == nil {
			paths = append(paths, cwd)
		}
	}
	sort.Strings(paths)
	return paths
}

type Runner func(context.Context, string) ([]byte, error)
type Store struct {
	paths    []string
	run      Runner
	every    time.Duration
	mu       sync.RWMutex
	snapshot Snapshot
}

func New(paths []string, every time.Duration) *Store {
	if len(paths) == 0 {
		paths = DefaultPaths()
	}
	for _, path := range paths {
		if path == "off" {
			paths = nil
			break
		}
	}
	if every <= 0 {
		every = 2 * time.Minute
	}
	seen := map[string]bool{}
	unique := []string{}
	for _, p := range paths {
		p = filepath.Clean(p)
		if !seen[p] {
			unique = append(unique, p)
			seen[p] = true
		}
	}
	s := &Store{paths: unique, run: runStatus, every: every}
	s.snapshot = Snapshot{Results: []Result{}}
	for _, p := range unique {
		s.snapshot.Results = append(s.snapshot.Results, Result{Path: p, Error: "initial collection pending"})
	}
	return s
}
func (s *Store) Start(ctx context.Context) {
	s.refresh(ctx)
	ticker := time.NewTicker(s.every)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.refresh(ctx)
		}
	}
}
func (s *Store) Snapshot() Snapshot { s.mu.RLock(); defer s.mu.RUnlock(); return s.snapshot }
func (s *Store) refresh(ctx context.Context) {
	old := s.Snapshot()
	previous := map[string]*Document{}
	for _, r := range old.Results {
		previous[r.Path] = r.Document
	}
	results := make([]Result, len(s.paths))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for n := 0; n < min(3, len(s.paths)); n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				p := s.paths[i]
				r := Result{Path: p, AttemptedAt: time.Now()}
				c, cancel := context.WithTimeout(ctx, 45*time.Second)
				data, err := s.run(c, p)
				cancel()
				if err != nil {
					r.Error = "gh x status --json failed; check gh-x installation, checkout and host authentication"
				} else {
					r.Document, err = Parse(data)
					if err != nil {
						r.Error = err.Error()
					}
				}
				if r.Error != "" {
					r.Document = previous[p]
				}
				results[i] = r
			}
		}()
	}
	for i := range s.paths {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	s.mu.Lock()
	s.snapshot = Snapshot{GeneratedAt: time.Now(), Results: results}
	s.mu.Unlock()
}

// A bounded writer prevents runaway subprocess output from exhausting the agent.
type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8<<20 {
		return 0, errors.New("status output too large")
	}
	return b.Buffer.Write(p)
}
func runStatus(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, githubcli.Binary(), "x", "status", "--json")
	cmd.Dir = path
	cmd.WaitDelay = time.Second
	var out limitedBuffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	return out.Bytes(), err
}

type Local struct {
	Machine string `json:"machine"`
	Online  bool   `json:"online"`
	Result
}
type Repository struct {
	Name   string    `json:"name"`
	Locals []Local   `json:"locals"`
	GitHub *Document `json:"github,omitempty"`
}
type Machine struct {
	Name     string
	Online   bool
	Snapshot *Snapshot
}
type View struct {
	Repositories []Repository `json:"repositories"`
	Errors       []string     `json:"errors"`
}

// Merge selects one newest document per hosted repository, preserving every local
// checkout. Failed refreshes retain old documents with an explicit local error.
func Merge(machines []Machine) View {
	view := View{Repositories: []Repository{}, Errors: []string{}}
	byName := map[string]*Repository{}
	for _, m := range machines {
		if m.Snapshot == nil {
			view.Errors = append(view.Errors, m.Name+": projects not reporting")
			continue
		}
		if !m.Online {
			view.Errors = append(view.Errors, m.Name+": machine offline; cached projects")
		}
		if m.Snapshot.Error != "" {
			view.Errors = append(view.Errors, m.Name+": "+m.Snapshot.Error)
		}
		for _, r := range m.Snapshot.Results {
			name := m.Name + ":" + r.Path
			if r.Document != nil && r.Document.Repository.Name != "" {
				name = r.Document.Repository.Name
			}
			repo := byName[name]
			if repo == nil {
				repo = &Repository{Name: name}
				byName[name] = repo
			}
			repo.Locals = append(repo.Locals, Local{Machine: m.Name, Online: m.Online, Result: r})
			if r.Document != nil && (repo.GitHub == nil || r.Document.GeneratedAt.After(repo.GitHub.GeneratedAt)) {
				repo.GitHub = r.Document
			}
		}
	}
	for _, repo := range byName {
		view.Repositories = append(view.Repositories, *repo)
	}
	sort.Slice(view.Repositories, func(i, j int) bool { return view.Repositories[i].Name < view.Repositories[j].Name })
	return view
}
