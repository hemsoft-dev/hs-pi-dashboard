package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
)

func TestSessionsEndpointCombinesPiAndClaudeForFleet(t *testing.T) {
	piDir, claudeDir := t.TempDir(), t.TempDir()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	write := func(dir, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(content+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(piDir, `{"type":"session","id":"same","timestamp":"`+now+`","cwd":"/repo"}`)
	write(claudeDir, `{"type":"user","sessionId":"same","uuid":"u1","timestamp":"`+now+`","cwd":"/repo","message":{"role":"user","content":"Hello"}}`)
	handler := sessionHandler(Options{Dir: piDir, ClaudeDir: claudeDir, Machine: "test-machine", T3DBs: []string{"off"}})
	server := httptest.NewServer(handler)
	defer server.Close()
	deadline := time.Now().Add(10 * time.Second)
	for range 2 {
	retry:
		response, err := http.Get(server.URL + "/sessions")
		if err != nil {
			t.Fatal(err)
		}
		var snap sessions.Snapshot
		err = json.NewDecoder(response.Body).Decode(&snap)
		response.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if len(snap.Sessions) == 0 && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
			goto retry
		}
		if response.StatusCode != 200 || snap.Machine != "test-machine" || snap.ActiveSessions != 2 || len(snap.Sessions) != 2 {
			t.Fatalf("agent response = %+v", snap)
		}
		ids := map[string]bool{}
		apps := map[string]string{}
		for _, s := range snap.Sessions {
			ids[s.ID] = true
			apps[s.ID] = s.Source
		}
		if apps["same"] != "pi" || apps["claude-code:same"] != "claude-code" {
			t.Fatalf("application identity missing from endpoint: %+v", apps)
		}
		if !ids["same"] || !ids["claude-code:same"] {
			t.Fatalf("session ids collided: %+v", ids)
		}
	}
}

func TestT3FailureDoesNotBreakPiEndpoint(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if err := os.WriteFile(filepath.Join(dir, "session.jsonl"), []byte(`{"type":"session","id":"pi","timestamp":"`+now+`","cwd":"/repo"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	handler := sessionHandler(Options{Dir: dir, ClaudeDir: t.TempDir(), T3DBs: []string{filepath.Join(t.TempDir(), "absent.sqlite")}})
	var rec *httptest.ResponseRecorder
	var snap sessions.Snapshot
	deadline := time.Now().Add(10 * time.Second)
	for {
		rec = httptest.NewRecorder()
		handler(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
		if err := json.Unmarshal(rec.Body.Bytes(), &snap); err != nil {
			t.Fatal(err)
		}
		if len(snap.Sessions) > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if rec.Code != http.StatusOK || len(snap.Sessions) != 1 || snap.ActiveSessions != 1 || len(snap.Sources) != 1 || snap.Sources[0].State != "unavailable" {
		t.Fatalf("independent sources: %+v", snap)
	}
}

func TestSlowSourcesPollTogetherAndKeepTheirOwnResults(t *testing.T) {
	started := make(chan int, 3)
	release := make(chan struct{})
	var pollers []func() sessions.Snapshot
	for i := range 3 {
		pollers = append(pollers, func() sessions.Snapshot {
			started <- i
			<-release
			return sessions.Snapshot{Machine: []string{"Pi", "first T3", "second T3"}[i]}
		})
	}
	done := make(chan []sessions.Snapshot, 1)
	go func() { done <- pollSources(pollers) }()
	for range 3 {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			t.Fatal("one slow source blocked independent source polls")
		}
	}
	close(release)
	results := <-done
	for i, name := range []string{"Pi", "first T3", "second T3"} {
		if results[i].Machine != name {
			t.Fatalf("source ordering: %+v", results)
		}
	}
}
