// Package fleet aggregates per-machine agent snapshots and serves the
// dashboard UI.
package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/herdr"
	"github.com/HemSoft/hs-pi-dashboard/internal/hermes"
	"github.com/HemSoft/hs-pi-dashboard/internal/projects"
	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
	"github.com/HemSoft/hs-pi-dashboard/internal/usage"
	"github.com/HemSoft/hs-pi-dashboard/internal/web"
)

// Target is one machine's agent.
type Target struct {
	Name        string
	URL         string
	TerminalURL string
}

// MachineState is the aggregated view of one machine.
type MachineState struct {
	Projects       *projects.Snapshot      `json:"projects,omitempty"`
	Sources        []sessions.SourceHealth `json:"sources,omitempty"`
	Name           string                  `json:"name"`
	URL            string                  `json:"url"`
	Online         bool                    `json:"online"`
	Error          string                  `json:"error,omitempty"`
	GeneratedAt    time.Time               `json:"generatedAt"`
	Sessions       []sessions.Summary      `json:"sessions"`
	ActiveSessions int                     `json:"activeSessions"`
	Usage          *usage.Snapshot         `json:"usage,omitempty"`
	Herdr          *herdr.Snapshot         `json:"herdr,omitempty"`
	TerminalURL    string                  `json:"terminalUrl,omitempty"`
}

// FleetSnapshot is the dashboard-facing aggregation of all machines.
type FleetSnapshot struct {
	Projects       projects.View    `json:"projects"`
	GeneratedAt    time.Time        `json:"generatedAt"`
	Machines       []MachineState   `json:"machines"`
	HermesSessions []hermes.Session `json:"hermesSessions"`
	ActiveSessions int              `json:"activeSessions"`
}

// agentTransport keeps an idle connection for each request a poll sends one
// agent at once (/sessions, /herdr and /projects) plus /usage. With
// http.DefaultTransport's two per host, every poll would dial one new
// connection per agent and close another.
var agentTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConnsPerHost = 4
	return t
}()

var agentClient = &http.Client{Transport: agentTransport}

// Server polls agents and serves /api/fleet plus the embedded UI.
type Server struct {
	targets   []Target
	pollEvery time.Duration
	timeout   time.Duration
	usagePoll time.Duration

	mu        sync.Mutex
	cache     map[string]MachineState
	usageMu   sync.Mutex
	usageData map[string]*usage.Snapshot
}

// ParseTargets decodes the -fleet flag: comma-separated name|url pairs.
func ParseTargets(spec string) ([]Target, error) {
	var targets []Target
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		fields := strings.Split(part, "|")
		if len(fields) < 2 || len(fields) > 3 || strings.TrimSpace(fields[0]) == "" || strings.TrimSpace(fields[1]) == "" {
			return nil, fmt.Errorf("invalid fleet entry %q: want name|url[|https-terminal-url]", part)
		}
		target := Target{Name: strings.TrimSpace(fields[0]), URL: strings.TrimSpace(fields[1])}
		if len(fields) == 3 {
			target.TerminalURL = strings.TrimSpace(fields[2])
			u, err := url.Parse(target.TerminalURL)
			if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
				return nil, fmt.Errorf("invalid terminal URL for %q: HTTPS without credentials required", target.Name)
			}
		}
		targets = append(targets, target)
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("empty fleet spec")
	}
	return targets, nil
}

// NewServer builds the aggregator.
func NewServer(targets []Target, pollEvery time.Duration) *Server {
	if pollEvery <= 0 {
		pollEvery = 5 * time.Second
	}
	return &Server{
		targets:   targets,
		pollEvery: pollEvery,
		timeout:   3 * time.Second,
		usagePoll: time.Minute,
		cache:     make(map[string]MachineState, len(targets)),
		usageData: make(map[string]*usage.Snapshot, len(targets)),
	}
}

// Run starts the poll loops and the HTTP server. It blocks until the server
// exits.
func (s *Server) Run(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return s.serve(context.Background(), ln)
}

// serve answers HTTP on ln at once, while the first fleet and usage polls run
// in the background, so a stalled agent cannot delay /healthz or the page.
// Cancelling ctx closes the server and waits for both poll loops to stop.
func (s *Server) serve(ctx context.Context, ln net.Listener) error {
	ctx, cancel := context.WithCancel(ctx)
	var loops sync.WaitGroup
	defer func() {
		cancel()
		loops.Wait()
	}()
	loops.Go(func() { every(ctx, s.pollEvery, s.poll) })
	loops.Go(func() { every(ctx, s.usagePoll, s.pollUsage) })

	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("hs-pi-dashboard serve listening on %s (poll=%s, machines=%d)", ln.Addr(), s.pollEvery, len(s.targets))
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()
	select {
	case err := <-served:
		return err
	case <-ctx.Done():
		_ = srv.Close()
		<-served
		return ctx.Err()
	}
}

// every runs fn at once and then on each tick until ctx ends. A run longer
// than interval drops the ticks it overlaps instead of starting another run,
// so runs of fn never overlap.
func every(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for ctx.Err() == nil {
		fn(ctx)
		select {
		case <-ctx.Done():
		case <-ticker.C:
		}
	}
}

// poll refreshes every target. Each target's /sessions, /herdr and /projects
// requests run at the same time, each under its own s.timeout, so a stalled
// target costs one timeout per poll rather than one per endpoint.
func (s *Server) poll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range s.targets {
		wg.Go(func() {
			var herdrSnap *herdr.Snapshot
			var projectsSnap *projects.Snapshot
			var endpoints sync.WaitGroup
			endpoints.Go(func() { herdrSnap = s.fetchHerdr(ctx, t) })
			endpoints.Go(func() { projectsSnap = s.fetchProjects(ctx, t) })
			state := s.fetch(ctx, t)
			endpoints.Wait()
			state.Herdr = herdrSnap
			state.Projects = projectsSnap
			state.TerminalURL = t.TerminalURL
			s.mu.Lock()
			s.cache[t.Name] = state
			s.mu.Unlock()
		})
	}
	wg.Wait()
}

func (s *Server) fetch(ctx context.Context, t Target) MachineState {
	state := MachineState{Name: t.Name, URL: t.URL}

	reqCtx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimRight(t.URL, "/")+"/sessions", nil)
	if err != nil {
		state.Error = err.Error()
		return s.mergeStale(state)
	}

	resp, err := agentClient.Do(req)
	if err != nil {
		state.Error = err.Error()
		return s.mergeStale(state)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		state.Error = fmt.Sprintf("agent returned %s", resp.Status)
		return s.mergeStale(state)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		state.Error = err.Error()
		return s.mergeStale(state)
	}

	var snap sessions.Snapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		state.Error = err.Error()
		return s.mergeStale(state)
	}
	if collectionPending(snap) {
		return s.mergePending(state, snap)
	}

	state.Online = true
	state.GeneratedAt = snap.GeneratedAt
	state.Sessions = snap.Sessions
	// Legacy agents omit Source. Their non-Pi IDs are namespaced, and
	// sessionsDir identifies the Pi collector root even in mixed snapshots.
	// Preserve explicit sources and leave unknown namespaces unclassified.
	for i := range state.Sessions {
		row := &state.Sessions[i]
		if row.Source != "" {
			continue
		}
		switch {
		case strings.HasPrefix(row.ID, "t3:"):
			row.Source = "t3"
		case strings.HasPrefix(row.ID, "claude-code:"):
			row.Source = "claude-code"
		case snap.SessionsDir != "" && row.ID != "" && !strings.Contains(row.ID, ":"):
			row.Source = "pi"
		}
	}
	state.Sources = snap.Sources
	state.ActiveSessions = snap.ActiveSessions
	// Agents deployed before activeSessions was added still report enough data
	// for an accurate count up to their 20-row display cap.
	if state.ActiveSessions == 0 {
		for _, session := range state.Sessions {
			if session.Active {
				state.ActiveSessions++
			}
		}
	}
	return state
}

// mergeStale keeps the last good session list so a rebooting machine shows
// its history grayed out instead of vanishing.
func (s *Server) mergeStale(state MachineState) MachineState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if prev, ok := s.cache[state.Name]; ok {
		state.Sessions = prev.Sessions
		state.Sources = prev.Sources
		state.GeneratedAt = prev.GeneratedAt
	}
	return state
}

// collectionPending reports whether snap is the placeholder an agent serves
// before its first session collection completes. A completed collection
// always names the Pi collector root in sessionsDir, even when it found no
// sessions, so a reply with neither sessionsDir nor a sessions array has not
// collected anything yet.
func collectionPending(snap sessions.Snapshot) bool {
	return snap.SessionsDir == "" && snap.Sessions == nil
}

// sessionCollectionPending is the machine error while an agent's first
// session collection runs.
const sessionCollectionPending = "session collection pending after agent start"

// mergePending keeps a reachable agent online while its first collection runs,
// but never as a healthy machine with no sessions. The reply's own source
// health stays visible, the error names the pending collection, and the last
// good rows remain as inactive history so nothing counts them as live until a
// real collection reports them again.
func (s *Server) mergePending(state MachineState, snap sessions.Snapshot) MachineState {
	state.Online = true
	state.Error = sessionCollectionPending
	state.GeneratedAt = snap.GeneratedAt
	state.Sources = snap.Sources
	s.mu.Lock()
	prev := s.cache[state.Name]
	s.mu.Unlock()
	if len(prev.Sessions) > 0 {
		state.Error += "; showing last known sessions"
		state.GeneratedAt = prev.GeneratedAt
		state.Sessions = retainedSessions(prev.Sessions)
	}
	return state
}

// retainedSessions copies cached rows as inactive history. The copy keeps the
// cached MachineState unchanged for concurrent readers. T3 rows lose their
// sidebar badge, as the agent does for an expired T3 source.
func retainedSessions(rows []sessions.Summary) []sessions.Summary {
	out := append([]sessions.Summary(nil), rows...)
	for i := range out {
		row := &out[i]
		row.Active = false
		if row.Source == "t3" {
			row.Status = "unavailable"
			row.T3Status = nil
		}
	}
	return out
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet", s.handleFleet)
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	mux.HandleFunc("GET /assets/billing.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(web.BillingJS)
	})
	mux.HandleFunc("GET /assets/billing.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(web.BillingCSS)
	})
	mux.HandleFunc("GET /assets/projects.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(web.ProjectsJS)
	})
	mux.HandleFunc("GET /assets/projects.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(web.ProjectsCSS)
	})
	mux.HandleFunc("GET /assets/herdr.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(web.HerdrJS)
	})
	mux.HandleFunc("GET /assets/herdr.css", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(web.HerdrCSS)
	})
	mux.HandleFunc("GET /assets/smoothie-1.36.1.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		_, _ = w.Write(web.SmoothieJS)
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(web.Index)
	})
	// Hermes sessions come from ~/.hermes/state.db on the same machine the
	// server runs on; an absent DB just renders nothing.
	mux.HandleFunc("GET /api/hermes-sessions", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, hermes.Default().Snapshot(20))
	})
	return mux
}

func (s *Server) handleFleet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.cachedFleet())
}

// cachedFleet snapshots the current aggregation under lock.
func (s *Server) cachedFleet() FleetSnapshot {
	s.usageMu.Lock()
	usageCopy := make(map[string]*usage.Snapshot, len(s.usageData))
	for name, snap := range s.usageData {
		usageCopy[name] = snap
	}
	s.usageMu.Unlock()

	s.mu.Lock()
	defer s.mu.Unlock()
	machines := make([]MachineState, 0, len(s.targets))
	for _, t := range s.targets {
		state, ok := s.cache[t.Name]
		if !ok {
			state = MachineState{Name: t.Name, URL: t.URL, Error: "no poll yet"}
		}
		if snap, ok := usageCopy[t.Name]; ok {
			state.Usage = snap
		}
		machines = append(machines, state)
	}
	hermesSnapshot := hermes.Default().Snapshot(20)
	projectMachines := make([]projects.Machine, 0, len(machines))
	for _, m := range machines {
		projectMachines = append(projectMachines, projects.Machine{Name: m.Name, Online: m.Online, Snapshot: m.Projects})
	}
	return FleetSnapshot{
		Projects:       projects.Merge(projectMachines),
		GeneratedAt:    time.Now(),
		Machines:       machines,
		HermesSessions: hermesSnapshot.Sessions,
		ActiveSessions: activeSessionCount(machines, hermesSnapshot.ActiveSessions),
	}
}

// activeSessionCount returns live work only. An offline machine may retain its
// last good session list, but those stale sessions must not reach the count.
func activeSessionCount(machines []MachineState, activeHermesSessions int) int {
	count := activeHermesSessions
	for _, machine := range machines {
		if !machine.Online {
			continue
		}
		count += machine.ActiveSessions
	}
	return count
}

// pollUsage fetches every machine's /usage snapshot; failures keep the last
// good data (usage ages slowly, so gray beats blank).
func (s *Server) pollUsage(ctx context.Context) {
	reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	for _, t := range s.targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			snap := s.fetchUsage(reqCtx, t)
			if snap == nil {
				return
			}
			s.usageMu.Lock()
			s.usageData[t.Name] = snap
			s.usageMu.Unlock()
		}(t)
	}
	wg.Wait()
}

func (s *Server) fetchUsage(ctx context.Context, t Target) *usage.Snapshot {
	reqCtx, cancel := context.WithTimeout(ctx, s.timeout+5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet,
		strings.TrimRight(t.URL, "/")+"/usage", nil)
	if err != nil {
		return nil
	}
	resp, err := agentClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil
	}
	var snap usage.Snapshot
	if json.Unmarshal(body, &snap) != nil {
		return nil
	}
	// Drop "unknown" cards (the old agents' xAI placeholder): they carry no
	// data, and agents keep sending them until they are redeployed.
	kept := snap.Cards[:0]
	for _, c := range snap.Cards {
		if c.Kind != usage.KindUnknown {
			kept = append(kept, c)
		}
	}
	snap.Cards = kept
	if len(snap.Cards) == 0 {
		// The agent has not finished its first refresh; keep the previous
		// snapshot instead of caching emptiness.
		return nil
	}
	return &snap
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(payload)
}

// fetchProjects keeps the last good document for each checkout. An endpoint
// failure keeps the whole cached snapshot with an explicit error. A reply keeps
// each path's cached document when it has none, as a restarted agent's pending
// or failed first sweep does, or only an older one; see keepNewerDocuments.
func (s *Server) fetchProjects(ctx context.Context, t Target) *projects.Snapshot {
	s.mu.Lock()
	previous := s.cache[t.Name].Projects
	s.mu.Unlock()
	c, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, strings.TrimRight(t.URL, "/")+"/projects", nil)
	message := "projects endpoint unavailable; update agent or check machine connection"
	if err == nil {
		resp, e := agentClient.Do(req)
		if e == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var snap projects.Snapshot
				if json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&snap) == nil && snap.Results != nil {
					keepNewerDocuments(snap.Results, previous)
					return &snap
				}
			}
		}
	}
	snap := projects.Snapshot{Error: message, Results: []projects.Result{}}
	if previous != nil {
		snap = *previous
		snap.Error = message
	}
	return &snap
}

// keepNewerDocuments gives each freshly decoded result the cached document for
// its path when the reply has no document or an older one. The result keeps
// its own error, so a pending or failed sweep shows beside the retained data.
// Paths without a cached document keep the reply unchanged. Documents are
// shared read-only between cache entries; results is the caller's own slice.
func keepNewerDocuments(results []projects.Result, previous *projects.Snapshot) {
	if previous == nil {
		return
	}
	cached := make(map[string]*projects.Document, len(previous.Results))
	for _, r := range previous.Results {
		if r.Document != nil {
			cached[r.Path] = r.Document
		}
	}
	for i := range results {
		doc := cached[results[i].Path]
		if doc != nil && (results[i].Document == nil || results[i].Document.GeneratedAt.Before(doc.GeneratedAt)) {
			results[i].Document = doc
		}
	}
}
