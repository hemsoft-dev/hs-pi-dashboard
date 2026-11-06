package agent

// Steady-state resource check for the agent's collection cycle (issue #91).
// docs/resource-growth.md explains the method; the bounds are reasoned below.
//
// One cycle is what the agent does every minute for usage and every 5 seconds
// for sessions, against fakes only:
//
//   - a usage refresh through usage.Store: every provider fetcher runs
//     concurrently, Codex, Claude and Moonshot over HTTP to a local fake
//     provider, Antigravity by starting a fake agy CLI (this test binary), and
//     the Store keeps or retains each card;
//   - a session refresh through sessionCache: the Pi and Claude Code scanners
//     read fake session folders and the T3 collector opens a fixture T3 V2
//     SQLite database read-only and closes it;
//   - GET /usage and GET /sessions over a real HTTP listener, as the fleet
//     server polls them.
//
// HOME and the provider settings point at temporary fakes, PATH is empty and
// the Credential Manager reader is swapped out by TestMain, so no cycle can
// reach a real provider, vault, CLI or personal file.

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/backgroundcmd"
	"github.com/HemSoft/hs-pi-dashboard/internal/resourcecheck"
	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
	"github.com/HemSoft/hs-pi-dashboard/internal/t3"
	"github.com/HemSoft/hs-pi-dashboard/internal/usage"
)

// growthFakeCLI makes this test binary act as a fake CLI when a cycle starts
// it as one. It runs before testing parses flags, so agy's arguments never
// reach the test framework.
const growthFakeCLI = "HS_PI_DASHBOARD_GROWTH_FAKE_CLI"

func init() {
	switch os.Getenv(growthFakeCLI) {
	case "agy":
		reset := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
		fmt.Print(`{"num_turns":0,"command":{"name":"usage","data":{"groups":[{"buckets":[` +
			`{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.8,"resetTime":"` + reset + `"}]}]}}}`)
		os.Exit(0)
	case "hang":
		// The retained-child control: live until the parent closes stdin.
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
}

// Bounds on growth over the warm-up baseline, after a forced GC and Settle.
// docs/resource-growth.md records the measured growth on Windows and Linux.
//
//   - Goroutines 8: in steady state the only goroutines a cycle leaves are
//     keep-alive connections. The provider transport keeps at most 2 idle
//     connections and the /usage-/sessions client 1; each costs 3 goroutines
//     in this process (client read and write loops, server conn). The
//     baseline keeps each resource's lowest sample, so up to two connections
//     the baseline sampled closed may be idle again at the end: 6, plus 2 for
//     runtime and testing timers. One leaked goroutine per cycle is 300.
//   - OS threads GOMAXPROCS+12: the runtime adds a thread when every idle one
//     is busy, and never releases it, so the count creeps up under bursts
//     (Home's Windows run went 18 to 21 by cycle 130, then flat to 1230). It
//     cannot pass GOMAXPROCS threads running Go code plus those blocked in
//     system calls, and a cycle blocks at most 12 goroutines at once (eight
//     provider fetchers, agy's wait, three session pollers). A thread can
//     only leak by stranding its goroutine (blocked forever, or locked and
//     never returning), which the goroutine bound already catches.
//   - Live heap 1 MiB: the agent retains one usage snapshot, one session
//     snapshot and the T3 collector's per-thread activity, all fixed-size for
//     fixed fakes; the runtime's goroutine descriptors grow with peak
//     concurrency, not cycles. A 4 KiB retention per cycle is 1.2 MiB.
//   - Handles 8, plus resourcecheck.HandlesPerThread per new OS thread: Linux
//     counts 2 descriptors per connection (both ends are in this process), so
//     the same two connections are 4. Windows counts socket, thread, event
//     and process handles; a thread's own are credited, and thread growth
//     has its own bound above. One leaked connection per cycle is 600.
//   - Children 0: agy is waited for inside the cycle, so none may outlive it.
var agentGrowthBounds = resourcecheck.Bounds{Goroutines: 8, Threads: runtime.GOMAXPROCS(0) + 12, HeapBytes: 1 << 20, Handles: 8, Children: 0}

// growthCycles is 300 by default and can be raised for a longer local or
// scheduled run; it never lowers below 300.
func growthCycles(t *testing.T) int {
	cycles := 300
	if raw := os.Getenv("HS_PI_DASHBOARD_GROWTH_CYCLES"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < cycles {
			t.Fatalf("HS_PI_DASHBOARD_GROWTH_CYCLES=%q: want an integer of at least %d", raw, cycles)
		}
		cycles = n
	}
	return cycles
}

func TestResourceGrowthAgentCollectionCycles(t *testing.T) {
	h := newAgentGrowthHarness(t)
	plan := resourcecheck.Plan{Warmup: 30, MaxWarmup: 300, Cycles: growthCycles(t), Bounds: agentGrowthBounds, SettleTimeout: 3 * time.Second}
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

// The check must fail when a cycle retains something. Each control adds one
// deliberate leak to every cycle of the same harness, under the same bounds,
// and expects that resource to be reported. Fewer cycles keep the leaked
// processes and connections few; each control still exceeds its bound.
func TestResourceGrowthAgentDetectsRetainedResources(t *testing.T) {
	type control struct {
		name  string
		leaks []string // resources the check must report
		leak  func(t *testing.T, h *agentGrowthHarness) func()
	}
	controls := []control{
		{"unclosed provider response bodies", []string{"goroutines", resourcecheck.HandleName}, func(t *testing.T, h *agentGrowthHarness) func() {
			// A fake collector that reads its provider and never closes the
			// body, the failure issue #91 names.
			var bodies []io.Closer
			h.extra = func() error {
				resp, err := h.providerClient.Get("https://api.moonshot.ai/v1/users/me/balance")
				if err != nil {
					return err
				}
				bodies = append(bodies, resp.Body)
				return nil
			}
			return func() {
				for _, body := range bodies {
					body.Close()
				}
			}
		}},
		{"leaked goroutine", []string{"goroutines"}, func(t *testing.T, h *agentGrowthHarness) func() {
			stop := make(chan struct{})
			h.extra = func() error { go func() { <-stop }(); return nil }
			return func() { close(stop) }
		}},
		{"retained heap", []string{"live heap bytes"}, func(t *testing.T, h *agentGrowthHarness) func() {
			var kept [][]byte
			h.extra = func() error { kept = append(kept, make([]byte, 256<<10)); return nil }
			return func() { kept = nil }
		}},
		{"child never waited for", []string{"child processes"}, func(t *testing.T, h *agentGrowthHarness) func() {
			var children []*hangingChild
			h.extra = func() error {
				child, err := startHangingChild()
				if err == nil {
					children = append(children, child)
				}
				return err
			}
			return func() {
				for _, child := range children {
					child.stop()
				}
			}
		}},
	}
	for _, c := range controls {
		t.Run(c.name, func(t *testing.T) {
			if c.leaks[0] == "child processes" && resourcecheck.Measure().Children == resourcecheck.Unmeasured {
				t.Skipf("child processes are unmeasured on %s", runtime.GOOS)
			}
			h := newAgentGrowthHarness(t)
			release := c.leak(t, h)
			defer release()
			leak := h.extra
			plan := resourcecheck.Plan{Warmup: 3, Cycles: 12, Bounds: agentGrowthBounds, SettleTimeout: time.Second}
			result, err := plan.Run(func(measured bool) error {
				// Leak only in measured cycles, after the baseline.
				h.extra = nil
				if measured {
					h.extra = leak
				}
				return h.cycle()
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

type agentGrowthHarness struct {
	store          *usage.Store
	cache          *sessionCache
	agent          *httptest.Server
	client         *http.Client // the fleet side's view of this agent
	providerClient *http.Client
	providerHits   map[string]*atomic.Int64
	extra          func() error // a negative control's deliberate leak
}

func newAgentGrowthHarness(t *testing.T) *agentGrowthHarness {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	isolateGrowthEnv(t, home, exe)
	writeGrowthFixtures(t, home)

	h := &agentGrowthHarness{providerHits: map[string]*atomic.Int64{}}
	provider := httptest.NewServer(h.fakeProvider(t))
	t.Cleanup(provider.Close)
	target, _ := url.Parse(provider.URL)
	// Production uses http.DefaultTransport; a clone keeps its pool settings
	// without sharing this test's idle connections with other tests.
	providerTransport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(providerTransport.CloseIdleConnections)
	h.providerClient = &http.Client{Timeout: 20 * time.Second, Transport: redirectTo{target: target, next: providerTransport}}
	h.store = usage.NewStore(&usage.Fetcher{Home: home, Client: h.providerClient}, "growth", time.Hour)

	// The same sources sessionHandler composes, with a cache the test can
	// expire: production refreshes at most every 5 s of wall time.
	window := 10 * time.Minute
	pollers := []func() sessions.Snapshot{
		sessions.NewScanner(filepath.Join(home, "pi-sessions"), window).Poll,
		sessions.NewClaudeScanner(filepath.Join(home, "claude-projects"), window).Poll,
		t3.New(filepath.Join(home, "t3", "userdata", "statev2.sqlite"), window, filepath.Join(home, "absent-t3")).Poll,
	}
	h.cache = &sessionCache{now: time.Now, poll: func() sessions.Snapshot {
		snap := sessions.MergeSnapshots(pollSources(pollers)...)
		snap.Machine = "growth"
		return snap
	}, last: sessions.Snapshot{Machine: "growth", GeneratedAt: time.Now()}}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /usage", usageHandler(h.store))
	mux.HandleFunc("GET /sessions", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, h.cache.snapshot())
	})
	h.agent = httptest.NewServer(mux)
	t.Cleanup(h.agent.Close)
	clientTransport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(clientTransport.CloseIdleConnections)
	h.client = &http.Client{Timeout: 30 * time.Second, Transport: clientTransport}
	return h
}

// isolateGrowthEnv points every setting the collectors read at fakes.
func isolateGrowthEnv(t *testing.T, home, exe string) {
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", "KIMI_K3_API_KEY", "GITHUB_BILLING_ORG",
		"OPENCODE_GO_WORKSPACE_ID", "OPENCODE_ZEN_WORKSPACE_ID", "OPENCODE_GO_AUTH_COOKIE", "OPENCODE_ZEN_AUTH_COOKIE", "ANTIGRAVITY_CREDENTIALS_FILE"} {
		t.Setenv(name, "")
	}
	t.Setenv("MOONSHOT_API_KEY", "synthetic-moonshot")
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HS_PI_DASHBOARD_AGY_BIN", exe)
	t.Setenv(growthFakeCLI, "agy")
	// A race-instrumented binary sleeps 1 s on exit by default; the fake CLI
	// is this test binary, so drop that wait (the parent is already running).
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	t.Setenv("HS_PI_DASHBOARD_MUSE_BIN", filepath.Join(home, "absent-muse"))
	t.Setenv("HS_PI_DASHBOARD_GROK_T3_DB", filepath.Join(home, "absent-grok.sqlite"))
}

func writeGrowthFixtures(t *testing.T, home string) {
	t.Helper()
	files := map[string]string{
		".codex/auth.json":                                `{"tokens":{"access_token":"synthetic-codex","account_id":"synthetic-account"}}`,
		".claude/.credentials.json":                       `{"claudeAiOauth":{"accessToken":"synthetic-claude"}}`,
		".gemini/antigravity-cli/antigravity-oauth-token": "synthetic",
		"pi-sessions/2026-11-05_growth.jsonl": `{"type":"session","version":3,"id":"growth","timestamp":"2026-11-05T12:00:00Z","cwd":"/repo/growth"}` + "\n" +
			`{"type":"message","id":"u1","timestamp":"2026-11-05T12:00:03Z","message":{"role":"user","content":"steady state"}}` + "\n",
		"claude-projects/repo/session.jsonl": `{"type":"user","sessionId":"growth","uuid":"u1","cwd":"/repo/growth","timestamp":"2026-11-05T12:00:00Z","message":{"role":"user","content":"steady state"}}` + "\n",
	}
	for name, content := range files {
		path := filepath.Join(home, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// The T3 package's offline V2 fixture, rows invented.
	script, err := os.ReadFile(filepath.Join("..", "t3", "testdata", "ingest", "fixture.sql"))
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(home, "t3", "userdata", "statev2.sqlite")
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(script) + growthT3Tables); err != nil {
		t.Fatal(err)
	}
}

// growthT3Tables adds the V2 projections fixture.sql leaves out (it serves
// pmd's ingest), in the shape internal/t3's own tests use, so each poll reads
// every projection instead of stopping at the first missing table.
const growthT3Tables = `
CREATE TABLE orchestration_v2_projection_provider_threads(provider_thread_id TEXT PRIMARY KEY,thread_id TEXT,driver TEXT,payload_json TEXT,provider TEXT);
CREATE TABLE orchestration_v2_projection_provider_turns(provider_thread_id TEXT,ordinal INTEGER,payload_json TEXT);
CREATE TABLE orchestration_v2_projection_messages(thread_id TEXT,role TEXT,payload_json TEXT);
CREATE TABLE orchestration_v2_projection_nodes(node_id TEXT PRIMARY KEY,thread_id TEXT,kind TEXT,payload_json TEXT);
CREATE INDEX v2_turns_thread ON orchestration_v2_projection_provider_turns(provider_thread_id,ordinal);
CREATE INDEX v2_nodes_thread ON orchestration_v2_projection_nodes(thread_id);
CREATE INDEX v2_messages_thread ON orchestration_v2_projection_messages(thread_id);
INSERT INTO orchestration_v2_projection_messages SELECT thread_id,'user','{}' FROM orchestration_v2_projection_threads;`

// fakeProvider answers the three HTTP providers. Every fourth answer fails, so
// the Store's retention of the last good card runs as well.
func (h *agentGrowthHarness) fakeProvider(t *testing.T) http.Handler {
	bodies := map[string]func() string{
		"/backend-api/wham/usage": func() string {
			return `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":9,"limit_window_seconds":604800}}}`
		},
		"/api/oauth/usage": func() string {
			reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
			return `{"limits":[{"kind":"session","percent":5,"resets_at":"` + reset + `"},{"kind":"weekly_all","percent":14,"resets_at":"` + reset + `"}]}`
		},
		"/v1/users/me/balance": func() string { return `{"data":{"available_balance":12.5}}` },
	}
	for path := range bodies {
		h.providerHits[path] = &atomic.Int64{}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			t.Errorf("unexpected provider request %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if h.providerHits[r.URL.Path].Add(1)%4 == 0 {
			http.Error(w, "synthetic outage", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body())
	})
}

type redirectTo struct {
	target *url.URL
	next   http.RoundTripper
}

func (r redirectTo) RoundTrip(req *http.Request) (*http.Response, error) {
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host, out.Host = r.target.Scheme, r.target.Host, ""
	return r.next.RoundTrip(out)
}

func (h *agentGrowthHarness) cycle() error {
	// Usage: one synchronous refresh through Store.Start; its ticker
	// goroutine must then exit on cancellation.
	ctx, cancel := context.WithCancel(context.Background())
	h.store.Start(ctx)
	cancel()

	// Sessions: expire the cache, let a request start the refresh, wait for it.
	h.cache.mu.Lock()
	h.cache.completed = time.Time{}
	h.cache.mu.Unlock()
	if err := h.get("/sessions"); err != nil {
		return err
	}
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(time.Millisecond) {
		h.cache.mu.Lock()
		running := h.cache.running
		h.cache.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("session refresh still running after 10s")
		}
	}
	for _, path := range []string{"/usage", "/sessions"} {
		if err := h.get(path); err != nil {
			return err
		}
	}
	if h.extra != nil {
		return h.extra()
	}
	return nil
}

func (h *agentGrowthHarness) get(path string) error {
	resp, err := h.client.Get(h.agent.URL + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", path, resp.Status)
	}
	return nil
}

// requireExercised proves the cycles reached every fake: each HTTP provider
// once per cycle, the fake agy output, and the fixture T3 database.
func (h *agentGrowthHarness) requireExercised(t *testing.T, cycles int) {
	t.Helper()
	for path, hits := range h.providerHits {
		if hits.Load() < int64(cycles) {
			t.Errorf("provider %s answered %d of %d cycles", path, hits.Load(), cycles)
		}
	}
	cards := map[string]usage.Card{}
	for _, card := range h.store.Snapshot().Cards {
		cards[card.Key] = card
	}
	for _, key := range []string{"codex", "claude", "moonshot", "antigravity"} {
		if card := cards[key]; len(card.Gauges) == 0 && card.BalanceUSD == nil {
			t.Errorf("%s card has no reading from its fake: %+v", key, card)
		}
	}
	snap := h.cache.snapshot()
	sources := map[string]string{}
	for _, source := range snap.Sources {
		sources[source.Source] = source.State + " " + source.Error
	}
	t.Logf("session sources: %v, %d sessions", sources, len(snap.Sessions))
	if len(snap.Sessions) == 0 {
		t.Errorf("no sessions read from the fixtures: %v", sources)
	}
}

// hangingChild is a started child the retained-child control never waits for
// until its cleanup.
type hangingChild struct {
	stdin io.Closer
	wait  func() error
}

func startHangingChild() (*hangingChild, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := backgroundcmd.Command(exe)
	cmd.Env = append(os.Environ(), growthFakeCLI+"=hang", "GORACE=atexit_sleep_ms=0")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &hangingChild{stdin: stdin, wait: cmd.Wait}, nil
}

func (c *hangingChild) stop() {
	c.stdin.Close()
	_ = c.wait()
}
