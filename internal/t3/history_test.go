package t3

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"
)

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
