package fleet

// An agent restart must not make its machine read as healthy and empty (#121).
// These tests drive the real agent /sessions handler and the real fleet
// Server. The agent answers its first request with a placeholder while its
// first collection runs; the fleet keeps the last known rows as inactive
// history beside an explicit pending error until a real collection arrives.

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/agent"
	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
)

const pendingWithRows = sessionCollectionPending + "; showing last known sessions"

// restartableAgent serves one /sessions handler at a time and 404s other
// paths. down drops each connection without a reply, as a stopped agent does;
// restart installs a fresh handler, as a new agent process does.
type restartableAgent struct {
	*httptest.Server
	handler atomic.Pointer[http.Handler]
	down    atomic.Bool
}

func newRestartableAgent(t *testing.T, h http.Handler) *restartableAgent {
	t.Helper()
	a := &restartableAgent{}
	a.handler.Store(&h)
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.down.Load() {
			panic(http.ErrAbortHandler)
		}
		if r.URL.Path != "/sessions" {
			http.NotFound(w, r)
			return
		}
		(*a.handler.Load()).ServeHTTP(w, r)
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *restartableAgent) restart(h http.Handler) {
	a.handler.Store(&h)
	a.down.Store(false)
}

// piAgent builds the real /sessions handler over a temp Pi directory holding
// the given number of active sessions. T3 is off and Claude Code reads an
// empty directory, so no real transcript or database is touched.
func piAgent(t *testing.T, active int) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range active {
		line := `{"type":"session","id":"pi-` + string(rune('a'+i)) + `","timestamp":"` + now + `","cwd":"/repo"}` + "\n"
		if err := os.WriteFile(filepath.Join(dir, "session-"+string(rune('a'+i))+".jsonl"), []byte(line), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return agent.SessionsHandler(agent.Options{Dir: dir, ClaudeDir: t.TempDir(), Machine: "mini", T3DBs: []string{"off"}}), dir
}

// healthyEmpty is the reading the fleet must never invent: online, no error
// and no sessions.
func healthyEmpty(m MachineState) bool {
	return m.Online && m.Error == "" && len(m.Sessions) == 0
}

// pageActiveCount mirrors the page's own count: active rows of online machines.
func pageActiveCount(machines []MachineState) int {
	n := 0
	for _, m := range machines {
		for _, row := range m.Sessions {
			if m.Online && row.Active {
				n++
			}
		}
	}
	return n
}

// machine reads the one target's cached state under the server lock, as
// cachedFleet does. These tests skip cachedFleet itself because it also reads
// the host's Hermes database through hermes.Default().
func machine(t *testing.T, s *Server) MachineState {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.targets) != 1 {
		t.Fatalf("targets = %+v", s.targets)
	}
	state, ok := s.cache[s.targets[0].Name]
	if !ok {
		return MachineState{Name: s.targets[0].Name, Error: "no poll yet"}
	}
	return state
}

// pollUntil polls until ok holds for the machine, failing after 10 s.
func pollUntil(t *testing.T, s *Server, ok func(MachineState) bool) MachineState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		s.poll(context.Background())
		m := machine(t, s)
		if ok(m) {
			return m
		}
		if time.Now().After(deadline) {
			t.Fatalf("machine never reached the expected state: %+v", m)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertCounts(t *testing.T, phase string, m MachineState, want int) {
	t.Helper()
	if m.ActiveSessions != want || activeSessionCount([]MachineState{m}, 0) != want || pageActiveCount([]MachineState{m}) != want {
		t.Fatalf("%s: activeSessions=%d fleet=%d page=%d, want %d", phase, m.ActiveSessions,
			activeSessionCount([]MachineState{m}, 0), pageActiveCount([]MachineState{m}), want)
	}
}

func TestRestartedAgentKeepsLastSessionsUntilFirstCollection(t *testing.T) {
	handler, _ := piAgent(t, 1)
	a := newRestartableAgent(t, handler)
	s := NewServer([]Target{{Name: "mini", URL: a.URL}}, time.Hour)

	// Positive control: a normal reading.
	normal := pollUntil(t, s, func(m MachineState) bool { return len(m.Sessions) == 1 })
	if !normal.Online || normal.Error != "" || !normal.Sessions[0].Active {
		t.Fatalf("normal reading = %+v", normal)
	}
	assertCounts(t, "normal", normal, 1)

	// Positive control: an honest offline reading keeps the last good row.
	a.down.Store(true)
	s.poll(context.Background())
	offline := machine(t, s)
	if offline.Online || offline.Error == "" || len(offline.Sessions) != 1 {
		t.Fatalf("offline reading = %+v", offline)
	}
	assertCounts(t, "offline", offline, 0)

	// The restarted agent's first reply is its pre-collection placeholder.
	// 807e0d2 read this as online, no error and zero sessions.
	restarted, _ := piAgent(t, 1)
	a.restart(restarted)
	s.poll(context.Background())
	pending := machine(t, s)
	if healthyEmpty(pending) {
		t.Fatalf("restarted agent read as healthy and empty: %+v", pending)
	}
	if !pending.Online || pending.Error != pendingWithRows || len(pending.Sessions) != 1 || pending.Sessions[0].ID != "pi-a" {
		t.Fatalf("pending reading = %+v", pending)
	}
	if !pending.GeneratedAt.Equal(normal.GeneratedAt) {
		t.Fatalf("retained rows lost their age: %s, want %s", pending.GeneratedAt, normal.GeneratedAt)
	}
	assertCounts(t, "pending", pending, 0)

	// Recovery: the first completed collection is the normal path again.
	recovered := pollUntil(t, s, func(m MachineState) bool { return m.Error == "" })
	if !recovered.Online || len(recovered.Sessions) != 1 || !recovered.Sessions[0].Active {
		t.Fatalf("recovered reading = %+v", recovered)
	}
	assertCounts(t, "recovered", recovered, 1)
}

func TestFreshAgentPendingThenGenuinelyEmpty(t *testing.T) {
	handler, dir := piAgent(t, 0)
	a := newRestartableAgent(t, handler)
	s := NewServer([]Target{{Name: "mini", URL: a.URL}}, time.Hour)

	// The fleet holds no rows yet. The first reply still reports the pending
	// collection rather than a healthy empty machine.
	s.poll(context.Background())
	pending := machine(t, s)
	if healthyEmpty(pending) || !pending.Online || pending.Error != sessionCollectionPending || len(pending.Sessions) != 0 {
		t.Fatalf("first reading = %+v", pending)
	}
	assertCounts(t, "pending", pending, 0)

	// A completed collection that found nothing is a real, healthy, empty
	// machine: online, no error, zero sessions.
	empty := pollUntil(t, s, func(m MachineState) bool { return m.Error == "" })
	if !healthyEmpty(empty) {
		t.Fatalf("empty machine = %+v", empty)
	}
	resp, err := http.Get(a.URL + "/sessions")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var snap sessions.Snapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap.SessionsDir != dir || snap.Sessions != nil || collectionPending(snap) {
		t.Fatalf("completed empty collection = %+v", snap)
	}
}

// The serve loop and concurrent page reads see no healthy empty sample across
// a restart. The race detector checks the copies of retained rows.
func TestServeLoopNeverShowsRestartedAgentHealthyAndEmpty(t *testing.T) {
	handler, _ := piAgent(t, 1)
	a := newRestartableAgent(t, handler)
	s := NewServer([]Target{{Name: "mini", URL: a.URL}}, 20*time.Millisecond)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- s.serve(ctx, ln) }()
	defer func() { cancel(); <-served }()

	wait := func(phase string, ok func(MachineState) bool) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for !ok(machine(t, s)) {
			if time.Now().After(deadline) {
				t.Fatalf("%s never arrived: %+v", phase, machine(t, s))
			}
			time.Sleep(2 * time.Millisecond)
		}
	}
	wait("normal reading", func(m MachineState) bool { return m.Online && len(m.Sessions) == 1 && m.Error == "" })
	a.down.Store(true)
	wait("offline reading", func(m MachineState) bool { return !m.Online })

	// The pending reading lasts one 20 ms poll. Hold the restarted agent's
	// later requests until the sampler has seen it, so a descheduled sampler
	// cannot miss the window. The first request still gets the placeholder.
	restarted, _ := piAgent(t, 1)
	seen := make(chan struct{})
	var requests atomic.Int32
	a.restart(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) > 1 {
			select {
			case <-seen:
			case <-r.Context().Done():
				return
			}
		}
		restarted.ServeHTTP(w, r)
	}))
	sawPending, recovered := false, false
	deadline := time.Now().Add(10 * time.Second)
	for !recovered && time.Now().Before(deadline) {
		m := machine(t, s)
		if healthyEmpty(m) {
			t.Fatalf("restarted agent read as healthy and empty: %+v", m)
		}
		if m.Online && m.Error == pendingWithRows {
			assertCounts(t, "pending", m, 0)
			if !sawPending {
				sawPending = true
				close(seen)
			}
		}
		recovered = m.Online && m.Error == "" && len(m.Sessions) == 1 && m.Sessions[0].Active
		time.Sleep(time.Millisecond)
	}
	if !sawPending || !recovered {
		t.Fatalf("pending seen=%t, recovered=%t: %+v", sawPending, recovered, machine(t, s))
	}
}

// Retained rows are copies: the cached reading they came from keeps its
// active T3 badge, while the pending reading shows them as unavailable history
// beside the placeholder's own T3 source health.
func TestPendingReadingCopiesRetainedRows(t *testing.T) {
	working := "Working"
	now := time.Now().UTC()
	normal := sessions.Snapshot{Machine: "mini", GeneratedAt: now, SessionsDir: "/pi", ActiveSessions: 2, Sessions: []sessions.Summary{
		{ID: "t3:env:thread", Source: "t3", Active: true, Status: "working", T3Status: &working},
		{ID: "pi-a", Source: "pi", Active: true},
	}}
	// The 807e0d2 agent placeholder with T3 on, byte for byte in shape.
	placeholder := []byte(`{"sources":[{"source":"t3","path":"","state":"unavailable","error":"T3 collection starting"}],"machine":"mini","generatedAt":"` +
		now.Add(time.Minute).Format(time.RFC3339Nano) + `","sessionsDir":"","sessions":null,"activeSessions":0}`)
	var restarted atomic.Bool
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/sessions" {
			http.NotFound(w, r)
			return
		}
		if restarted.Load() {
			_, _ = w.Write(placeholder)
			return
		}
		_ = json.NewEncoder(w).Encode(normal)
	}))
	defer a.Close()
	s := NewServer([]Target{{Name: "mini", URL: a.URL}}, time.Hour)
	s.poll(context.Background())
	before := s.cache["mini"]
	restarted.Store(true)
	s.poll(context.Background())
	pending := machine(t, s)

	if !before.Sessions[0].Active || before.Sessions[0].T3Status == nil || before.Sessions[0].Status != "working" || !before.Sessions[1].Active {
		t.Fatalf("pending reading mutated the cached rows: %+v", before.Sessions)
	}
	if pending.Error != pendingWithRows || len(pending.Sessions) != 2 || !pending.GeneratedAt.Equal(now) {
		t.Fatalf("pending reading = %+v", pending)
	}
	if row := pending.Sessions[0]; row.Active || row.Status != "unavailable" || row.T3Status != nil {
		t.Fatalf("retained T3 row = %+v", row)
	}
	if row := pending.Sessions[1]; row.Active || row.Status != "" {
		t.Fatalf("retained Pi row = %+v", row)
	}
	if len(pending.Sources) != 1 || pending.Sources[0].Error != "T3 collection starting" {
		t.Fatalf("placeholder source health hidden: %+v", pending.Sources)
	}
	assertCounts(t, "pending", pending, 0)

	// A second pending poll retains the same history without growing it.
	s.poll(context.Background())
	if again := machine(t, s); again.Error != pendingWithRows || len(again.Sessions) != 2 || again.Sessions[0].Active {
		t.Fatalf("second pending reading = %+v", again)
	}
}

// A legacy or minimal agent that reports rows without sessionsDir is still a
// completed reading, not a placeholder.
func TestCollectionPendingNeedsBothMarkers(t *testing.T) {
	for _, tc := range []struct {
		snap sessions.Snapshot
		want bool
	}{
		{sessions.Snapshot{}, true},
		{sessions.Snapshot{SessionsDir: "/pi"}, false},
		{sessions.Snapshot{Sessions: []sessions.Summary{}}, false},
		{sessions.Snapshot{Sessions: []sessions.Summary{{ID: "a"}}}, false},
	} {
		if got := collectionPending(tc.snap); got != tc.want {
			t.Errorf("collectionPending(%+v) = %t, want %t", tc.snap, got, tc.want)
		}
	}
}
