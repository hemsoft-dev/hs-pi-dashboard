package fleet

// Poll timing with a stalled agent (issue #117). A stalled agent accepts the
// connection and never answers. It must cost about one fleet timeout per poll,
// leave healthy agents on the configured cadence and never hold up /healthz.
// The tests scale the interval and timeout down and assert multiples of them;
// TestProductionTimingWithStalledTarget repeats the issue's measurements with
// the production 5 s interval and 3 s timeout when
// HS_PI_DASHBOARD_PRODUCTION_TIMING=1.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/herdr"
	"github.com/HemSoft/hs-pi-dashboard/internal/projects"
	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
)

const projectsUnavailable = "projects endpoint unavailable; update agent or check machine connection"

// fakeAgent answers /sessions, /herdr and /projects with fresh data. While
// stalled is set it accepts each request and blocks until the request's
// context ends, recording how long each one was held.
type fakeAgent struct {
	*httptest.Server
	stalled atomic.Bool

	mu   sync.Mutex
	held map[string][]time.Duration
}

func newFakeAgent(t *testing.T, name string) *fakeAgent {
	t.Helper()
	a := &fakeAgent{held: map[string][]time.Duration{}}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.stalled.Load() {
			a.hold(r)
			return
		}
		now := time.Now()
		var payload any
		switch r.URL.Path {
		case "/sessions":
			payload = sessions.Snapshot{Machine: name, GeneratedAt: now, Sessions: []sessions.Summary{{ID: name + "-keep", Source: "pi"}}}
		case "/herdr":
			payload = herdr.Snapshot{GeneratedAt: now, Available: true, Sessions: []herdr.Session{{Name: name, PaneCount: 1}}}
		case "/projects":
			payload = projects.Snapshot{GeneratedAt: now, Results: []projects.Result{{Path: "/fake/" + name, AttemptedAt: now}}}
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *fakeAgent) hold(r *http.Request) {
	start := time.Now()
	<-r.Context().Done()
	a.mu.Lock()
	a.held[r.URL.Path] = append(a.held[r.URL.Path], time.Since(start))
	a.mu.Unlock()
}

func (a *fakeAgent) holds() map[string][]time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	held := map[string][]time.Duration{}
	for path, d := range a.held {
		held[path] = append([]time.Duration(nil), d...)
	}
	return held
}

// countRequests wraps agentClient for the test and counts, per host and path,
// the requests the fleet server has in flight at once: from the start of each
// round trip until it fails or its response body is closed. The agent's own
// view lags, because it notices a cancelled request only once it reads the
// closed connection.
func countRequests(t *testing.T) func() map[string]int {
	t.Helper()
	c := &countingTransport{next: agentClient.Transport, inFlight: map[string]int{}, maxIn: map[string]int{}}
	saved := agentClient
	agentClient = &http.Client{Transport: c}
	t.Cleanup(func() { agentClient = saved })
	return func() map[string]int {
		c.mu.Lock()
		defer c.mu.Unlock()
		maxIn := map[string]int{}
		for key, n := range c.maxIn {
			maxIn[key] = n
		}
		return maxIn
	}
}

type countingTransport struct {
	next     http.RoundTripper
	mu       sync.Mutex
	inFlight map[string]int
	maxIn    map[string]int
}

func (c *countingTransport) add(key string, delta int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inFlight[key] += delta
	c.maxIn[key] = max(c.maxIn[key], c.inFlight[key])
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	key := r.URL.Host + r.URL.Path
	c.add(key, 1)
	resp, err := c.next.RoundTrip(r)
	if err != nil {
		c.add(key, -1)
		return nil, err
	}
	resp.Body = &countedBody{ReadCloser: resp.Body, done: func() { c.add(key, -1) }}
	return resp, nil
}

type countedBody struct {
	io.ReadCloser
	once sync.Once
	done func()
}

func (b *countedBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.done)
	return err
}

func (s *Server) cached(name string) (MachineState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.cache[name]
	return state, ok
}

// startServe runs s.serve on a loopback port and returns its base URL and
// when it started. Cleanup cancels it and waits for its poll loops.
func startServe(t *testing.T, s *Server) (string, time.Time) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	started := time.Now()
	go func() { done <- s.serve(ctx, ln) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("serve returned %v, want context.Canceled", err)
		}
	})
	return "http://" + ln.Addr().String(), started
}

func stalledFleet(t *testing.T, pollEvery, timeout time.Duration) (*Server, *fakeAgent, *fakeAgent) {
	t.Helper()
	healthy, stalled := newFakeAgent(t, "healthy"), newFakeAgent(t, "stalled")
	s := NewServer([]Target{{Name: "healthy", URL: healthy.URL}, {Name: "stalled", URL: stalled.URL}}, pollEvery)
	s.timeout = timeout
	s.usagePoll = time.Hour
	return s, healthy, stalled
}

// The three endpoint requests of a stalled target share the poll's wall time:
// one poll takes one timeout, not three.
func TestStalledTargetCostsOneTimeoutPerPoll(t *testing.T) {
	s, _, stalled := stalledFleet(t, time.Hour, 400*time.Millisecond)
	stalled.stalled.Store(true)
	start := time.Now()
	s.poll(context.Background())
	elapsed := time.Since(start)
	if elapsed > s.timeout*3/2 {
		t.Fatalf("poll took %s with a stalled target, want at most 1.5 x the %s timeout", elapsed, s.timeout)
	}
	if elapsed < s.timeout*9/10 {
		t.Fatalf("poll took %s, shorter than the %s timeout: the stalled requests were not held to their deadline", elapsed, s.timeout)
	}
	if state, _ := s.cached("healthy"); !state.Online || len(state.Sessions) != 1 {
		t.Fatalf("healthy = %+v", state)
	}
}

// A target that stalls after a good poll goes offline with the same error
// texts as before #117 and keeps its last good sessions and projects.
func TestStalledTargetKeepsLastGoodStateAndErrors(t *testing.T) {
	s, _, stalled := stalledFleet(t, time.Hour, 150*time.Millisecond)
	s.poll(context.Background())
	good, _ := s.cached("stalled")
	if !good.Online || len(good.Sessions) != 1 || good.Projects == nil || len(good.Projects.Results) != 1 {
		t.Fatalf("first poll = %+v", good)
	}
	stalled.stalled.Store(true)
	for range 2 {
		s.poll(context.Background())
		state, _ := s.cached("stalled")
		if want := fmt.Sprintf("Get %q: context deadline exceeded", stalled.URL+"/sessions"); state.Online || state.Error != want {
			t.Fatalf("online=%v error=%q, want offline with %q", state.Online, state.Error, want)
		}
		if state.Herdr == nil || state.Herdr.Error != "Live agent collector is unreachable" || len(state.Herdr.Sessions) != 0 {
			t.Fatalf("herdr = %+v", state.Herdr)
		}
		if state.Projects == nil || state.Projects.Error != projectsUnavailable || len(state.Projects.Results) != 1 {
			t.Fatalf("projects = %+v", state.Projects)
		}
		if len(state.Sessions) != 1 || state.Sessions[0].ID != "stalled-keep" || !state.GeneratedAt.Equal(good.GeneratedAt) {
			t.Fatalf("last good sessions not kept: %+v at %s", state.Sessions, state.GeneratedAt)
		}
	}
}

// The server answers while the first poll still waits on a stalled target.
func TestServeAnswersBeforeFirstPollFinishes(t *testing.T) {
	s, _, stalled := stalledFleet(t, time.Hour, time.Second)
	stalled.stalled.Store(true)
	base, started := startServe(t, s)
	client := &http.Client{Timeout: 5 * time.Second}
	for _, path := range []string{"/healthz", "/"} {
		resp, err := client.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d", path, resp.StatusCode)
		}
	}
	if elapsed := time.Since(started); elapsed >= s.timeout {
		t.Fatalf("server first answered %s after start, want under the %s timeout", elapsed, s.timeout)
	}
	if _, polled := s.cached("stalled"); polled {
		t.Fatal("the first poll had already finished, so the test proves nothing")
	}
}

// watchCadence samples the healthy target's cached GeneratedAt and returns the
// longest wait between observed changes over the window.
func watchCadence(s *Server, window, sample time.Duration) (longest time.Duration, changes int) {
	var last time.Time
	var lastChange time.Time
	deadline := time.Now().Add(window)
	for now := time.Now(); now.Before(deadline); now = time.Now() {
		state, _ := s.cached("healthy")
		if !state.GeneratedAt.IsZero() && !state.GeneratedAt.Equal(last) {
			if !lastChange.IsZero() {
				longest = max(longest, now.Sub(lastChange))
			}
			last, lastChange = state.GeneratedAt, now
			changes++
		}
		time.Sleep(sample)
	}
	if !lastChange.IsZero() {
		longest = max(longest, time.Since(lastChange))
	}
	return longest, changes
}

// With the production ratio of timeout to interval (3:5), a stalled target
// leaves the healthy target's cache on the configured cadence.
func TestServeKeepsHealthyCadenceWhileTargetStalls(t *testing.T) {
	s, _, stalled := stalledFleet(t, 500*time.Millisecond, 300*time.Millisecond)
	stalled.stalled.Store(true)
	startServe(t, s)
	longest, changes := watchCadence(s, 3*time.Second, 5*time.Millisecond)
	if limit := s.pollEvery * 11 / 10; longest > limit {
		t.Fatalf("healthy cache went %s without a change (%d changes), want at most %s", longest, changes, limit)
	}
	if changes < 5 {
		t.Fatalf("healthy cache changed %d times in 3 s at a %s interval", changes, s.pollEvery)
	}
}

// Polls run one at a time even when a stalled target makes a poll outlast the
// interval: each endpoint has at most one request in flight, and each request
// is cancelled at its deadline.
func TestPollRequestsNeverOverlapAndHonorDeadline(t *testing.T) {
	s, healthy, stalled := stalledFleet(t, 20*time.Millisecond, 150*time.Millisecond)
	stalled.stalled.Store(true)
	inFlight := countRequests(t)
	startServe(t, s)
	paths := []string{"/sessions", "/herdr", "/projects"}
	deadline := time.Now().Add(5 * time.Second)
	for {
		held := stalled.holds()
		enough := true
		for _, path := range paths {
			enough = enough && len(held[path]) >= 4
		}
		if enough {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("stalled target finished too few requests: %v", held)
		}
		time.Sleep(10 * time.Millisecond)
	}
	maxIn, held := inFlight(), stalled.holds()
	for _, path := range paths {
		for _, agent := range []*fakeAgent{healthy, stalled} {
			if key := agent.Listener.Addr().String() + path; maxIn[key] != 1 {
				t.Errorf("%s had %d requests in flight at once, want 1", key, maxIn[key])
			}
		}
		for _, d := range held[path] {
			if d < s.timeout*8/10 || d > s.timeout+250*time.Millisecond {
				t.Errorf("%s request held %s, want it cancelled at its %s deadline", path, d, s.timeout)
			}
		}
	}
}

// The issue's acceptance measurements at production settings (about 35 s).
func TestProductionTimingWithStalledTarget(t *testing.T) {
	if os.Getenv("HS_PI_DASHBOARD_PRODUCTION_TIMING") != "1" {
		t.Skip("set HS_PI_DASHBOARD_PRODUCTION_TIMING=1 to run the 35 s production-timing check")
	}
	healthy, stalled := newFakeAgent(t, "healthy"), newFakeAgent(t, "stalled")
	targets := []Target{{Name: "healthy", URL: healthy.URL}, {Name: "stalled", URL: stalled.URL}}
	stalled.stalled.Store(true)

	direct := NewServer(targets, 5*time.Second)
	start := time.Now()
	direct.poll(context.Background())
	pollTook := time.Since(start)
	t.Logf("one poll with the production %s timeout: %s", direct.timeout, pollTook)
	if pollTook > 3500*time.Millisecond {
		t.Errorf("poll took %s, want at most 3.5 s", pollTook)
	}

	s := NewServer(targets, 0) // production defaults: 5 s interval, 3 s timeout
	base, started := startServe(t, s)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(base + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	healthzAfter := time.Since(started)
	t.Logf("/healthz answered %s after serve started", healthzAfter)
	if resp.StatusCode != http.StatusOK || healthzAfter > time.Second {
		t.Errorf("/healthz status %d after %s, want 200 within 1 s", resp.StatusCode, healthzAfter)
	}
	longest, changes := watchCadence(s, 30*time.Second, 50*time.Millisecond)
	t.Logf("healthy cache: %d changes in 30 s, longest gap %s", changes, longest)
	if longest > 5500*time.Millisecond {
		t.Errorf("healthy cache went %s without a change, want at most 5.5 s", longest)
	}
	state, _ := s.cached("stalled")
	if state.Online || state.Herdr == nil || state.Herdr.Error != "Live agent collector is unreachable" ||
		state.Projects == nil || state.Projects.Error != projectsUnavailable {
		t.Errorf("stalled = %+v", state)
	}
}
