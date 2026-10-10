package t3

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
)

type runtimeFixture struct {
	threads           []liveThread
	status            int
	exchanges, grants int
	scope             string
}

func attachRuntime(t *testing.T, c *Collector) (*runtimeFixture, *httptest.Server) {
	t.Helper()
	f := &runtimeFixture{status: http.StatusOK, scope: "orchestration:read"}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/oauth/token":
			f.exchanges++
			if req.Method != "POST" || req.ParseForm() != nil || req.Form.Get("scope") != "orchestration:read" || req.Form.Get("subject_token") != "one-time-secret" {
				t.Error("incorrect scoped exchange")
			}
			json.NewEncoder(w).Encode(map[string]any{"access_token": "private-session-secret", "token_type": "Bearer", "scope": f.scope, "expires_in": 3600})
		case "/api/orchestration/shell":
			if req.Method != "GET" || req.Header.Get("Authorization") != "Bearer private-session-secret" {
				t.Error("incorrect read-only request")
			}
			w.WriteHeader(f.status)
			if f.status == http.StatusOK {
				json.NewEncoder(w).Encode(map[string]any{"threads": f.threads})
			} else {
				w.Write([]byte("private-server-error-secret"))
			}
		default:
			t.Error("unexpected API operation")
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(server.Close)
	r := newRuntime(c.path, "t3-native")
	r.now = c.now
	r.run = func(_ context.Context, binary string, args ...string) ([]byte, error) {
		f.grants++
		want := []string{"auth", "pairing", "create", "--base-dir", filepath.Dir(filepath.Dir(c.path)), "--ttl", "1m", "--label", "hs-pi-dashboard read-only", "--json"}
		if binary != "t3-native" || !reflect.DeepEqual(args, want) {
			t.Error("incorrect local pairing command")
		}
		return []byte(`{"credential":"one-time-secret"}`), nil
	}
	c.runtime = r
	writeRuntime(t, r.path, server.URL, 1)
	return f, server
}

func writeRuntime(t *testing.T, path, origin string, pid int) {
	t.Helper()
	data, _ := json.Marshal(map[string]any{"pid": pid, "origin": origin, "startedAt": "2026-10-10T01:00:00Z"})
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestLiveWorkingMonitoringQuietStopAndRecovery(t *testing.T) {
	db, c, now := fixture(t)
	for _, id := range []string{"one", "two", "quiet-tool", "monitor", "idle", "dead", "archived"} {
		thread(t, db, id, now.Add(-time.Hour))
	}
	exec(t, db, `UPDATE projection_thread_sessions SET status='ready',active_turn_id=NULL WHERE thread_id='monitor'`)
	exec(t, db, `UPDATE projection_turns SET state='completed' WHERE thread_id='monitor'`)
	f, server := attachRuntime(t, c)
	f.threads = []liveThread{
		{ID: "one", Session: &liveSession{Status: "running"}}, {ID: "two", Session: &liveSession{Status: "running"}},
		{ID: "quiet-tool", Session: &liveSession{Status: "running"}}, {ID: "monitor", Session: &liveSession{Status: "ready"}, BackgroundLiveness: "monitoring"},
		{ID: "idle", Session: &liveSession{Status: "ready"}}, {ID: "dead", Session: &liveSession{Status: "stopped"}},
		{ID: "archived", ArchivedAt: ptr("2026-10-10T00:00:00Z"), Session: &liveSession{Status: "running"}},
	}
	check := func(want int) sessions.Snapshot {
		t.Helper()
		snap := c.Poll()
		if snap.ActiveSessions != want || len(snap.Sessions) != 7 {
			t.Fatalf("count=%d rows=%d health=%+v", snap.ActiveSessions, len(snap.Sessions), snap.Sources)
		}
		return snap
	}
	initial := check(4)
	ids := map[string]bool{}
	for _, s := range initial.Sessions {
		if s.Active {
			ids[strings.TrimPrefix(s.ID, c.prefix())] = true
		}
		if strings.HasSuffix(s.ID, ":monitor") && s.Status != "monitoring" {
			t.Fatal("monitor status")
		}
	}
	if !reflect.DeepEqual(ids, map[string]bool{"one": true, "two": true, "quiet-tool": true, "monitor": true}) {
		t.Fatal(ids)
	}
	c.now = func() time.Time { return now.Add(10 * time.Minute) }
	check(4)
	f.threads[0].Session.Status = "ready"
	check(3)
	if f.grants != 1 || f.exchanges != 1 {
		t.Fatal("credential was not reused")
	}
	f.status = http.StatusUnauthorized
	failed := check(0)
	if failed.Sources[0].State != "unavailable" || !failed.Sources[0].LastSuccessAt.Equal(now.Add(10*time.Minute)) {
		t.Fatal(failed.Sources)
	}
	encoded, _ := json.Marshal(failed)
	if strings.Contains(string(encoded), "secret") {
		t.Fatal("credential or server error leaked")
	}
	f.status = http.StatusOK
	check(3)
	if f.grants != 2 {
		t.Fatal("auth did not renew")
	}
	c.runtime.expires = c.runtime.now().Add(time.Second)
	check(3)
	if f.grants != 3 {
		t.Fatal("expiry did not renew")
	}
	writeRuntime(t, c.runtime.path, server.URL, 2)
	check(3)
	if f.grants != 4 {
		t.Fatal("restart identity did not renew")
	}
	server.Close()
	check(0) // persisted running rows cannot survive a crashed runtime
}

func TestRuntimeFailureCannotUseFreshDatabaseAsLiveness(t *testing.T) {
	db, c, now := fixture(t)
	thread(t, db, "fresh", now)
	f, _ := attachRuntime(t, c)
	f.threads = []liveThread{{ID: "fresh", Session: &liveSession{Status: "running"}}}
	if c.Poll().ActiveSessions != 1 {
		t.Fatal("setup")
	}
	for _, status := range []int{403, 500, 302} {
		f.status = status
		if snap := c.Poll(); snap.ActiveSessions != 0 || snap.Sources[0].State != "unavailable" {
			t.Fatal("fresh DB kept unavailable runtime active")
		}
	}
	f.status = 200
	for _, threads := range [][]liveThread{nil, {{ID: ""}}, {{ID: "fresh", Session: &liveSession{Status: "unknown"}}}, {{ID: "fresh"}, {ID: "fresh"}}, {{ID: "fresh", BackgroundLiveness: "unknown"}}} {
		f.threads = threads
		if c.Poll().ActiveSessions != 0 {
			t.Fatal("invalid snapshot counted")
		}
	}
	f.threads = []liveThread{}
	if snap := c.Poll(); snap.ActiveSessions != 0 || snap.Sources[0].State != "healthy" {
		t.Fatal("empty live snapshot")
	}
	f.scope = "orchestration:read orchestration:operate"
	c.runtime.token = ""
	if snap := c.Poll(); snap.ActiveSessions != 0 || snap.Sources[0].State != "unavailable" {
		t.Fatal("accepted broader credential")
	}
	f.scope = "orchestration:read"
	c.runtime.run = func(context.Context, string, ...string) ([]byte, error) { return nil, errors.New("private CLI secret") }
	if snap := c.Poll(); strings.Contains(snap.Sources[0].Error, "secret") || snap.ActiveSessions != 0 {
		t.Fatal("CLI failure")
	}
}

func TestRuntimeRejectsRemoteDiscoveryBeforeIssuingCredentials(t *testing.T) {
	_, c, _ := fixture(t)
	f, _ := attachRuntime(t, c)
	for _, origin := range []string{"http://example.com:3773", "http://user:pass@127.0.0.1:3773", "file:///tmp/runtime", "http://127.0.0.1:3773/other", "http://127.0.0.1:3773?token=private"} {
		writeRuntime(t, c.runtime.path, origin, 1)
		if _, err := c.runtime.snapshot(context.Background()); err == nil {
			t.Fatal("remote or credential-bearing origin accepted")
		}
	}
	if f.grants != 0 {
		t.Fatal("issued credentials for invalid discovery")
	}
}

func TestRuntimeTimeoutAndMetadataIdentityDedup(t *testing.T) {
	db, c, now := fixture(t)
	thread(t, db, "claude", now)
	exec(t, db, `UPDATE projection_thread_sessions SET provider_name='claudeAgent',provider_session_id='shared'`)
	f, _ := attachRuntime(t, c)
	f.threads = []liveThread{{ID: "claude", BackgroundLiveness: "monitoring", Session: &liveSession{Status: "ready"}}}
	snap := c.Poll()
	native := sessions.Snapshot{Sessions: []sessions.Summary{{ID: "claude-code:shared", Provider: "claude-code", Source: "claude-code", Active: true}}}
	merged := sessions.MergeSnapshots(snap, snap, native)
	if len(merged.Sessions) != 1 || merged.ActiveSessions != 1 || merged.Sessions[0].Status != "monitoring" {
		t.Fatal("duplicate or lost stable identity")
	}
	c.runtime.token = ""
	c.runtime.run = func(ctx context.Context, _ string, _ ...string) ([]byte, error) { <-ctx.Done(); return nil, ctx.Err() }
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := c.runtime.snapshot(ctx); err == nil {
		t.Fatal("timeout ignored")
	}
}

func ptr(s string) *string { return &s }

func TestLiveThreadCreatedBetweenDatabaseAndRuntimeSnapshots(t *testing.T) {
	_, c, now := fixture(t)
	f, _ := attachRuntime(t, c)
	f.threads = []liveThread{{ID: "new", Title: "New live work", CreatedAt: now.Format(time.RFC3339), UpdatedAt: now.Format(time.RFC3339), Session: &liveSession{Status: "running", ProviderName: "codex"}}}
	first := c.Poll()
	if first.ActiveSessions != 1 || len(first.Sessions) != 1 || first.Sessions[0].Name != "New live work" {
		t.Fatal("new live thread was lost")
	}
	second := c.Poll()
	if len(second.Sessions) != 1 || second.Sessions[0].ID != first.Sessions[0].ID {
		t.Fatal("live-only identity changed")
	}
}

func TestLiveStateMatchesWorkingShelf(t *testing.T) {
	tests := []struct {
		thread liveThread
		status string
		active bool
	}{
		{liveThread{Session: &liveSession{Status: "starting"}}, "connecting", true},
		{liveThread{Session: &liveSession{Status: "ready"}, HasPendingApprovals: true}, "waiting", true},
		{liveThread{Session: &liveSession{Status: "ready"}, HasPendingUserInput: true}, "waiting", true},
		{liveThread{Session: &liveSession{Status: "ready"}, BackgroundLiveness: "working"}, "working", true},
		{liveThread{Session: &liveSession{Status: "error"}, BackgroundLiveness: "monitoring"}, "error", false},
		{liveThread{Session: &liveSession{Status: "interrupted"}}, "stopped", false},
		{liveThread{}, "idle", false},
	}
	for _, tt := range tests {
		status, active := tt.thread.state()
		if status != tt.status || active != tt.active {
			t.Fatalf("state %s/%v", status, active)
		}
	}
}
