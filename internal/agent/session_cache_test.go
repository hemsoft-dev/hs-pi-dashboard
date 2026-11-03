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
