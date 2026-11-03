package t3

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestColdRuntimeTimeoutPreservesHistoryCadence(t *testing.T) {
	db, c, now := v2Fixture(t)
	v2Thread(t, db, "one", now)
	f, _ := attachRuntime(t, c)
	f.v2 = true
	f.threads = []liveThread{{ID: "one", Status: "running"}}
	c.runtimeTimeout = time.Nanosecond
	if got := c.Poll(); got.Sources[0].State != "unavailable" || c.file != nil {
		t.Fatalf("cold timeout: %+v", got)
	}
	if c.historyFile == nil || c.history["one"].messages != 2 || !c.historyAt.Equal(now) {
		t.Fatal("initial counter refresh missing")
	}
	exec(t, db, "INSERT INTO orchestration_v2_projection_messages VALUES('one','user','{}')")
	c.now = func() time.Time { return now.Add(time.Second) }
	if got := c.Poll(); got.Sources[0].State != "unavailable" || c.file != nil {
		t.Fatalf("repeated timeout: %+v", got)
	}
	if c.history["one"].messages != 2 || !c.historyAt.Equal(now) || !c.historyAttempt.Equal(now) {
		t.Fatal("runtime failure reset history throttle")
	}
	c.now = func() time.Time { return now.Add(historyInterval) }
	c.Poll()
	if c.history["one"].messages != 3 || !c.historyAt.Equal(now.Add(historyInterval)) {
		t.Fatal("history cadence did not resume")
	}
}

func TestHistoryIdentityResetsWhenDatabaseIsReplaced(t *testing.T) {
	db, c, now := v2Fixture(t)
	v2Thread(t, db, "one", now)
	if got := c.Poll(); got.Sessions[0].MessageCount != 2 {
		t.Fatal(got)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(c.path, c.path+".replaced"); err != nil {
		t.Fatal(err)
	}
	replacement := createDB(t, c.path)
	exec(t, replacement, v2Schema)
	v2Thread(t, replacement, "one", now)
	exec(t, replacement, "INSERT INTO orchestration_v2_projection_messages VALUES('one','user','{}'),('one','assistant','{}')")
	c.now = func() time.Time { return now.Add(time.Second) }
	got := c.Poll()
	if got.Sources[0].State != "healthy" || got.Sessions[0].MessageCount != 4 || !got.Sessions[0].T3CountsAt.Equal(now.Add(time.Second)) {
		t.Fatalf("replacement reused counters: %+v", got)
	}
}

type historyFailure struct {
	projectionReader
	calls int
	err   error
}

func (r *historyFailure) QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error) {
	if q == v2HistoryQuery {
		r.calls++
		if r.err != nil {
			return nil, r.err
		}
		return nil, context.DeadlineExceeded
	}
	return r.projectionReader.QueryContext(ctx, q, args...)
}

func TestHistoryScanFailureCannotInvalidateMetadata(t *testing.T) {
	db, c, now := v2Fixture(t)
	v2Thread(t, db, "one", now)
	failed := &historyFailure{projectionReader: db, err: errors.New("counter scan conversion failed")}
	rows, err := c.readV2Threads(context.Background(), failed)
	if err != nil || len(rows) != 1 || rows[0].Name != "V2 one" || rows[0].T3CountsError == "" || rows[0].T3CountsAt != nil {
		t.Fatalf("optional scan failed liveness: %+v %v", rows, err)
	}
}

func TestHistoryCountersHaveIndependentCadenceAndAge(t *testing.T) {
	db, c, now := v2Fixture(t)
	v2Thread(t, db, "one", now)
	read := func() int {
		t.Helper()
		rows, err := c.readV2Threads(context.Background(), db)
		if err != nil {
			t.Fatal(err)
		}
		if rows[0].T3CountsAt == nil {
			t.Fatal("missing counter timestamp")
		}
		return rows[0].MessageCount
	}
	if read() != 2 {
		t.Fatal("initial counters")
	}
	exec(t, db, "INSERT INTO orchestration_v2_projection_messages VALUES('one','user','{}')")
	if read() != 2 {
		t.Fatal("history scanned again before cadence")
	}
	c.now = func() time.Time { return now.Add(historyInterval) }
	if read() != 3 {
		t.Fatal("counters did not refresh")
	}
	c.now = func() time.Time { return now.Add(2 * historyInterval) }
	failed := &historyFailure{projectionReader: db}
	rows, err := c.readV2Threads(context.Background(), failed)
	if err != nil || rows[0].MessageCount != 3 || !rows[0].T3CountsAt.Equal(now.Add(historyInterval)) || !strings.Contains(rows[0].T3CountsError, "deadline") {
		t.Fatalf("history timeout: %+v %v", rows, err)
	}
	if failed.calls != 1 {
		t.Fatal("unexpected history attempts")
	}
}

func TestColdHistoryDeadlineDoesNotDropMetadata(t *testing.T) {
	db, c, now := v2Fixture(t)
	v2Thread(t, db, "one", now)
	failed := &historyFailure{projectionReader: db}
	rows, err := c.readV2Threads(context.Background(), failed)
	if err != nil || len(rows) != 1 || rows[0].Name != "V2 one" || rows[0].T3CountsAt != nil || !strings.Contains(rows[0].T3CountsError, "deadline") {
		t.Fatalf("metadata must survive: %+v %v", rows, err)
	}
}
