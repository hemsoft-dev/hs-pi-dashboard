package t3

import (
	"strings"
	"testing"
	"time"
)

func TestLateIdentityReadSharesRemainingRuntimeBudget(t *testing.T) {
	db, c, now := v2Fixture(t)
	v2Thread(t, db, "known", now)
	f, _ := attachRuntime(t, c)
	f.v2 = true
	f.threads = []liveThread{{ID: "known", Status: "running"}}
	if got := c.Poll(); got.ActiveSessions != 1 {
		t.Fatal(got)
	}
	c.runtimeTimeout = 350 * time.Millisecond
	f.threads = append(f.threads, liveThread{ID: "late", Status: "running"})
	released := make(chan error, 1)
	f.beforeShell = func() {
		time.Sleep(200 * time.Millisecond)
		exec(t, db, "BEGIN EXCLUSIVE")
		go func() { time.Sleep(400 * time.Millisecond); _, err := db.Exec("ROLLBACK"); released <- err }()
	}
	got := c.Poll()
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if got.ActiveSessions != 1 || got.Sources[0].State != "healthy" || !strings.Contains(got.Sources[0].Error, "provider identities read deadline") {
		t.Fatalf("identity escaped shared deadline: %+v", got)
	}
}

func TestDeadlineCannotRetainIdleAsDispatchCapacity(t *testing.T) {
	db, c, now := v2Fixture(t)
	if c.Poll().Sources[0].State != "healthy" {
		t.Fatal("initial snapshot")
	}
	exec(t, db, "BEGIN EXCLUSIVE")
	defer exec(t, db, "ROLLBACK")
	c.readTimeout = 20 * time.Millisecond
	c.now = func() time.Time { return now.Add(time.Minute) }
	got := c.Poll()
	if got.Sources[0].State != "unavailable" || got.ActiveSessions != 0 || !strings.Contains(got.Sources[0].Error, "deadline") {
		t.Fatalf("cached idle must be unknown: %+v", got)
	}
}

func TestProjectionDeadlineRetainsStateAndExpires(t *testing.T) {
	db, c, now := v2Fixture(t)
	v2Thread(t, db, "deadline", now)
	good := c.Poll()
	if good.Sources[0].State != "healthy" {
		t.Fatal(good.Sources)
	}
	c.last.ActiveSessions = 1
	c.last.Sessions[0].Active = true
	c.last.Sessions[0].Status = "working"
	exec(t, db, "BEGIN EXCLUSIVE")
	defer exec(t, db, "ROLLBACK")
	c.readTimeout = 20 * time.Millisecond
	c.now = func() time.Time { return now.Add(time.Minute) }
	got := c.Poll()
	if got.Sources[0].State != "healthy" || got.ActiveSessions != 1 || !got.GeneratedAt.Equal(good.GeneratedAt) || !strings.Contains(got.Sources[0].Error, "deadline") || strings.Contains(got.Sources[0].Error, "schema") {
		t.Fatalf("deadline cache: %+v", got)
	}
	c.now = func() time.Time { return now.Add(5*time.Minute + time.Nanosecond) }
	got = c.Poll()
	if got.Sources[0].State != "unavailable" || got.ActiveSessions != 0 || !strings.Contains(got.Sources[0].Error, "deadline") || strings.Contains(got.Sources[0].Error, "schema") {
		t.Fatalf("deadline expiry: %+v", got)
	}
}
func TestRuntimeReadHasIndependentDeadline(t *testing.T) {
	db, c, now := v2Fixture(t)
	v2Thread(t, db, "budget", now)
	f, _ := attachRuntime(t, c)
	f.v2 = true
	f.threads = []liveThread{{ID: "budget", Status: "running"}}
	if got := c.Poll(); got.Sources[0].State != "healthy" {
		t.Fatal(got.Sources)
	}
	c.readTimeout = 150 * time.Millisecond
	f.beforeShell = func() { time.Sleep(200 * time.Millisecond) }
	if got := c.Poll(); got.Sources[0].State != "healthy" || got.ActiveSessions != 1 {
		t.Fatalf("runtime budget: %+v", got)
	}
}
