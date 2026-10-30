package fleet

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
)

func TestParseTargets(t *testing.T) {
	targets, err := ParseTargets("home|http://100.101.122.39:8787, laptop|http://100.117.202.124:8787")
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].Name != "home" || targets[1].URL != "http://100.117.202.124:8787" {
		t.Fatalf("targets = %+v", targets)
	}
	if _, err := ParseTargets("broken"); err == nil {
		t.Fatal("expected error for entry without name|url")
	}
	if _, err := ParseTargets("  "); err == nil {
		t.Fatal("expected error for empty spec")
	}
}

func TestServerAggregatesMachinesAndMarksOffline(t *testing.T) {
	payload, err := json.Marshal(sessions.Snapshot{
		Machine:     "home",
		GeneratedAt: time.Now(),
		Sessions: []sessions.Summary{{
			ID:           "abc",
			Project:      "hs-tui-launcher",
			Active:       true,
			Model:        "gpt-6-astra",
			LastActivity: time.Now(),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(payload)
	}))
	defer agent.Close()

	url := agent.URL
	targets, err := ParseTargets("home|" + url + ",ghost|http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}

	server := NewServer(targets, time.Second)
	server.poll(context.Background())
	// Poll directly against a real HTTP mux so handleFleet sees the cache.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/fleet", server.handleFleet)
	req := httptest.NewRequest(http.MethodGet, "/api/fleet", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var snap FleetSnapshot
	if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Machines) != 2 {
		t.Fatalf("machines = %d, want 2", len(snap.Machines))
	}

	home := snap.Machines[0]
	if !home.Online || home.Error != "" {
		t.Fatalf("home state = %+v", home)
	}
	if len(home.Sessions) != 1 || home.Sessions[0].ID != "abc" {
		t.Fatalf("home sessions = %+v", home.Sessions)
	}

	ghost := snap.Machines[1]
	if ghost.Online || ghost.Error == "" {
		t.Fatalf("ghost state = %+v", ghost)
	}
}

func TestActiveSessionCount(t *testing.T) {
	machines := []MachineState{
		{
			Name:           "online",
			Online:         true,
			ActiveSessions: 1,
		},
		{
			Name:           "offline-with-stale-cache",
			Online:         false,
			ActiveSessions: 1,
		},
	}
	if got := activeSessionCount(machines, 1); got != 2 {
		t.Fatalf("activeSessionCount() = %d, want 2", got)
	}
}

func TestRoutesServePinnedSmoothieChartsWithoutInternet(t *testing.T) {
	server := NewServer([]Target{{Name: "test", URL: "http://127.0.0.1:1"}}, time.Second)
	req := httptest.NewRequest(http.MethodGet, "/assets/smoothie-1.36.1.js", nil)
	rec := httptest.NewRecorder()
	server.routes().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/javascript; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	if !strings.Contains(rec.Body.String(), "SmoothieChart") {
		t.Fatal("embedded Smoothie Charts runtime was not served")
	}
}

func TestServerKeepsStaleSessionsWhenAgentFails(t *testing.T) {
	fail := false
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		_, _ = w.Write([]byte(`{"machine":"home","sessions":[{"id":"keep","active":false}]}`))
	}))
	defer agent.Close()

	targets, err := ParseTargets("home|" + agent.URL)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(targets, time.Hour) // never repolls on its own
	server.poll(context.Background())

	snap := server.cachedFleet()
	if len(snap.Machines) != 1 || !snap.Machines[0].Online || len(snap.Machines[0].Sessions) != 1 {
		t.Fatalf("first poll = %+v", snap.Machines)
	}

	fail = true
	server.poll(context.Background())
	snap = server.cachedFleet()
	state := snap.Machines[0]
	if state.Online {
		t.Fatal("state should be offline after failure")
	}
	if !strings.Contains(state.Error, "500") {
		t.Fatalf("error = %q", state.Error)
	}
	if len(state.Sessions) != 1 || state.Sessions[0].ID != "keep" {
		t.Fatalf("stale sessions not retained: %+v", state.Sessions)
	}
}

func TestT3SourceHealthSurvivesRepeatedOfflinePolls(t *testing.T) {
	fail := false
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"sessions":[{"id":"t3:one","source":"t3","active":true}],"activeSessions":1,"sources":[{"source":"t3","state":"unavailable","error":"locked"}]}`))
	}))
	defer agent.Close()
	server := NewServer([]Target{{Name: "mini", URL: agent.URL}}, time.Hour)
	server.poll(context.Background())
	if server.cache["mini"].Sources[0].Error != "locked" {
		t.Fatal("source health lost")
	}
	fail = true
	for range 2 {
		server.poll(context.Background())
		state := server.cache["mini"]
		if state.Online || state.ActiveSessions != 0 || len(state.Sessions) != 1 || len(state.Sources) != 1 {
			t.Fatalf("cached offline T3: %+v", state)
		}
		if activeSessionCount([]MachineState{state}, 0) != 0 {
			t.Fatal("offline T3 counted in Pulse")
		}
	}
}

func TestFleetCarriesMuseAllowanceFieldsAlongsideOldAgents(t *testing.T) {
	// mini runs the new agent; old agents send no Muse card and keep working.
	newAgent := `{"machine":"mini","cards":[{"key":"muse-code","label":"Muse Code","kind":"quota","subscriptionPriceUsd":null,` +
		`"plan":"Muse Code Power Usage","allowanceSource":"mint","observedAt":"2026-10-26T17:54:05.064Z","freshForSeconds":420,` +
		`"fetchedAt":"2026-10-26T17:54:06Z","gauges":[{"bucketId":"window","label":"5h","usedPercent":15,` +
		`"resetAt":"2026-10-26T22:18:24Z","windowSeconds":18000,"projectedEnd":126.1},{"bucketId":"weekly","label":"Weekly",` +
		`"usedPercent":5,"resetAt":"2026-11-02T00:00:00Z","windowSeconds":604800,"projectedEnd":46.9}]},` +
		`{"key":"muse-code-unavailable-probe","label":"x","kind":"quota","error":"allowance unavailable (CLI reported none)","fetchedAt":"2026-10-26T17:54:06Z"}]}`
	oldAgent := `{"machine":"home","cards":[{"key":"codex","label":"Codex","kind":"quota","fetchedAt":"2026-10-26T17:54:06Z","gauges":[{"label":"Weekly","usedPercent":10}]}]}`
	serve := func(body string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	}
	mini, home := serve(newAgent), serve(oldAgent)
	defer mini.Close()
	defer home.Close()
	targets, err := ParseTargets("mini|" + mini.URL + ",home|" + home.URL)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(targets, time.Second)
	server.pollUsage(context.Background())
	rec := httptest.NewRecorder()
	server.handleFleet(rec, httptest.NewRequest(http.MethodGet, "/api/fleet", nil))
	var raw struct {
		Machines []struct {
			Name  string `json:"name"`
			Usage struct {
				Cards []map[string]any `json:"cards"`
			} `json:"usage"`
		} `json:"machines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	muse := raw.Machines[0].Usage.Cards[0]
	if muse["key"] != "muse-code" || muse["plan"] != "Muse Code Power Usage" || muse["allowanceSource"] != "mint" ||
		muse["observedAt"] != "2026-10-26T17:54:05.064Z" || muse["freshForSeconds"] != 420.0 {
		t.Fatalf("muse card=%v", muse)
	}
	gauges := muse["gauges"].([]any)
	short := gauges[0].(map[string]any)
	if len(gauges) != 2 || short["bucketId"] != "window" || short["usedPercent"] != 15.0 || short["windowSeconds"] != 18000.0 {
		t.Fatalf("gauges=%v", gauges)
	}
	// An unavailable reading stays an error with no windows, never zero.
	if probe := raw.Machines[0].Usage.Cards[1]; probe["error"] == nil || probe["gauges"] != nil {
		t.Fatalf("unavailable=%v", probe)
	}
	if old := raw.Machines[1].Usage.Cards; len(old) != 1 || old[0]["key"] != "codex" || old[0]["plan"] != nil {
		t.Fatalf("old agent=%v", old)
	}
}

// Windows pairing alone can exceed the former three-second HTTP limit.
func TestColdSessionCollectionRemainsOnline(t *testing.T) {
	agent := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(3100 * time.Millisecond):
			_ = json.NewEncoder(w).Encode(sessions.Snapshot{Machine: "laptop", Sessions: []sessions.Summary{{ID: "cold-thread", Active: true}}})
		case <-r.Context().Done():
		}
	}))
	defer agent.Close()
	server := NewServer(nil, time.Second)
	got := server.fetch(context.Background(), Target{Name: "laptop", URL: agent.URL})
	if !got.Online || len(got.Sessions) != 1 || got.Sessions[0].ID != "cold-thread" {
		t.Fatalf("cold collection lost: %+v", got)
	}
}
