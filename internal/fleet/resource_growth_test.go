package fleet

// Steady-state resource check for the fleet server's poll loops (issue #91).
// docs/resource-growth.md explains the method; the bounds are reasoned below.
//
// One cycle is one pass of each loop Server.Run starts: poll (every 5 s in
// production: /sessions, /herdr and /projects from every agent) and pollUsage
// (every minute: /usage). The agents are local fakes:
//
//   - alpha and beta answer every endpoint, with a changing session list;
//   - legacy is an older agent: sessions without a source field and 404 for
//     /herdr and /projects, so the server closes those bodies unread;
//   - down answers 503 everywhere, so the server keeps its last good state.
//
// The /api/fleet handler is not driven: it also reads Hermes sessions through
// hermes.Default(), which binds to the home directory the first caller saw
// and so could read the host's real ~/.hermes database.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/herdr"
	"github.com/HemSoft/hs-pi-dashboard/internal/projects"
	"github.com/HemSoft/hs-pi-dashboard/internal/resourcecheck"
	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
	"github.com/HemSoft/hs-pi-dashboard/internal/usage"
)

// Bounds on growth over the warm-up baseline, after a forced GC and Settle.
// docs/resource-growth.md records the measured growth on Windows and Linux.
//
//   - Goroutines 8: the server polls each agent's endpoints in sequence, so
//     http.DefaultTransport keeps one idle connection per agent (3 goroutines
//     each: client read and write loops, fake agent conn). Answers the server
//     closes unread end their connection, which then winds down. Up to two
//     connections the lowest baseline sample missed is 6, plus 2 for runtime
//     and testing timers. One leaked goroutine per cycle is 300.
//   - OS threads GOMAXPROCS+4: the runtime adds a thread when every idle one
//     is busy and never releases it, so the count creeps up under bursts
//     (Home's Windows runs grew 0 to 4). It cannot pass GOMAXPROCS threads
//     running Go code plus those blocked in system calls, at most one per
//     agent polled at once. A thread can only leak by stranding its
//     goroutine, which the goroutine bound already catches.
//   - Live heap 1 MiB: the server keeps one MachineState and one usage
//     snapshot per agent, fixed-size for fixed fakes. A 4 KiB retention per
//     cycle is 1.2 MiB over 300 cycles.
//   - Handles 8, plus resourcecheck.HandlesPerThread per new OS thread: the
//     same two connections are 4 descriptors on Linux (both ends are in this
//     process). One leaked connection per cycle is 600.
//   - Children 0: the server starts no processes.
var fleetGrowthBounds = resourcecheck.Bounds{Goroutines: 8, Threads: runtime.GOMAXPROCS(0) + 4, HeapBytes: 1 << 20, Handles: 8, Children: 0}

// growthCycles is 1000 by default (a fleet cycle takes about 2 ms) and can be
// raised for a longer local or scheduled run; it never lowers below that.
func growthCycles(t *testing.T) int {
	cycles := 1000
	if raw := os.Getenv("HS_PI_DASHBOARD_GROWTH_CYCLES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < cycles {
			t.Fatalf("HS_PI_DASHBOARD_GROWTH_CYCLES=%q: want an integer of at least %d", raw, cycles)
		}
		cycles = n
	}
	return cycles
}

func TestResourceGrowthFleetPolls(t *testing.T) {
	h := newFleetGrowthHarness(t)
	// A fleet cycle takes about 2 ms, so warm-up blocks are 300 cycles: long
	// enough that a block adding no thread or handle means the pools are full.
	plan := resourcecheck.Plan{Warmup: 300, MaxWarmup: 3000, Cycles: growthCycles(t), Bounds: fleetGrowthBounds, SettleTimeout: 3 * time.Second}
	result, err := plan.Run(func(bool) error { return h.cycle() })
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("platform %s/%s, %d warm-up + %d measured cycles in %s", runtime.GOOS, runtime.GOARCH, result.WarmupCycles, plan.Cycles, result.Elapsed.Round(time.Millisecond))
	t.Logf("baseline: %s", result.Baseline)
	t.Logf("final:    %s", result.Final)
	for _, v := range result.Violations {
		t.Errorf("not steady: %s", v)
	}
	h.requireExercised(t, result.WarmupCycles+plan.Cycles)
}

// The check must fail when a poll retains something. Each control adds one
// deliberate leak to every measured cycle of the same harness, under the same
// bounds, and expects that resource to be reported.
func TestResourceGrowthFleetDetectsRetainedResources(t *testing.T) {
	controls := []struct {
		name  string
		leaks []string
		leak  func(h *fleetGrowthHarness) (func() error, func())
	}{
		{"unclosed agent response bodies", []string{"goroutines", resourcecheck.HandleName}, func(h *fleetGrowthHarness) (func() error, func()) {
			// A fetch that reads an agent and never closes the body.
			var bodies []io.Closer
			return func() error {
					resp, err := http.DefaultClient.Get(h.agents["alpha"].URL + "/sessions")
					if err != nil {
						return err
					}
					bodies = append(bodies, resp.Body)
					return nil
				}, func() {
					for _, body := range bodies {
						body.Close()
					}
				}
		}},
		{"leaked goroutine", []string{"goroutines"}, func(*fleetGrowthHarness) (func() error, func()) {
			stop := make(chan struct{})
			return func() error { go func() { <-stop }(); return nil }, func() { close(stop) }
		}},
		{"retained heap", []string{"live heap bytes"}, func(*fleetGrowthHarness) (func() error, func()) {
			var kept [][]byte
			return func() error { kept = append(kept, make([]byte, 256<<10)); return nil }, func() { kept = nil }
		}},
	}
	for _, c := range controls {
		t.Run(c.name, func(t *testing.T) {
			h := newFleetGrowthHarness(t)
			leak, release := c.leak(h)
			defer release()
			plan := resourcecheck.Plan{Warmup: 3, Cycles: 12, Bounds: fleetGrowthBounds, SettleTimeout: time.Second}
			result, err := plan.Run(func(measured bool) error {
				if err := h.cycle(); err != nil || !measured {
					return err
				}
				return leak() // only in measured cycles, after the baseline
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("baseline: %s", result.Baseline)
			t.Logf("final:    %s", result.Final)
			reported := map[string]bool{}
			for _, v := range result.Violations {
				t.Logf("reported: %s", v)
				reported[v.Resource] = true
			}
			for _, resource := range c.leaks {
				if resource == resourcecheck.HandleName && result.Baseline.Handles == resourcecheck.Unmeasured {
					continue
				}
				if !reported[resource] {
					t.Errorf("%s leak not reported as %s growth", c.name, resource)
				}
			}
		})
	}
}

type fleetGrowthHarness struct {
	server *Server
	agents map[string]*httptest.Server
	hits   map[string]*atomic.Int64 // agent name + path
}

func newFleetGrowthHarness(t *testing.T) *fleetGrowthHarness {
	t.Helper()
	h := &fleetGrowthHarness{agents: map[string]*httptest.Server{}, hits: map[string]*atomic.Int64{}}
	var targets []Target
	for _, name := range []string{"alpha", "beta", "legacy", "down"} {
		for _, path := range []string{"/sessions", "/herdr", "/projects", "/usage"} {
			h.hits[name+path] = &atomic.Int64{}
		}
		agent := httptest.NewServer(h.fakeAgent(t, name))
		t.Cleanup(agent.Close)
		h.agents[name] = agent
		targets = append(targets, Target{Name: name, URL: agent.URL})
	}
	// The fleet server uses http.DefaultClient; drop this test's idle
	// connections before the next test measures.
	t.Cleanup(http.DefaultTransport.(*http.Transport).CloseIdleConnections)
	h.server = NewServer(targets, time.Hour)
	return h
}

func (h *fleetGrowthHarness) fakeAgent(t *testing.T, name string) http.Handler {
	var polls atomic.Int64
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits, ok := h.hits[name+r.URL.Path]
		if !ok {
			t.Errorf("unexpected request %s to %s", r.URL.Path, name)
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		if name == "down" {
			http.Error(w, "synthetic outage", http.StatusServiceUnavailable)
			return
		}
		var payload any
		switch r.URL.Path {
		case "/sessions":
			// The session list changes every poll, as live sessions do.
			n := polls.Add(1)
			source := "pi"
			if name == "legacy" {
				source = ""
			}
			rows := make([]sessions.Summary, 0, 3)
			for i := int64(0); i < 1+n%3; i++ {
				rows = append(rows, sessions.Summary{ID: fmt.Sprintf("%s-%d", name, n+i), Source: source, Active: i == 0})
			}
			payload = sessions.Snapshot{Machine: name, GeneratedAt: time.Now(), SessionsDir: "/fake/sessions", Sessions: rows}
		case "/herdr", "/projects":
			if name == "legacy" {
				http.NotFound(w, r)
				return
			}
			if r.URL.Path == "/herdr" {
				payload = herdr.Snapshot{GeneratedAt: time.Now(), Available: true, Sessions: []herdr.Session{{Name: name, PaneCount: 1}}}
			} else {
				payload = projects.Snapshot{GeneratedAt: time.Now(), Results: []projects.Result{{Path: "/fake/" + name, AttemptedAt: time.Now()}}}
			}
		case "/usage":
			used := float64(polls.Load() % 100)
			payload = usage.Snapshot{Machine: name, GeneratedAt: time.Now(), Cards: []usage.Card{
				{Key: "codex", Label: "Codex", Kind: usage.KindQuota, FetchedAt: time.Now(), Gauges: []usage.Gauge{{Label: "Weekly", UsedPercent: &used}}},
				{Key: "xai", Label: "xAI", Kind: usage.KindUnknown},
			}}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(payload)
	})
}

func (h *fleetGrowthHarness) cycle() error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h.server.poll(ctx)
	h.server.pollUsage(ctx)
	return nil
}

// requireExercised proves every agent endpoint answered every cycle and the
// server's state reflects each kind of agent.
func (h *fleetGrowthHarness) requireExercised(t *testing.T, cycles int) {
	t.Helper()
	for key, hits := range h.hits {
		if hits.Load() < int64(cycles) {
			t.Errorf("%s answered %d of %d cycles", key, hits.Load(), cycles)
		}
	}
	h.server.mu.Lock()
	states := map[string]MachineState{}
	for name, state := range h.server.cache {
		states[name] = state
	}
	h.server.mu.Unlock()
	h.server.usageMu.Lock()
	usageMachines := len(h.server.usageData)
	h.server.usageMu.Unlock()
	if !states["alpha"].Online || states["alpha"].Herdr == nil || states["alpha"].Herdr.Error != "" || len(states["alpha"].Sessions) == 0 {
		t.Errorf("alpha=%+v", states["alpha"])
	}
	if legacy := states["legacy"]; !legacy.Online || legacy.Herdr == nil || legacy.Herdr.Error == "" || legacy.Sessions[0].Source != "pi" {
		t.Errorf("legacy=%+v", legacy)
	}
	if down := states["down"]; down.Online || down.Error == "" {
		t.Errorf("down=%+v", down)
	}
	if usageMachines != 3 {
		t.Errorf("usage cached for %d agents, want the 3 that answer", usageMachines)
	}
}
