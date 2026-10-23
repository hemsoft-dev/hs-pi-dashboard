// Package projects collects gh-x's versioned status contract without duplicating
// its health, review, or check rules.
package projects

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/HemSoft/hs-pi-dashboard/internal/githubcli"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
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

// Project polling shares the user's GitHub quota with every other session.
const DefaultPollInterval = 30 * time.Minute
const MinPollInterval = 15 * time.Minute

type Runner func(context.Context, string) ([]byte, error)
type Store struct {
	paths    []string
	run      Runner
	every    time.Duration
	mu       sync.RWMutex
	snapshot Snapshot
	retryAt  time.Time
	now      func() time.Time
}

func New(paths []string, every time.Duration) *Store {
	for _, path := range paths {
		if strings.TrimSpace(path) == "off" {
			paths = nil
			break
		}
	}
	if every <= 0 {
		every = DefaultPollInterval
	}
	if every < MinPollInterval {
		every = MinPollInterval
	}
	seen := map[string]bool{}
	unique := []string{}
	for _, p := range paths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		p = filepath.Clean(p)
		if !seen[p] {
			unique = append(unique, p)
			seen[p] = true
		}
	}
	s := &Store{paths: unique, run: runStatus, every: every, now: time.Now}
	s.snapshot = Snapshot{Results: []Result{}}
	for _, p := range unique {
		s.snapshot.Results = append(s.snapshot.Results, Result{Path: p, Error: "initial collection pending"})
	}
	return s
}
func (s *Store) Start(ctx context.Context) {
	s.refresh(ctx)
	// Wait a full interval after collection finishes. A slow sweep must not
	// queue another immediate sweep as a ticker would.
	timer := time.NewTimer(s.nextDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.refresh(ctx)
			timer.Reset(s.nextDelay())
		}
	}
}
func (s *Store) Snapshot() Snapshot { s.mu.RLock(); defer s.mu.RUnlock(); return s.snapshot }
func (s *Store) nextDelay() time.Duration {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return max(s.every, s.retryAt.Sub(s.now()))
}

func (s *Store) refresh(ctx context.Context) {
	s.mu.RLock()
	blocked := s.now().Before(s.retryAt)
	s.mu.RUnlock()
	if blocked {
		return
	}
	old := s.Snapshot()
	previous := map[string]*Document{}
	for _, r := range old.Results {
		previous[r.Path] = r.Document
	}
	results := make([]Result, len(s.paths))
	var limited *rateLimitError
	var retryAt time.Time
	// Keep at most one gh-x process running on this agent. Each process can
	// make several GitHub requests; a worker pool multiplies that burst.
	for i, p := range s.paths {
		if ctx.Err() != nil {
			return
		}
		r := Result{Path: p, AttemptedAt: s.now()}
		if limited != nil {
			r.Error = "skipped: GitHub rate limited"
			r.Document = previous[p]
			results[i] = r
			continue
		}
		c, cancel := context.WithTimeout(ctx, 45*time.Second)
		data, err := s.run(c, p)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if errors.As(err, &limited) {
				r.Error = limited.Error()
				retryAt = maxTime(s.now().Add(s.every), limited.until)
			} else {
				r.Error = "gh x status --json failed; check gh-x installation, checkout and host authentication"
			}
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
	s.mu.Lock()
	// Cancellation can also happen with no configured paths or while waiting
	// to publish the completed sweep. Keep the prior snapshot in that case.
	if ctx.Err() == nil {
		s.snapshot = Snapshot{GeneratedAt: s.now(), Results: results}
		s.retryAt = retryAt
	}
	s.mu.Unlock()
}

// A bounded writer prevents runaway subprocess output from exhausting the agent.
type limitedBuffer struct{ buffer bytes.Buffer }

func (b *limitedBuffer) Len() int      { return b.buffer.Len() }
func (b *limitedBuffer) Bytes() []byte { return b.buffer.Bytes() }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8<<20 {
		return 0, errors.New("status output too large")
	}
	return b.buffer.Write(p)
}
func runStatus(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, githubcli.Binary(), "x", "status", "--json")
	cmd.Dir = path
	cmd.WaitDelay = time.Second
	var out limitedBuffer
	cmd.Stdout = &out
	var stderr stderrBuffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if err != nil {
		return out.Bytes(), classifyStatusFailure(stderr.Bytes(), err, time.Now())
	}
	return out.Bytes(), nil
}

// Retain only a bounded diagnostic prefix, but drain all stderr so a verbose
// child cannot block. Raw output never enters dashboard errors.
type stderrBuffer struct{ buffer bytes.Buffer }

func (b *stderrBuffer) Len() int      { return b.buffer.Len() }
func (b *stderrBuffer) Bytes() []byte { return b.buffer.Bytes() }

func (b *stderrBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if left := (32 << 10) - b.Len(); left > 0 {
		_, _ = b.buffer.Write(p[:min(left, len(p))])
	}
	return n, nil
}

type rateLimitError struct{ until time.Time }

func (e *rateLimitError) Error() string {
	if !e.until.IsZero() {
		return "GitHub rate limited; retry after " + e.until.UTC().Format(time.RFC3339)
	}
	return "GitHub rate limited; retry on the next collection interval"
}

var resetHeader = regexp.MustCompile(`(?im)x-ratelimit-reset\s*[:=]\s*(\d+)`)
var retryHeader = regexp.MustCompile(`(?im)retry-after\s*[:=]\s*(\d+)`)

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
func classifyStatusFailure(stderr []byte, original error, now time.Time) error {
	message := strings.ToLower(string(stderr))
	if !strings.Contains(message, "rate limit") && !strings.Contains(message, "rate_limit exceeded") && !strings.Contains(message, "rate_limit_exceeded") &&
		!strings.Contains(message, "abuse detection") && !strings.Contains(message, "http 429") {
		return original
	}
	var until time.Time
	if match := resetHeader.FindSubmatch(stderr); len(match) > 1 {
		if seconds, err := strconv.ParseInt(string(match[1]), 10, 64); err == nil {
			until = time.Unix(seconds, 0)
		}
	}
	if match := retryHeader.FindSubmatch(stderr); len(match) > 1 {
		if seconds, err := strconv.ParseInt(string(match[1]), 10, 64); err == nil && seconds > 0 && seconds <= int64((time.Duration(1<<63-1))/time.Second) {
			until = maxTime(until, now.Add(time.Duration(seconds)*time.Second))
		}
	}
	if !until.After(now) {
		until = time.Time{}
	}
	return &rateLimitError{until: until}
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
