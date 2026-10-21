// Package agent serves a JSON snapshot of this machine's coding sessions so the
// fleet dashboard can aggregate it.
package agent

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/herdr"
	"github.com/HemSoft/hs-pi-dashboard/internal/projects"
	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
	"github.com/HemSoft/hs-pi-dashboard/internal/t3"
	"github.com/HemSoft/hs-pi-dashboard/internal/usage"
)

// Options configures one agent.
type Options struct {
	ProjectPaths []string
	ProjectsPoll time.Duration
	T3Binary     string        // installed T3 CLI for scoped local pairing; empty = discover t3
	T3DBs        []string      // T3 environment databases; nil uses ~/.t3/userdata/state.sqlite, "off" disables
	Addr         string        // listen address, e.g. 100.101.122.39:8787
	Dir          string        // pi sessions dir; empty = ~/.pi/agent/sessions
	ClaudeDir    string        // Claude Code projects dir; empty = ~/.claude/projects
	Machine      string        // label reported in the snapshot
	ActiveWindow time.Duration // how long a quiet session still counts as active
	UsagePoll    time.Duration // provider usage refresh interval (default 1m)
	HerdrBinary  string        // installed Herdr executable; empty = discover it
}

// Run starts the agent HTTP server and blocks until it exits.
func Run(opts Options) error {
	usageStore := usage.NewStore(usage.New(), opts.Machine, usagePollInterval(opts.UsagePoll))
	mux := http.NewServeMux()
	projectStore := projects.New(opts.ProjectPaths, opts.ProjectsPoll)
	go projectStore.Start(context.Background())
	mux.HandleFunc("GET /projects", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, projectStore.Snapshot())
	})
	live := herdr.New(opts.HerdrBinary)
	go live.Start(context.Background())
	mux.HandleFunc("GET /herdr", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, live.Snapshot())
	})

	mux.HandleFunc("GET /sessions", sessionHandler(opts))

	mux.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      true,
			"machine": opts.Machine,
		})
	})

	mux.HandleFunc("GET /usage", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, usageStore.Snapshot())
	})

	// Refresh provider usage in the background; the endpoint serves the cache.
	// A fresh background context (not the request's) — the loop outlives requests.
	go usageStore.Start(context.Background())

	srv := &http.Server{
		Addr:              opts.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("hs-pi-dashboard agent listening on %s (machine=%s dir=%s)", opts.Addr, opts.Machine, opts.Dir)
	return srv.ListenAndServe()
}

func sessionHandler(opts Options) http.HandlerFunc {
	scanner := sessions.NewScanner(opts.Dir, opts.ActiveWindow)
	claudeScanner := sessions.NewClaudeScanner(opts.ClaudeDir, opts.ActiveWindow)
	paths := opts.T3DBs
	if len(paths) == 0 {
		paths = []string{""}
	}
	var collectors []*t3.Collector
	seen := map[string]bool{}
	for _, path := range paths {
		if path != "off" && !seen[path] {
			collectors = append(collectors, t3.New(path, opts.ActiveWindow, opts.T3Binary))
			seen[path] = true
		}
	}
	pollers := []func() sessions.Snapshot{scanner.Poll, claudeScanner.Poll}
	for _, collector := range collectors {
		pollers = append(pollers, collector.Poll)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		snaps := pollSources(pollers)
		snap := sessions.MergeSnapshots(snaps...)
		snap.Machine = opts.Machine
		writeJSON(w, http.StatusOK, snap)
	}
}

// Poll sources together so multiple T3 environments share the same wall-clock
// budget. Each T3 query has a 2.5s limit; the fleet request allows 3s.
func pollSources(pollers []func() sessions.Snapshot) []sessions.Snapshot {
	snapshots := make([]sessions.Snapshot, len(pollers))
	var workers sync.WaitGroup
	for i, poll := range pollers {
		workers.Add(1)
		go func() { defer workers.Done(); snapshots[i] = poll() }()
	}
	workers.Wait()
	return snapshots
}

func usagePollInterval(d time.Duration) time.Duration {
	if d <= 0 {
		return time.Minute
	}
	return d
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	_ = enc.Encode(payload)
}
