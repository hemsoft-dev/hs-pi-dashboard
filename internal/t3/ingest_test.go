package t3

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"os"
	osexec "os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fixtureDB builds testdata/ingest/fixture.sql into a fresh database.
func fixtureDB(t *testing.T) string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("testdata", "ingest", "fixture.sql"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "statev2.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(string(script)); err != nil {
		t.Fatal(err)
	}
	return path
}

func buildFixture(t *testing.T, path string) []byte { // fixed clock
	t.Helper()
	at := time.Date(2026, 10, 27, 1, 2, 3, 456789000, time.FixedZone("EDT", -4*3600))
	snap, err := BuildIngestSnapshot(context.Background(), path, "", func() time.Time { return at })
	if err != nil {
		t.Fatal(err)
	}
	if snap.GeneratedAt != "2026-10-27T05:02:03.456Z" {
		t.Fatalf("generatedAt = %q, want UTC milliseconds", snap.GeneratedAt)
	}
	body, err := json.Marshal(snap)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

// comparable decodes a snapshot without generatedAt, keeping numbers exact.
func comparable(t *testing.T, body []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var value map[string]any
	if err := dec.Decode(&value); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	delete(value, "generatedAt")
	return value
}

func assertSameSnapshot(t *testing.T, got, want []byte) {
	t.Helper()
	if g, w := comparable(t, got), comparable(t, want); !reflect.DeepEqual(g, w) {
		pretty := func(v any) string { b, _ := json.MarshalIndent(v, "", " "); return string(b) }
		t.Fatalf("snapshot differs from the reference\n--- got\n%s\n--- want\n%s", pretty(g), pretty(w))
	}
}

// expected.json is scripts/t3/snapshot.py's output for fixture.sql.
func TestIngestSnapshotMatchesReferenceGolden(t *testing.T) {
	want, err := os.ReadFile(filepath.Join("testdata", "ingest", "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertSameSnapshot(t, buildFixture(t, fixtureDB(t)), want)
}

// Replays the reference sender itself against the same database when Python
// is available, so the golden file can't drift from snapshot.py.
func TestIngestSnapshotMatchesReferenceSender(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("snapshot.py's file: URI form is POSIX-only")
	}
	python, err := osexec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	path := fixtureDB(t)
	script, _ := filepath.Abs(filepath.Join("testdata", "ingest", "snapshot.py"))
	cmd := osexec.Command(python, "-I", script, path)
	cmd.Dir = t.TempDir()
	want, err := cmd.Output()
	if err != nil {
		t.Fatalf("snapshot.py: %v", err)
	}
	assertSameSnapshot(t, buildFixture(t, path), want)
}

func TestIngestSnapshotSendsNoQuestionsOrPayloads(t *testing.T) {
	body := buildFixture(t, fixtureDB(t))
	for _, secret := range []string{"secret prompt text", "options", "effort", "relationshipToParent", "scripts_json"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Errorf("snapshot carries %q", secret)
		}
	}
	var snap IngestSnapshot
	if err := json.Unmarshal(body, &snap); err != nil {
		t.Fatal(err)
	}
	if len(snap.Pending) != 3 {
		t.Fatalf("pending = %+v, want the running, unmatched and ordinal-free requests", snap.Pending)
	}
	for _, p := range snap.Pending {
		if p.Questions == nil || len(p.Questions) != 0 {
			t.Errorf("pending %v questions = %v, want []", p.RequestID, p.Questions)
		}
	}
	if !regexp.MustCompile(`"questions":\[\]`).Match(body) {
		t.Error(`questions must encode as []`)
	}
}

func TestIngestSnapshotEmptyDatabaseListsNothing(t *testing.T) {
	path := fixtureDB(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"orchestration_v2_projection_threads", "orchestration_v2_projection_runtime_requests"} {
		if _, err := db.Exec("DELETE FROM " + table); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec("DROP TABLE projection_thread_pull_requests"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	body := buildFixture(t, path)
	if !strings.Contains(string(body), `"threads":[],"pending":[]`) {
		t.Fatalf("empty snapshot = %s", body)
	}
}

func TestIngestSnapshotRefusesUnreadableSources(t *testing.T) {
	now := func() time.Time { return time.Now() }
	if _, err := BuildIngestSnapshot(context.Background(), filepath.Join(t.TempDir(), "missing.sqlite"), "", now); err == nil {
		t.Error("missing database: want an error")
	}
	path := fixtureDB(t)
	db, _ := sql.Open("sqlite", path)
	if _, err := db.Exec(`UPDATE orchestration_v2_projection_threads SET payload_json='[1]' WHERE thread_id='t-live'`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := BuildIngestSnapshot(context.Background(), path, "", now); err == nil || !strings.Contains(err.Error(), "t-live") {
		t.Errorf("non-object payload: err = %v, want one naming the thread", err)
	}
}

func TestTruthyFollowsPython(t *testing.T) {
	for _, tc := range []struct {
		value any
		want  bool
	}{
		{nil, false}, {false, false}, {true, true},
		{json.Number("0"), false}, {json.Number("0.0"), false}, {json.Number("2"), true},
		{"", false}, {"x", true},
		{[]any{}, false}, {[]any{1}, true},
		{map[string]any{}, false}, {map[string]any{"since": "x"}, true},
	} {
		if got := truthy(tc.value); got != tc.want {
			t.Errorf("truthy(%#v) = %v, want %v", tc.value, got, tc.want)
		}
	}
}

// HS_PI_INGEST_LIVE_DB=<statev2.sqlite> compares a real database read by both
// senders back to back (opt-in; a busy machine may change in between).
func TestIngestSnapshotMatchesReferenceOnLiveDatabase(t *testing.T) {
	path := os.Getenv("HS_PI_INGEST_LIVE_DB")
	if path == "" {
		t.Skip("set HS_PI_INGEST_LIVE_DB to a T3 statev2.sqlite")
	}
	python, err := osexec.LookPath("python3")
	if err != nil {
		t.Skip("python3 not installed")
	}
	script, _ := filepath.Abs(filepath.Join("testdata", "ingest", "snapshot.py"))
	started := time.Now()
	got := buildFixture(t, path)
	built := time.Since(started)
	cmd := osexec.Command(python, "-I", script, path)
	cmd.Dir = t.TempDir()
	want, err := cmd.Output()
	if err != nil {
		t.Fatalf("snapshot.py: %v", err)
	}
	var snap IngestSnapshot
	_ = json.Unmarshal(got, &snap)
	t.Logf("%d threads, %d pending, %d bytes, built in %v", len(snap.Threads), len(snap.Pending), len(got), built)
	assertSameSnapshot(t, got, want)
}
