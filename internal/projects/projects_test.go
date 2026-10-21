package projects

import (
	"context"
	"errors"
	"os"
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
	if maxLive.Load() > 3 || len(initial.Results) != 5 || initial.Results[0].Document == nil {
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
