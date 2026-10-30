// Package fleet aggregates per-machine agent snapshots and serves the
// dashboard UI.
package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
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

// Run starts the poll loop and the HTTP server. It blocks until the server
// exits.
func (s *Server) Run(addr string) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ticker := time.NewTicker(s.pollEvery)
	defer ticker.Stop()
	usageTicker := time.NewTicker(s.usagePoll)
	defer usageTicker.Stop()
	s.poll(ctx) // first pass immediately
	go s.pollUsage(ctx)
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.poll(ctx)
			}
		}
	}()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-usageTicker.C:
				s.pollUsage(ctx)
			}
		}
	}()

	srv := &http.Server{Addr: addr, Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}
	log.Printf("hs-pi-dashboard serve listening on %s (poll=%s, machines=%d)", addr, s.pollEvery, len(s.targets))
	return srv.ListenAndServe()
}

func (s *Server) poll(ctx context.Context) {
	var wg sync.WaitGroup
	for _, t := range s.targets {
		wg.Add(1)
		go func(t Target) {
			defer wg.Done()
			state := s.fetch(ctx, t)
			state.Herdr = s.fetchHerdr(ctx, t)
			state.Projects = s.fetchProjects(ctx, t)
			state.TerminalURL = t.TerminalURL
			s.mu.Lock()
			s.cache[t.Name] = state
			s.mu.Unlock()
		}(t)
	}
	wg.Wait()
}

func (s *Server) fetch(ctx context.Context, t Target) MachineState {
	state := MachineState{Name: t.Name, URL: t.URL}

	// Session collection includes cold native T3 CLI pairing on Windows.
	reqCtx, cancel := context.WithTimeout(ctx, max(s.timeout, 35*time.Second))
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, strings.TrimRight(t.URL, "/")+"/sessions", nil)
	if err != nil {
		state.Error = err.Error()
		return s.mergeStale(state)
	}

	resp, err := http.DefaultClient.Do(req)
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
	resp, err := http.DefaultClient.Do(req)
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

func (s *Server) fetchProjects(ctx context.Context, t Target) *projects.Snapshot {
	c, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(c, http.MethodGet, strings.TrimRight(t.URL, "/")+"/projects", nil)
	message := "projects endpoint unavailable; update agent or check machine connection"
	if err == nil {
		resp, e := http.DefaultClient.Do(req)
		if e == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var snap projects.Snapshot
				if json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&snap) == nil && snap.Results != nil {
					return &snap
				}
			}
		}
	}
	s.mu.Lock()
	previous := s.cache[t.Name].Projects
	s.mu.Unlock()
	snap := projects.Snapshot{Error: message, Results: []projects.Result{}}
	if previous != nil {
		snap = *previous
		snap.Error = message
	}
	return &snap
}
