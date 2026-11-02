package t3

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestV2BusyRetainsSnapshotUntilExpiry(t *testing.T) {
	db, c, now := v2Fixture(t)
	v2Thread(t, db, "busy", now)
	good := c.Poll()
	if good.Sources[0].State != "healthy" {
		t.Fatal(good.Sources)
	}
	// Preserve a live state too; SQLite fixtures deliberately disable runtime.
	c.last.Sessions[0].Active = true
	c.last.Sessions[0].Status = "working"
	c.last.ActiveSessions = 1
	exec(t, db, "BEGIN EXCLUSIVE")
	defer exec(t, db, "ROLLBACK")
	c.now = func() time.Time { return now.Add(time.Minute) }
	got := c.Poll()
	if got.Sources[0].State != "healthy" || got.ActiveSessions != 1 || got.Sessions[0].Status != "working" || !got.GeneratedAt.Equal(good.GeneratedAt) || !got.Sources[0].LastSuccessAt.Equal(now) || !strings.Contains(got.Sources[0].Error, "busy") {
		t.Fatalf("cached snapshot: %+v", got)
	}
	c.now = func() time.Time { return now.Add(5*time.Minute + time.Nanosecond) }
	got = c.Poll()
	if got.Sources[0].State != "unavailable" || got.ActiveSessions != 0 || got.Sessions[0].Status != "unavailable" {
		t.Fatalf("expired snapshot: %+v", got)
	}
}

func TestV2BriefWriteLockRecovers(t *testing.T) {
	db, c, _ := v2Fixture(t)
	exec(t, db, "BEGIN EXCLUSIVE")
	released := make(chan error, 1)
	go func() { time.Sleep(650 * time.Millisecond); _, err := db.Exec("ROLLBACK"); released <- err }()
	started := time.Now()
	got := c.Poll()
	if err := <-released; err != nil {
		t.Fatal(err)
	}
	if got.Sources[0].State != "healthy" || time.Since(started) < 600*time.Millisecond {
		t.Fatalf("brief lock: %+v", got.Sources)
	}
}

func TestV2SchemaFailureDoesNotReuseSnapshot(t *testing.T) {
	db, c, _ := v2Fixture(t)
	c.Poll()
	exec(t, db, "DROP TABLE orchestration_v2_projection_nodes")
	got := c.Poll()
	if got.Sources[0].State != "unavailable" || !strings.Contains(got.Sources[0].Error, "incompatible schema") || strings.Contains(got.Sources[0].Error, "busy") {
		t.Fatal(got.Sources)
	}
	// A later lock must not resurrect the snapshot rejected by schema validation.
	exec(t, db, "BEGIN EXCLUSIVE")
	defer exec(t, db, "ROLLBACK")
	got = c.Poll()
	if got.Sources[0].State != "unavailable" {
		t.Fatal(got.Sources)
	}
}

func TestV2BusyWithoutSnapshotIsUnavailable(t *testing.T) {
	db, c, _ := v2Fixture(t)
	exec(t, db, "BEGIN EXCLUSIVE")
	defer exec(t, db, "ROLLBACK")
	got := c.Poll()
	if got.Sources[0].State != "unavailable" || !strings.Contains(got.Sources[0].Error, "busy") || strings.Contains(got.Sources[0].Error, "schema") {
		t.Fatal(got.Sources)
	}
}

func TestV2ReadReleasesDatabaseBeforeRuntime(t *testing.T) {
	db, c, now := v2Fixture(t)
	v2Thread(t, db, "release", now)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := c.readV2Threads(ctx, db); err != nil {
		t.Fatal(err)
	}
	exec(t, db, "BEGIN EXCLUSIVE")
	exec(t, db, "ROLLBACK")
}

func TestV2IdentityWriteLockAfterProjection(t *testing.T) {
	for _, brief := range []bool{false, true} {
		t.Run(map[bool]string{false: "retained", true: "retry"}[brief], func(t *testing.T) {
			db, c, now := v2Fixture(t)
			v2Thread(t, db, "known", now)
			f, _ := attachRuntime(t, c)
			f.v2 = true
			f.threads = []liveThread{{ID: "known", Status: "running"}}
			good := c.Poll()
			if good.Sources[0].State != "healthy" || good.ActiveSessions != 1 {
				t.Fatal(good)
			}
			f.threads = append(f.threads, liveThread{ID: "new", Status: "running"})
			released := make(chan error, 1)
			f.beforeShell = func() {
				exec(t, db, "BEGIN EXCLUSIVE")
				if brief {
					go func() { time.Sleep(650 * time.Millisecond); _, err := db.Exec("ROLLBACK"); released <- err }()
				}
			}
			c.now = func() time.Time { return now.Add(time.Minute) }
			got := c.Poll()
			if brief {
				if err := <-released; err != nil {
					t.Fatal(err)
				}
				if got.Sources[0].State != "healthy" || got.ActiveSessions != 2 || got.Sources[0].Error != "" {
					t.Fatalf("identity retry: %+v", got)
				}
			} else {
				exec(t, db, "ROLLBACK")
				if got.Sources[0].State != "healthy" || got.ActiveSessions != 1 || !got.GeneratedAt.Equal(good.GeneratedAt) || !strings.Contains(got.Sources[0].Error, "busy") {
					t.Fatalf("identity cache: %+v", got)
				}
				if recovered := c.Poll(); recovered.ActiveSessions != 2 || recovered.Sources[0].Error != "" {
					t.Fatalf("identity recovery: %+v", recovered)
				}
			}
		})
	}
}
