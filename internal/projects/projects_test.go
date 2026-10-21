package projects

import (
	"context"
	"errors"
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
