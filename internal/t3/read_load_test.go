package t3

import (
	"context"
	"database/sql"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Opt-in production-sized diagnostic. The reader always uses mode=ro.
// HS_T3_BENCH_WRITE_COPY=1 permits a writer only on the explicitly named copy,
// never on the live desktop/server database. No prompts or payloads are logged.
func TestProductionSizedReadUnderWriteLoad(t *testing.T) {
	path := os.Getenv("HS_T3_BENCH_DB")
	if path == "" {
		t.Skip("set HS_T3_BENCH_DB to a production-sized SQLite copy")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 2_000_000_000 {
		t.Fatal("diagnostic requires at least a 2 GB database")
	}
	t.Logf("database_bytes=%d", info.Size())
	dsn := "file:" + strings.ReplaceAll(url.QueryEscape(path), "+", "%20")
	db, err := sql.Open("sqlite", dsn+"?mode=ro&_pragma=busy_timeout(500)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var commits atomic.Int64
	if os.Getenv("HS_T3_BENCH_WRITE_COPY") == "1" {
		if filepath.Base(path) != "t3-home-load-copy.sqlite" {
			t.Fatal("writer requires the explicitly named benchmark copy")
		}
		home, _ := os.UserHomeDir()
		for _, base := range []string{".t3", ".t3-server"} {
			live, err := os.Stat(filepath.Join(home, base, "userdata", "statev2.sqlite"))
			if err == nil && os.SameFile(info, live) {
				t.Fatal("refusing writes to the live database")
			}
		}
		writer, err := sql.Open("sqlite", dsn+"?mode=rw&_pragma=busy_timeout(100)&_pragma=journal_mode(WAL)")
		if err != nil {
			t.Fatal(err)
		}
		if _, err = writer.Exec("CREATE TABLE IF NOT EXISTS __hs_dashboard_bench_load(id INTEGER PRIMARY KEY,n INTEGER,payload BLOB)"); err != nil {
			writer.Close()
			t.Fatal(err)
		}
		if _, err = writer.Exec("INSERT OR IGNORE INTO __hs_dashboard_bench_load VALUES(1,0,zeroblob(16384))"); err != nil {
			writer.Close()
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			defer writer.Close()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					if _, err := writer.ExecContext(ctx, "UPDATE __hs_dashboard_bench_load SET n=n+1,payload=randomblob(16384) WHERE id=1"); err == nil {
						commits.Add(1)
					}
				}
			}
		}()
		defer func() {
			cancel()
			<-done
			t.Logf("write_commits=%d", commits.Load())
			if commits.Load() == 0 {
				t.Error("writer made no progress")
			}
		}()
	}
	var expected int
	if err := db.QueryRow("SELECT COUNT(*) FROM orchestration_v2_projection_threads WHERE deleted_at IS NULL").Scan(&expected); err != nil {
		t.Fatal(err)
	}
	// Restore the previous full-history plan for an exact before/after comparison.
	before := strings.ReplaceAll(v2ThreadsQuery, "WHERE 0 AND ", "WHERE ")
	for round := range 3 {
		for _, variant := range []struct{ name, query string }{{"before", before}, {"after", v2ThreadsQuery}} {
			started := time.Now()
			ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
			count, err := countProjectionRows(ctx, db, variant.query)
			cancel()
			t.Logf("round=%d variant=%s rows=%d read_ms=%d error=%v", round, variant.name, count, time.Since(started).Milliseconds(), err)
			if variant.name == "after" && (err != nil || count != expected) {
				t.Fatalf("metadata read failed: rows=%d expected=%d error=%v", count, expected, err)
			}
		}
	}
}

func countProjectionRows(ctx context.Context, db *sql.DB, query string) (int, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	values := make([]any, len(columns))
	for i := range values {
		values[i] = new(any)
	}
	count := 0
	for rows.Next() {
		if err := rows.Scan(values...); err != nil {
			return count, err
		}
		count++
	}
	return count, rows.Err()
}
