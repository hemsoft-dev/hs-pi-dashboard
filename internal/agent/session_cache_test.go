package agent

import (
	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
	"github.com/HemSoft/hs-pi-dashboard/internal/t3"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSessionCacheExpiryPreservesOtherT3Environment(t *testing.T) {
	now := time.Now()
	oldPath := "/desktop/statev2.sqlite"
	freshPath := "/server/statev2.sqlite"
	original := sessions.Snapshot{GeneratedAt: now, ActiveSessions: 2,
		Sources: []sessions.SourceHealth{
			{Source: "t3", Path: oldPath, State: "healthy", LastSuccessAt: now.Add(-6 * time.Minute)},
			{Source: "t3", Path: freshPath, State: "healthy", LastSuccessAt: now},
		}, Sessions: []sessions.Summary{
			{ID: t3.SessionPrefix(oldPath) + "old", Source: "t3", Active: true, Status: "working"},
			{ID: t3.SessionPrefix(freshPath) + "fresh", Source: "t3", Active: true, Status: "working"},
		},
	}
	cache := &sessionCache{now: func() time.Time { return now }, running: true, last: original}
	got := cache.snapshot()
	if got.ActiveSessions != 1 || got.Sessions[0].Active || got.Sessions[0].Status != "unavailable" || !got.Sessions[1].Active || got.Sessions[1].Status != "working" || got.Sources[1].State != "healthy" {
		t.Fatalf("cross-source expiry: %+v", got)
	}
	if original.ActiveSessions != 2 || !original.Sessions[0].Active {
		t.Fatal("mutated stored source")
	}
}

func TestSessionCacheCoalescesSlowRequestsAndWaitsAfterCompletion(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	defer close(release)
	var calls atomic.Int32
	cache := &sessionCache{now: time.Now, poll: func() sessions.Snapshot {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return sessions.Snapshot{Machine: "complete"}
	}}
	var workers sync.WaitGroup
	for range 32 {
		workers.Add(1)
		go func() { defer workers.Done(); cache.snapshot() }()
	}
	workers.Wait()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("refresh did not start")
	}
	if calls.Load() != 1 {
		t.Fatalf("duplicate reads: %d", calls.Load())
	}
	// A read that takes longer than the cadence must still remain single flight.
	cache.mu.Lock()
	cache.completed = time.Now().Add(-2 * sessionInterval)
	cache.mu.Unlock()
	cache.snapshot()
	if calls.Load() != 1 {
		t.Fatal("slow refresh was superseded")
	}
	release <- struct{}{}
	deadline := time.Now().Add(time.Second)
	for {
		cache.mu.Lock()
		running := cache.running
		cache.mu.Unlock()
		if !running {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("refresh did not complete")
		}
		time.Sleep(time.Millisecond)
	}
	if got := cache.snapshot(); got.Machine != "complete" || calls.Load() != 1 {
		t.Fatalf("completed snapshot: %+v", got)
	}
	cache.mu.Lock()
	cache.completed = time.Now().Add(-sessionInterval)
	cache.mu.Unlock()
	cache.snapshot()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("next refresh did not start")
	}
	if calls.Load() != 2 {
		t.Fatal("cadence did not resume")
	}
}

func TestSessionCacheExpiredT3CannotImplyIdleOrFreshness(t *testing.T) {
	now := time.Now()
	original := sessions.Snapshot{GeneratedAt: now.Add(-6 * time.Minute), ActiveSessions: 1,
		Sources:  []sessions.SourceHealth{{Source: "t3", State: "healthy", LastSuccessAt: now.Add(-6 * time.Minute)}},
		Sessions: []sessions.Summary{{Source: "t3", Status: "working", Active: true}},
	}
	cache := &sessionCache{now: func() time.Time { return now }, running: true, last: original}
	got := cache.snapshot()
	if got.Sources[0].State != "unavailable" || got.ActiveSessions != 0 || got.Sessions[0].Status != "unavailable" || !got.GeneratedAt.Equal(original.GeneratedAt) {
		t.Fatalf("expiry: %+v", got)
	}
	if !original.Sessions[0].Active || original.Sources[0].State != "healthy" {
		t.Fatal("mutated stored snapshot")
	}
}

// testClock is a settable clock that the refresh goroutine can read safely.
type testClock struct{ nanos atomic.Int64 }

func (c *testClock) now() time.Time   { return time.Unix(0, c.nanos.Load()).UTC() }
func (c *testClock) set(at time.Time) { c.nanos.Store(at.UnixNano()) }

// A refresh starts exactly when a full interval has passed since the last one
// completed, never earlier. The decision is made synchronously under the lock,
// so running is the observable result of each request.
func TestSessionCacheRefreshStartsExactlyAtTheInterval(t *testing.T) {
	completed := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	clock := &testClock{}
	release := make(chan struct{})
	var calls atomic.Int32
	cache := &sessionCache{now: clock.now, completed: completed, poll: func() sessions.Snapshot {
		calls.Add(1)
		<-release
		return sessions.Snapshot{Machine: "refreshed"}
	}}
	running := func() bool {
		cache.mu.Lock()
		defer cache.mu.Unlock()
		return cache.running
	}
	t.Cleanup(func() {
		close(release)
		for deadline := time.Now().Add(time.Second); running(); time.Sleep(time.Millisecond) {
			if time.Now().After(deadline) {
				t.Error("refresh did not complete")
				return
			}
		}
	})

	for _, early := range []time.Duration{0, time.Second, sessionInterval - time.Nanosecond} {
		clock.set(completed.Add(early))
		cache.snapshot()
		if running() {
			t.Fatalf("refresh started %v after the last one completed", early)
		}
	}
	clock.set(completed.Add(sessionInterval))
	cache.snapshot()
	if !running() {
		t.Fatal("refresh did not start once the interval had passed")
	}
	for deadline := time.Now().Add(time.Second); calls.Load() == 0; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("poll was not called")
		}
	}
}

// T3 data is fresh through exactly five minutes after its last success and
// expired one nanosecond later.
func TestSessionCacheT3ExpiryBoundaryIsFiveMinutes(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	path := "/server/statev2.sqlite"
	for _, tc := range []struct {
		age     time.Duration
		expired bool
	}{
		{0, false},
		{5 * time.Minute, false},
		{5*time.Minute + time.Nanosecond, true},
	} {
		original := sessions.Snapshot{ActiveSessions: 1,
			Sources:  []sessions.SourceHealth{{Source: "t3", Path: path, State: "healthy", LastSuccessAt: now.Add(-tc.age)}},
			Sessions: []sessions.Summary{{ID: t3.SessionPrefix(path) + "row", Source: "t3", Active: true, Status: "working"}},
		}
		cache := &sessionCache{now: func() time.Time { return now }, running: true, last: original}
		got := cache.snapshot()
		source, row := got.Sources[0], got.Sessions[0]
		if tc.expired {
			if source.State != "unavailable" || source.Error == "" || row.Active || row.Status != "unavailable" || got.ActiveSessions != 0 {
				t.Errorf("age %v: still fresh: %+v", tc.age, got)
			}
		} else if source.State != "healthy" || source.Error != "" || !row.Active || row.Status != "working" || got.ActiveSessions != 1 {
			t.Errorf("age %v: expired early: %+v", tc.age, got)
		}
	}
}
