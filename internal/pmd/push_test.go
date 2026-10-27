package pmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const secretToken = "pmd-test-token-never-logged"

type harness struct {
	t       *testing.T
	p       *pusher
	now     time.Time
	db      string
	token   string
	content string
	mu      sync.Mutex
	status  int
	reply   string
	posts   []*http.Request
	bodies  []map[string]any
	logs    []string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{t: t, now: time.Date(2026, 10, 27, 16, 0, 0, 0, time.UTC), db: filepath.Join(dir, "statev2.sqlite"),
		token: filepath.Join(dir, "pmd-token"), content: "a", status: http.StatusOK,
		reply: `{"threads":2,"inView":1,"created":0,"changed":0,"ended":0,"items":0}`}
	if err := os.WriteFile(h.db, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.writeToken(secretToken)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		h.posts, h.bodies = append(h.posts, r), append(h.bodies, body)
		w.WriteHeader(h.status)
		_, _ = io.WriteString(w, h.reply)
	}))
	t.Cleanup(srv.Close)
	opts := Options{URL: srv.URL + "/", Machine: "mini", TokenFile: h.token,
		Now:  func() time.Time { return h.now },
		Logf: func(f string, a ...any) { h.logs = append(h.logs, fmt.Sprintf(f, a...)) },
		Build: func(_ context.Context, src Source, now func() time.Time) (any, error) {
			if h.content == "fail" {
				return nil, fmt.Errorf("database is locked")
			}
			return map[string]any{"environment": src.Environment, "generatedAt": now().Format(time.RFC3339Nano),
				"threads": []any{map[string]any{"threadId": h.content}}, "pending": []any{}}, nil
		},
	}
	opts.defaults()
	h.p = newPusher(opts, Source{Path: h.db}, &tokenSource{path: h.token, logf: opts.Logf})
	return h
}

func (h *harness) writeToken(token string) {
	h.t.Helper()
	if err := os.WriteFile(h.token, []byte(token+"\n"), 0o600); err != nil {
		h.t.Fatal(err)
	}
}

// advance moves the clock and runs one scheduler step.
func (h *harness) advance(d time.Duration) {
	h.now = h.now.Add(d)
	h.p.step(context.Background())
}

func (h *harness) touchDB() {
	h.t.Helper()
	f, err := os.OpenFile(h.db+"-wal", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		h.t.Fatal(err)
	}
	_, _ = f.WriteString("x")
	f.Close()
}

func (h *harness) postCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.posts)
}

func (h *harness) expectPosts(want int, when string) {
	h.t.Helper()
	if got := h.postCount(); got != want {
		h.t.Fatalf("%s: %d posts, want %d (logs %q)", when, got, want, h.logs)
	}
}

func (h *harness) expectNoSecretLogged() {
	h.t.Helper()
	for _, line := range h.logs {
		if strings.Contains(line, secretToken) {
			h.t.Fatalf("token logged: %q", line)
		}
	}
}

func TestPushSendsFullSnapshotWithBearerToken(t *testing.T) {
	h := newHarness(t)
	h.advance(0)
	h.expectPosts(1, "first step")
	r := h.posts[0]
	if r.Method != http.MethodPost || r.URL.Path != "/v1/ingest/t3/mini" {
		t.Fatalf("request = %s %s", r.Method, r.URL.Path)
	}
	if r.Header.Get("Authorization") != "Bearer "+secretToken || r.Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers = %v", r.Header)
	}
	if h.bodies[0]["environment"] != "" || h.bodies[0]["threads"] == nil {
		t.Fatalf("body = %v", h.bodies[0])
	}
	if len(h.logs) != 2 || !strings.Contains(h.logs[1], "ok: 2 threads, 1 in view") {
		t.Fatalf("logs = %q, want push on, then one ok", h.logs)
	}
	h.advance(Interval)
	if len(h.logs) != 2 {
		t.Fatalf("a steady push logged again: %q", h.logs)
	}
	h.expectNoSecretLogged()
}

func TestPushEvery30sAndOnChangeAtMostEvery5s(t *testing.T) {
	h := newHarness(t)
	h.advance(0)
	h.expectPosts(1, "start")
	h.advance(10 * time.Second)
	h.expectPosts(1, "quiet at 10s")
	h.touchDB()
	h.advance(time.Second)
	h.expectPosts(2, "change 11s after the last push")
	h.touchDB()
	h.advance(2 * time.Second)
	h.expectPosts(2, "change 2s after the last push")
	h.advance(3 * time.Second)
	h.expectPosts(3, "pending change once 5s passed")
	h.advance(29 * time.Second)
	h.expectPosts(3, "quiet at 29s")
	h.advance(time.Second)
	h.expectPosts(4, "quiet at 30s")
}

func TestMissingTokenTurnsPushOffWithOneLine(t *testing.T) {
	h := newHarness(t)
	if err := os.Remove(h.token); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		h.advance(Interval)
	}
	h.expectPosts(0, "no token file")
	if len(h.logs) != 1 || !strings.Contains(h.logs[0], "pmd push off: no token file") {
		t.Fatalf("logs = %q, want one off line", h.logs)
	}
	h.writeToken(secretToken)
	h.advance(Interval)
	h.expectPosts(1, "token added")
	h.expectNoSecretLogged()
}

func TestTokenFileMustBePrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows uses ACLs, not mode bits")
	}
	h := newHarness(t)
	if err := os.Chmod(h.token, 0o644); err != nil {
		t.Fatal(err)
	}
	h.advance(0)
	h.advance(Interval)
	h.expectPosts(0, "mode 644")
	if len(h.logs) != 1 || !strings.Contains(h.logs[0], "must be mode 600") {
		t.Fatalf("logs = %q", h.logs)
	}
}

func TestAuthRefusalWaitsForANewToken(t *testing.T) {
	h := newHarness(t)
	h.status, h.reply = http.StatusForbidden, `{"error":{"code":"wrong_machine","message":"token \"hs-pi@air\" may post snapshots only for the machine after its @"}}`
	h.advance(0)
	h.expectPosts(1, "first push")
	for i := 0; i < 10; i++ {
		h.content = fmt.Sprint(i) // content changes don't lift an auth hold
		h.touchDB()
		h.advance(Interval)
	}
	h.expectPosts(1, "held after 403")
	if len(h.logs) != 2 || !strings.Contains(h.logs[1], "403 wrong_machine") {
		t.Fatalf("logs = %q, want one 403 line", h.logs)
	}
	h.writeToken(secretToken + "-rotated")
	h.advance(Interval)
	h.expectPosts(2, "token file changed")
	if len(h.logs) != 2 {
		t.Fatalf("the same 403 was logged again: %q", h.logs)
	}
	h.advance(holdFor + time.Second)
	h.expectPosts(3, "hold expired")
	h.expectNoSecretLogged()
}

func TestBadSnapshotWaitsForDifferentContent(t *testing.T) {
	h := newHarness(t)
	h.status, h.reply = http.StatusBadRequest, `{"error":{"code":"bad_request","message":"threads[0].updatedAt is required"}}`
	h.advance(0)
	h.advance(Interval)
	h.advance(Interval)
	h.expectPosts(1, "unchanged after 400")
	h.content = "b"
	h.advance(Interval)
	h.expectPosts(2, "content changed")
	if len(h.logs) != 2 || !strings.Contains(h.logs[1], "threads[0].updatedAt is required") {
		t.Fatalf("logs = %q", h.logs)
	}
	h.status = http.StatusRequestEntityTooLarge
	h.content = "c"
	h.advance(Interval)
	h.advance(Interval)
	h.expectPosts(3, "unchanged after 413")
	if len(h.logs) != 3 {
		t.Fatalf("a distinct 413 must log once: %q", h.logs)
	}
}

func TestStaleSnapshotIsDroppedAndTheNextOneSent(t *testing.T) {
	h := newHarness(t)
	h.status, h.reply = http.StatusConflict, `{"error":{"code":"stale_snapshot","message":"older"}}`
	h.advance(0)
	h.advance(Interval)
	h.advance(Interval)
	h.expectPosts(3, "409 is not held")
	if len(h.logs) != 2 {
		t.Fatalf("logs = %q, want one 409 line", h.logs)
	}
}

func TestServerAndNetworkFailuresRetryOnTheNextTick(t *testing.T) {
	h := newHarness(t)
	h.status, h.reply = http.StatusBadGateway, "bad gateway"
	h.advance(0)
	h.advance(Interval)
	h.expectPosts(2, "5xx retried")
	h.status, h.reply = http.StatusOK, `{"threads":1,"inView":1}`
	h.advance(Interval)
	h.status = http.StatusInternalServerError
	h.advance(Interval)
	h.content = "fail"
	h.advance(Interval)
	h.advance(Interval)
	want := []string{"pmd push on", "502 Bad Gateway", "ok: 1 threads", "500 Internal Server Error", "T3 snapshot unavailable: database is locked"}
	if len(h.logs) != len(want) {
		t.Fatalf("logs = %q, want %q", h.logs, want)
	}
	for i, w := range want {
		if !strings.Contains(h.logs[i], w) {
			t.Fatalf("log %d = %q, want %q", i, h.logs[i], w)
		}
	}
	h.p.opts.URL = "http://127.0.0.1:1"
	h.content = "a"
	h.advance(Interval)
	if !strings.Contains(h.logs[len(h.logs)-1], "pmd unreachable") {
		t.Fatalf("logs = %q", h.logs)
	}
	h.expectNoSecretLogged()
}

func TestEnvironmentLabelsLogLines(t *testing.T) {
	h := newHarness(t)
	h.p.src.Environment = "desktop"
	h.advance(0)
	if h.bodies[0]["environment"] != "desktop" || !strings.Contains(h.logs[1], "pmd push (desktop) ok") {
		t.Fatalf("body %v logs %q", h.bodies[0], h.logs)
	}
}

func TestRunStopsWithItsContext(t *testing.T) {
	h := newHarness(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	opts := h.p.opts
	opts.Tick = time.Millisecond
	opts.Logf = func(string, ...any) {} // two sources log concurrently
	opts.Sources = []Source{{Path: h.db}, {Environment: "desktop", Path: h.db}}
	go func() { Run(ctx, opts); close(done) }()
	deadline := time.After(5 * time.Second)
	for h.postCount() < 2 {
		select {
		case <-deadline:
			t.Fatal("no pushes from Run")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestParseSource(t *testing.T) {
	for _, tc := range []struct {
		in, env, path, err string
	}{
		{in: "server=/x/statev2.sqlite", env: "server", path: "/x/statev2.sqlite"},
		{in: "=/x/statev2.sqlite", env: "", path: "/x/statev2.sqlite"},
		{in: `desktop=C:\Users\User\.t3\userdata\statev2.sqlite`, env: "desktop", path: `C:\Users\User\.t3\userdata\statev2.sqlite`},
		{in: "Server=/x", err: "environment"},
		{in: "/x/statev2.sqlite", err: "env=path"},
		{in: "server=", err: "env=path"},
	} {
		src, err := ParseSource(tc.in)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("ParseSource(%q) err = %v, want %q", tc.in, err, tc.err)
			}
			continue
		}
		if err != nil || src.Environment != tc.env || src.Path != tc.path {
			t.Errorf("ParseSource(%q) = %+v, %v", tc.in, src, err)
		}
	}
}

func TestSources(t *testing.T) {
	explicit := []Source{{Environment: "server", Path: "/a"}, {Environment: "desktop", Path: "/b"}}
	if got, _ := Sources(explicit, []string{"/c"}); len(got) != 2 || got[0].Path != "/a" {
		t.Errorf("explicit = %+v", got)
	}
	if got, _ := Sources(nil, nil); len(got) != 1 || got[0].Environment != "" || filepath.Base(got[0].Path) != "statev2.sqlite" {
		t.Errorf("default = %+v", got)
	}
	if got, _ := Sources(nil, []string{"/c", "/c"}); len(got) != 1 || got[0].Path != "/c" {
		t.Errorf("one database = %+v", got)
	}
	if got, why := Sources(nil, []string{"off"}); got != nil || !strings.Contains(why, "T3 is off") {
		t.Errorf("off = %+v %q", got, why)
	}
	if got, why := Sources(nil, []string{"/c", "/d"}); got != nil || !strings.Contains(why, "-pmd-t3") {
		t.Errorf("several = %+v %q", got, why)
	}
}
