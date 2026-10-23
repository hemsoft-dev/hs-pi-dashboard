package projects

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, e := os.ReadFile("testdata/gh-x-v0.22.0.json")
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func TestRealProducerContract(t *testing.T) {
	d, e := Parse(fixture(t))
	if e != nil {
		t.Fatal(e)
	}
	if d.Repository.Name != "hemsoft-dev/hs-pi-dashboard" || len(d.Issues) != 2 || len(d.PullRequests.Merged) != 5 || d.PullRequests.Merged[0]["aiReview"] != "pass" || !d.WorkflowRunsPerfect {
		t.Fatalf("lost producer fields: %+v", d)
	}
	// Preserve future verdict strings rather than turning them into local policy.
	b := strings.ReplaceAll(string(fixture(t)), `"aiReview":"pass"`, `"aiReview":"future-verdict"`)
	d, e = Parse([]byte(b))
	if e != nil || d.PullRequests.Merged[0]["aiReview"] != "future-verdict" {
		t.Fatal("verdict changed", e)
	}
}
func TestInvalidContract(t *testing.T) {
	for _, b := range []string{"null", "{}", `{"schemaVersion":2}`, strings.Replace(string(fixture(t)), `"schemaVersion":1`, `"schemaVersion":2`, 1), strings.Replace(string(fixture(t)), `"clean":true`, `"clean":null`, 1), strings.Replace(string(fixture(t)), `"name":"issues"`, `"name":"unknown"`, 1)} {
		if _, e := Parse([]byte(b)); e == nil {
			t.Fatalf("accepted %s", b[:min(40, len(b))])
		}
	}
}
func TestBoundedCollectionAndCacheRecovery(t *testing.T) {
	s := New([]string{"a", "b", "c", "d", "e", "a"}, time.Minute)
	data := fixture(t)
	var live, maxLive atomic.Int32
	s.run = func(ctx context.Context, p string) ([]byte, error) {
		n := live.Add(1)
		defer live.Add(-1)
		for old := maxLive.Load(); n > old; old = maxLive.Load() {
			if maxLive.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		return data, nil
	}
	s.refresh(context.Background())
	initial := s.Snapshot()
	if maxLive.Load() != 1 || len(initial.Results) != 5 || initial.Results[0].Document == nil {
		t.Fatal("unbounded or missing collection", maxLive.Load())
	}
	s.run = func(context.Context, string) ([]byte, error) { return nil, errors.New("token=SECRET") }
	s.refresh(context.Background())
	failed := s.Snapshot()
	if failed.Results[0].Error == "" || strings.Contains(failed.Results[0].Error, "SECRET") || failed.Results[0].Document.GeneratedAt != initial.Results[0].Document.GeneratedAt {
		t.Fatal("lost cached age/error safety")
	}
	if initial.Results[0].Error != "" {
		t.Fatal("mutated published snapshot")
	}
	s.run = func(context.Context, string) ([]byte, error) { return data, nil }
	s.refresh(context.Background())
	if s.Snapshot().Results[0].Error != "" {
		t.Fatal("did not recover")
	}
}
func TestMergePreservesMachineHealthAndOneHostedView(t *testing.T) {
	d, _ := Parse(fixture(t))
	newer := *d
	newer.GeneratedAt = d.GeneratedAt.Add(time.Minute)
	newer.Issues = append(append([]map[string]any{}, d.Issues...), map[string]any{"number": 99})
	view := Merge([]Machine{{Name: "mini", Online: true, Snapshot: &Snapshot{Results: []Result{{Path: "/mini", Document: d}}}}, {Name: "other", Online: false, Snapshot: &Snapshot{Error: "unreachable", Results: []Result{{Path: "/other", Document: &newer, Error: "failed"}}}}, {Name: "old"}})
	if len(view.Repositories) != 1 || len(view.Repositories[0].Locals) != 2 || len(view.Repositories[0].GitHub.Issues) != 3 || len(view.Errors) != 3 {
		t.Fatalf("bad merge: %+v", view)
	}
	if view.Repositories[0].Locals[1].Online || view.Repositories[0].Locals[1].Error != "failed" {
		t.Fatal("offline/error hidden")
	}
}
func TestOffAndLimit(t *testing.T) {
	s := New([]string{"off", "some-checkout"}, 0)
	s.refresh(context.Background())
	if len(s.Snapshot().Results) != 0 {
		t.Fatal("off collected")
	}
	b := limitedBuffer{}
	if _, e := b.Write(make([]byte, (8<<20)+1)); e == nil {
		t.Fatal("unbounded output")
	}
}

func TestUnsetAndEmptyPathsNeverDiscoverOrRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := os.MkdirAll(filepath.Join(home, "github", "owner", "active", ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, paths := range [][]string{nil, {}, {""}, {" ", "\t"}, {"off"}, {"active", " off "}} {
		s := New(paths, 0)
		s.run = func(context.Context, string) ([]byte, error) {
			t.Fatal("disabled collector invoked gh-x")
			return nil, nil
		}
		s.refresh(context.Background())
		if len(s.paths) != 0 || len(s.Snapshot().Results) != 0 {
			t.Fatalf("discovered repositories for %#v", paths)
		}
	}
}

func TestPollingBudget(t *testing.T) {
	for _, tc := range []struct{ given, want time.Duration }{
		{0, 30 * time.Minute}, {-time.Second, 30 * time.Minute}, {5 * time.Minute, 15 * time.Minute}, {15 * time.Minute, 15 * time.Minute}, {time.Hour, time.Hour},
	} {
		if got := New(nil, tc.given).every; got != tc.want {
			t.Fatalf("poll %s became %s, want %s", tc.given, got, tc.want)
		}
	}
}

func TestCancellationDoesNotLaunchRemainingRepositoriesOrPublishFreshness(t *testing.T) {
	s := New([]string{"a", "b", "c"}, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	s.run = func(context.Context, string) ([]byte, error) { calls++; cancel(); return fixture(t), nil }
	s.refresh(ctx)
	if calls != 1 || !s.Snapshot().GeneratedAt.IsZero() {
		t.Fatalf("cancelled sweep: calls=%d snapshot=%+v", calls, s.Snapshot())
	}
	s.refresh(ctx)
	if calls != 1 {
		t.Fatal("cancelled context launched another process")
	}
}

func TestSlowSweepWaitsFullIntervalBeforeNextSweep(t *testing.T) {
	s := New([]string{"a"}, 0)
	s.every = 80 * time.Millisecond
	data := fixture(t)
	calls := make(chan int, 4)
	release := make(chan struct{})
	var n int
	s.run = func(ctx context.Context, _ string) ([]byte, error) {
		n++
		calls <- n
		if n == 2 {
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return data, nil
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.Start(ctx) }()
	defer func() { cancel(); <-done }()
	for expected := 1; expected <= 2; expected++ {
		select {
		case got := <-calls:
			if got != expected {
				t.Fatal(got)
			}
		case <-time.After(time.Second):
			t.Fatal("sweep did not start")
		}
	}
	// Let the second sweep overrun a polling interval. A ticker would have a
	// queued tick ready as soon as this sweep finishes.
	time.Sleep(2 * s.every)
	close(release)
	select {
	case <-calls:
		t.Fatal("slow sweep caused an immediate catch-up sweep")
	case <-time.After(s.every / 2):
	}
	select {
	case got := <-calls:
		if got != 3 {
			t.Fatal(got)
		}
	case <-time.After(time.Second):
		t.Fatal("next sweep never started")
	}
}

func TestCancelledDisabledCollectorDoesNotPublishFreshness(t *testing.T) {
	for _, paths := range [][]string{nil, {"off"}, {" "}} {
		s := New(paths, 0)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.refresh(ctx)
		if !s.Snapshot().GeneratedAt.IsZero() {
			t.Fatalf("cancelled disabled collector published freshness for %#v", paths)
		}
	}
}

func TestRateLimitClassification(t *testing.T) {
	now := time.Date(2026, 10, 23, 3, 0, 0, 0, time.UTC)
	original := errors.New("transport failure")
	for _, message := range []string{
		"gh: API rate limit exceeded", "GraphQL: API rate limit already exceeded",
		"secondary rate limit", "abuse detection mechanism", "HTTP 429", "rate_limit exceeded",
	} {
		var limited *rateLimitError
		if !errors.As(classifyStatusFailure([]byte(message), original, now), &limited) {
			t.Fatalf("not classified: %q", message)
		}
	}
	if got := classifyStatusFailure([]byte("HTTP 403 missing authentication token=SECRET"), original, now); got != original {
		t.Fatal(got)
	}
	err := classifyStatusFailure([]byte("rate limit exceeded token=SECRET\nX-RateLimit-Reset: 1792728000\nRetry-After: 7200"), original, now)
	var limited *rateLimitError
	if !errors.As(err, &limited) || !limited.until.Equal(now.Add(2*time.Hour)) || strings.Contains(err.Error(), "SECRET") {
		t.Fatal(err)
	}
}

func TestRateLimitedSweepRetainsDocumentsAndDelaysRetry(t *testing.T) {
	now := time.Date(2026, 10, 23, 3, 0, 0, 0, time.UTC)
	s := New([]string{"a", "b", "c"}, time.Minute)
	s.now = func() time.Time { return now }
	s.run = func(context.Context, string) ([]byte, error) { return fixture(t), nil }
	s.refresh(context.Background())
	initial := s.Snapshot()
	calls := 0
	s.run = func(context.Context, string) ([]byte, error) {
		calls++
		return nil, &rateLimitError{until: now.Add(time.Hour)}
	}
	s.refresh(context.Background())
	if calls != 1 {
		t.Fatal(calls)
	}
	for i, r := range s.Snapshot().Results {
		if r.Document != initial.Results[i].Document || !strings.Contains(r.Error, "rate limited") {
			t.Fatal(r)
		}
		if i > 0 && !strings.HasPrefix(r.Error, "skipped:") {
			t.Fatal(r)
		}
	}
	if s.nextDelay() != time.Hour {
		t.Fatal(s.nextDelay())
	}
	s.refresh(context.Background())
	if calls != 1 {
		t.Fatal("retried before reset")
	}
	now = now.Add(time.Hour)
	s.run = func(context.Context, string) ([]byte, error) { calls++; return fixture(t), nil }
	s.refresh(context.Background())
	if calls != 4 || s.Snapshot().Results[0].Error != "" {
		t.Fatal(calls, s.Snapshot())
	}
	if initial.Results[0].Error != "" {
		t.Fatal("published snapshot mutated")
	}
}

func TestUnknownRateResetWaitsNormalIntervalAndOtherErrorsContinue(t *testing.T) {
	now := time.Now()
	s := New([]string{"a", "b"}, 0)
	s.now = func() time.Time { return now }
	calls := 0
	s.run = func(context.Context, string) ([]byte, error) { calls++; return nil, &rateLimitError{} }
	s.refresh(context.Background())
	now = now.Add(s.every - time.Second)
	s.refresh(context.Background())
	if calls != 1 {
		t.Fatal(calls)
	}
	now = now.Add(time.Second)
	s.run = func(context.Context, string) ([]byte, error) { calls++; return nil, errors.New("missing gh-x") }
	s.refresh(context.Background())
	if calls != 3 {
		t.Fatal(calls)
	}
	for _, r := range s.Snapshot().Results {
		if !strings.Contains(r.Error, "installation") {
			t.Fatal(r)
		}
	}
	s.run = func(context.Context, string) ([]byte, error) { return []byte("invalid json"), nil }
	s.refresh(context.Background())
	if !strings.Contains(s.Snapshot().Results[0].Error, "invalid gh-x") {
		t.Fatal(s.Snapshot())
	}
}

func TestStderrIsBoundedAndDrained(t *testing.T) {
	var b stderrBuffer
	for range 10 {
		data := make([]byte, 32<<10)
		if n, err := b.Write(data); n != len(data) || err != nil {
			t.Fatal(n, err)
		}
	}
	if b.Len() != 32<<10 {
		t.Fatal(b.Len())
	}
}

// Hide the source's WriterTo so io.Copy uses the same destination method
// selection as os/exec's pipe reader. An embedded bytes.Buffer.ReadFrom
// bypasses a capped Write and makes this regression fail.
func TestSubprocessBuffersBoundedThroughCopy(t *testing.T) {
	var stderr stderrBuffer
	payload := strings.Repeat("x", 1<<20)
	n, err := io.Copy(&stderr, struct{ io.Reader }{strings.NewReader(payload)})
	if err != nil || n != int64(len(payload)) || stderr.Len() != 32<<10 {
		t.Fatal(n, err, stderr.Len())
	}
	var stdout limitedBuffer
	_, err = io.Copy(&stdout, struct{ io.Reader }{strings.NewReader(strings.Repeat(payload, 10))})
	if err == nil || stdout.Len() > 8<<20 {
		t.Fatal(err, stdout.Len())
	}
}

func TestRateLimitPathIsNotRateLimitFailure(t *testing.T) {
	original := errors.New("exit status 1")
	if got := classifyStatusFailure([]byte("checkout /tmp/flask_rate_limiter: missing extension"), original, time.Now()); got != original {
		t.Fatal(got)
	}
}

func TestSuccessfulProducerWithRateLimitedSectionsStopsSweep(t *testing.T) {
	now := time.Date(2026, 10, 23, 4, 0, 0, 0, time.UTC)
	s := New([]string{"a", "b", "c"}, 0)
	s.now = func() time.Time { return now }
	data := fixture(t)
	s.run = func(context.Context, string) ([]byte, error) { return data, nil }
	s.refresh(context.Background())
	initial := s.Snapshot()
	d, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	d.Sections[0].Status = "unavailable"
	d.Sections[0].Reason = "GitHub rate limit; x-ratelimit-reset: 1792731600"
	limitedJSON, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	s.run = func(context.Context, string) ([]byte, error) { calls++; return limitedJSON, nil }
	s.refresh(context.Background())
	if calls != 1 || !s.retryAt.Equal(now.Add(time.Hour)) {
		t.Fatal(calls, s.retryAt)
	}
	for i, r := range s.Snapshot().Results {
		if !strings.Contains(r.Error, "rate limited") || r.Document != initial.Results[i].Document {
			t.Fatal(r)
		}
	}
	s.refresh(context.Background())
	if calls != 1 {
		t.Fatal("retried before reset")
	}
	now = now.Add(time.Hour)
	s.run = func(context.Context, string) ([]byte, error) { calls++; return data, nil }
	s.refresh(context.Background())
	if calls != 4 || s.Snapshot().Results[0].Error != "" {
		t.Fatal(calls, s.Snapshot())
	}
}

func TestAvailableSectionsAndOtherFailuresKeepProducerBehavior(t *testing.T) {
	d, err := Parse(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	d.Sections[0].Reason = "rate limit mentioned in available metadata"
	if documentRateLimit(d, time.Now()) != nil {
		t.Fatal("classified an available section")
	}
	d.Sections[0].Status = "unavailable"
	d.Sections[0].Reason = "authentication failed"
	if documentRateLimit(d, time.Now()) != nil {
		t.Fatal("classified authentication failure")
	}
}

// Captured from the installed gh-x v0.23.2 binary in an isolated HOME with a
// rate-limited gh stub. gh-x exited 0 with empty stderr; see testdata provenance.
func TestCapturedProducerRateLimitStopsSweep(t *testing.T) {
	data, err := os.ReadFile("testdata/gh-x-v0.23.2-rate-limited.json")
	if err != nil {
		t.Fatal(err)
	}
	d, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	s := New([]string{"a", "b", "c"}, 0)
	s.now = func() time.Time { return d.GeneratedAt }
	calls := 0
	s.run = func(context.Context, string) ([]byte, error) { calls++; return data, nil }
	s.refresh(context.Background())
	if calls != 1 || !s.retryAt.After(d.GeneratedAt) {
		t.Fatal(calls, s.retryAt)
	}
	for _, r := range s.Snapshot().Results {
		if !strings.Contains(r.Error, "rate limited") {
			t.Fatal(r)
		}
	}
}

func TestRepeatedRateHeadersUseLatestDeadline(t *testing.T) {
	now := time.Date(2026, 10, 23, 4, 0, 0, 0, time.UTC)
	for _, message := range []string{
		"GitHub rate limit; X-RateLimit-Reset: 1792731600; X-RateLimit-Reset: 1792735200",
		"GitHub rate limit; Retry-After: 3600; Retry-After: 7200",
		"GitHub rate limit; X-RateLimit-Reset: 1792735200; Retry-After: 3600",
	} {
		var limited *rateLimitError
		if !errors.As(classifyStatusFailure([]byte(message), errors.New("failure"), now), &limited) || !limited.until.Equal(now.Add(2*time.Hour)) {
			t.Fatal(message, limited)
		}
	}
}

func TestSkippedResultsPreserveActualAttemptTimes(t *testing.T) {
	now := time.Now()
	s := New([]string{"a", "b", "c"}, 0)
	s.now = func() time.Time { return now }
	s.run = func(context.Context, string) ([]byte, error) { return fixture(t), nil }
	s.refresh(context.Background())
	previous := s.Snapshot()
	now = now.Add(time.Hour)
	s.run = func(context.Context, string) ([]byte, error) { return nil, &rateLimitError{} }
	s.refresh(context.Background())
	got := s.Snapshot()
	if !got.Results[0].AttemptedAt.Equal(now) {
		t.Fatal("attempted path missing actual time")
	}
	for i := 1; i < 3; i++ {
		if !got.Results[i].AttemptedAt.Equal(previous.Results[i].AttemptedAt) {
			t.Fatal("skip fabricated attempt", got.Results[i])
		}
	}
	fresh := New([]string{"a", "b"}, 0)
	fresh.run = s.run
	fresh.refresh(context.Background())
	if !fresh.Snapshot().Results[1].AttemptedAt.IsZero() {
		t.Fatal("never-attempted checkout has attempt")
	}
}

func TestOrdinaryRateLimitWordsDoNotTriggerBackoff(t *testing.T) {
	original := errors.New("failure")
	for _, message := range []string{"stat /tmp/rate limit/tests: missing directory", "config rate limits are invalid", "open /tmp/rate_limit_exceeded.txt: denied"} {
		if got := classifyStatusFailure([]byte(message), original, time.Now()); got != original {
			t.Fatal(message, got)
		}
	}
}
