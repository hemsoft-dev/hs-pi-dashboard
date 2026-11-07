package t3

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"testing"
	"time"
)

// The settled rule of project-manager#115 and chief#205, checked through
// BuildIngestSnapshot on threads added to the parity fixture.

type settledRun struct {
	status    string
	requested string
	completed any // string or nil
}

type settledCase struct {
	id      string
	payload string
	runs    []settledRun // ordinal = index
	want    bool
}

// done is one completed run requested an hour before it finished.
func done(completed string) []settledRun {
	return []settledRun{{"completed", "2026-11-06T11:00:00.000Z", completed}}
}

// done2 is one run with the given status that ended at latestZ.
func done2(status string) []settledRun {
	return []settledRun{{status, "2026-11-06T11:00:00.000Z", latestZ}}
}

const latestZ = "2026-11-06T12:00:00.000Z"

func settledAt(at string) string {
	b, _ := json.Marshal(map[string]any{"settledOverride": "settled", "settledAt": at})
	return string(b)
}

var settledCases = []settledCase{
	// Instants in Z.
	{"after-z", settledAt("2026-11-06T12:30:00.000Z"), done(latestZ), true},
	{"equal-z", settledAt(latestZ), done(latestZ), true},
	{"before-z", settledAt("2026-11-06T11:30:00.000Z"), done(latestZ), false},
	// Offsets on either side, and equal instants written differently.
	{"offset-after", settledAt("2026-11-06T08:30:00-04:00"), done(latestZ), true},
	{"offset-before", settledAt("2026-11-06T13:00:00+02:00"), done(latestZ), false},
	{"latest-offset-before", settledAt("2026-11-06T11:30:00.000Z"),
		[]settledRun{{"completed", "2026-11-06T07:00:00-04:00", "2026-11-06T08:00:00-04:00"}}, false},
	{"equal-across-offsets", settledAt("2026-11-06T08:00:00-04:00"), done("2026-11-06T12:00:00Z"), true},
	{"equal-across-offsets-fraction", settledAt("2026-11-06T13:30:00.250+01:30"), done("2026-11-06T12:00:00.25Z"), true},
	{"lowercase-t-z", settledAt("2026-11-06t12:30:00z"), done(latestZ), true},
	// Precision: every fractional digit counts, past nanoseconds too.
	{"precision-after", settledAt("2026-11-06T12:00:00.500Z"), done("2026-11-06T12:00:00Z"), true},
	{"trailing-zeros-equal", settledAt("2026-11-06T12:00:00.5000000000000Z"), done("2026-11-06T12:00:00.5Z"), true},
	{"beyond-nanoseconds-after", settledAt("2026-11-06T12:00:00.0000000001Z"), done("2026-11-06T12:00:00.000000000Z"), true},
	{"beyond-nanoseconds-before", settledAt("2026-11-06T12:00:00.0000000001Z"), done("2026-11-06T12:00:00.0000000002Z"), false},
	{"beyond-nanoseconds-before-offset", settledAt("2026-11-06T07:59:59.9999999999999-04:00"), done("2026-11-06T12:00:00Z"), false},
	{"beyond-nanoseconds-equal-offset", settledAt("2026-11-06T08:00:00.1000000000001-04:00"), done("2026-11-06T12:00:00.1000000000001Z"), true},
	// The 10- and 11-digit latest times of project-manager#116's edge golden.
	{"ten-digit-latest-after", settledAt("2026-11-06T20:16:35.0000000002Z"),
		[]settledRun{{"failed", "2026-11-06T20:00:00Z", "2026-11-06T20:16:35.0000000001Z"}}, true},
	{"eleven-digit-latest-before", settledAt("2026-11-06T20:16:35.0000000001Z"),
		[]settledRun{{"failed", "2026-11-06T20:00:00Z", "2026-11-06T20:16:35.00000000019Z"}}, false},
	{"half-second-equal", settledAt("2026-11-06T20:16:35.500Z"),
		[]settledRun{{"interrupted", "2026-11-06T20:00:00Z", "2026-11-06T20:16:35.5Z"}}, true},
	// settledAt that isn't a zoned RFC 3339 date-time.
	{"missing-at", `{"settledOverride":"settled"}`, done(latestZ), false},
	{"null-at", `{"settledOverride":"settled","settledAt":null}`, done(latestZ), false},
	{"empty-at", settledAt(""), done(latestZ), false},
	{"number-at", `{"settledOverride":"settled","settledAt":1793102400}`, done(latestZ), false},
	{"bool-at", `{"settledOverride":"settled","settledAt":true}`, done(latestZ), false},
	{"invalid-at", settledAt("not-a-time"), done(latestZ), false},
	{"naive-at", settledAt("2026-11-06T12:30:00"), done(latestZ), false},
	{"naive-fraction-at", settledAt("2026-11-06T12:30:00.000"), done(latestZ), false},
	{"space-separator", settledAt("2026-11-06 12:30:00Z"), done(latestZ), false},
	{"offset-24-hours", settledAt("2026-11-06T12:30:00+24:00"), done(latestZ), false},
	{"offset-60-minutes", settledAt("2026-11-06T12:30:00+04:60"), done(latestZ), false},
	{"offset-no-colon", settledAt("2026-11-06T12:30:00+0400"), done(latestZ), false},
	{"offset-hours-only", settledAt("2026-11-06T12:30:00+04"), done(latestZ), false},
	{"offset-name", settledAt("2026-11-06T12:30:00 UTC"), done(latestZ), false},
	{"empty-fraction", settledAt("2026-11-06T12:30:00.Z"), done(latestZ), false},
	{"leap-second", settledAt("2026-11-06T23:59:60Z"), done(latestZ), false},
	{"no-such-day", settledAt("2026-11-31T12:30:00Z"), done(latestZ), false},
	{"fullwidth-digits", settledAt("２０２６-11-06T12:30:00Z"), done(latestZ), false},
	{"trailing-newline", settledAt("2026-11-06T12:30:00Z\n"), done(latestZ), false},
	{"utc-past-year-9999", settledAt("9999-12-31T23:59:59-00:01"), done(latestZ), false},
	// Only the literal override counts.
	{"other-override", `{"settledOverride":"unsettled","settledAt":"2026-11-06T12:30:00.000Z"}`, done(latestZ), false},
	{"override-case", `{"settledOverride":"Settled","settledAt":"2026-11-06T12:30:00.000Z"}`, done(latestZ), false},
	{"no-override", `{"settledAt":"2026-11-06T12:30:00.000Z"}`, done(latestZ), false},
	// Settlement reads the latest run's coalesce(completed_at, requested_at), which
	// must be valid. An empty completed_at fails closed, though latestRun.at falls
	// back to requested_at.
	{"latest-at-invalid", settledAt("2026-11-06T12:30:00.000Z"), done("garbage"), false},
	{"latest-at-naive", settledAt("2026-11-06T12:30:00.000Z"),
		[]settledRun{{"completed", "2026-11-06T12:00:00", nil}}, false},
	{"latest-at-before-year-1", settledAt("2026-11-06T12:30:00.000Z"), done("0001-01-01T00:00:00+00:01"), false},
	{"latest-requested-when-completed-null", settledAt("2026-11-06T12:30:00.000Z"),
		[]settledRun{{"interrupted", latestZ, nil}}, true},
	{"latest-completed-empty-fails-closed", settledAt("2026-11-06T12:30:00.000Z"),
		[]settledRun{{"failed", latestZ, ""}}, false},
	{"latest-requested-invalid-completed-empty", settledAt("2026-11-06T12:30:00.000Z"),
		[]settledRun{{"failed", "garbage", ""}}, false},
	{"latest-completed-invalid-requested-valid", settledAt("2026-11-06T12:30:00.000Z"),
		[]settledRun{{"cancelled", latestZ, "garbage"}}, false},
	// The latest run is the highest ordinal, not the latest timestamp.
	{"latest-is-highest-ordinal", settledAt("2026-11-06T12:30:00.000Z"),
		[]settledRun{{"completed", "2026-11-06T12:40:00.000Z", "2026-11-06T13:00:00.000Z"}, {"completed", "2026-11-06T11:00:00.000Z", latestZ}}, true},
	// Only a finished latest run can be settled after; pmd checks that again.
	{"failed-latest", settledAt(latestZ), done2("failed"), true},
	{"interrupted-latest", settledAt(latestZ), done2("interrupted"), true},
	{"cancelled-latest", settledAt(latestZ), done2("cancelled"), true},
	{"running-latest", settledAt("2026-11-06T12:30:00.000Z"), []settledRun{{"running", latestZ, nil}}, false},
	{"starting-latest", settledAt("2026-11-06T12:30:00.000Z"), []settledRun{{"starting", latestZ, nil}}, false},
	{"preparing-latest", settledAt("2026-11-06T12:30:00.000Z"), []settledRun{{"preparing", latestZ, nil}}, false},
	{"queued-latest", settledAt("2026-11-06T12:30:00.000Z"), []settledRun{{"queued", latestZ, nil}}, false},
	{"paused-latest", settledAt("2026-11-06T12:30:00.000Z"), done2("paused"), false},
	{"unknown-status-latest", settledAt("2026-11-06T12:30:00.000Z"), done2("mystery"), false},
	{"status-case-latest", settledAt("2026-11-06T12:30:00.000Z"), done2("Completed"), false},
	// No run: an explicit valid settlement alone is enough.
	{"norun-valid-at", settledAt("2026-11-06T12:30:00.000Z"), nil, true},
	{"norun-offset-at", settledAt("2026-11-06T08:30:00-04:00"), nil, true},
	{"norun-lowercase-at", settledAt("2026-11-06t12:30:00z"), nil, true},
	{"norun-raw-unsettled", `{}`, nil, false},
	{"norun-missing-at", `{"settledOverride":"settled"}`, nil, false},
	{"norun-null-at", `{"settledOverride":"settled","settledAt":null}`, nil, false},
	{"norun-invalid-at", settledAt("garbage"), nil, false},
	{"norun-naive-at", settledAt("2026-11-06T12:30:00"), nil, false},
	{"norun-other-override", `{"settledOverride":"unsettled","settledAt":"2026-11-06T12:30:00.000Z"}`, nil, false},
	{"norun-before-year-1", settledAt("0001-01-01T00:00:00+00:01"), nil, false},
}

// settledFixture adds each case as thread "zs-<id>" to the parity fixture.
func settledFixture(t *testing.T) string {
	t.Helper()
	path := fixtureDB(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, c := range settledCases {
		id := "zs-" + c.id
		if _, err := db.Exec(`INSERT INTO orchestration_v2_projection_threads VALUES
 (?,'p-chief','Settled case','codex','full','default',NULL,'2026-11-06T10:00:00.000Z','2026-11-06T10:00:00.000Z',NULL,NULL,?,NULL)`,
			id, c.payload); err != nil {
			t.Fatalf("%s: %v", id, err)
		}
		for i, r := range c.runs {
			if _, err := db.Exec(`INSERT INTO orchestration_v2_projection_runs VALUES (?,?,?,'codex',NULL,?,?,?,'{}',NULL)`,
				id+"-run-"+strconv.Itoa(i), id, i, r.status, r.requested, r.completed); err != nil {
				t.Fatalf("%s run %d: %v", id, i, err)
			}
		}
	}
	return path
}

func TestIngestSnapshotSettledRule(t *testing.T) {
	snap, err := BuildIngestSnapshot(context.Background(), settledFixture(t), "", func() time.Time {
		return time.Date(2026, 11, 6, 20, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]IngestThread{}
	for _, th := range snap.Threads {
		got[th.ThreadID] = th
	}
	for _, c := range settledCases {
		th, ok := got["zs-"+c.id]
		if !ok {
			t.Errorf("%s: missing from the snapshot", c.id)
			continue
		}
		mark := "ok"
		if th.Settled != c.want {
			mark = "MISMATCH"
			t.Errorf("%s: settled = %v, want %v", c.id, th.Settled, c.want)
		}
		t.Logf("%-44s settled=%-5v want=%-5v %s", c.id, th.Settled, c.want, mark)

		// Everything else on the thread is what it was before the rule changed.
		var latest *IngestRun
		active, queued := false, false
		if n := len(c.runs); n > 0 {
			r := c.runs[n-1]
			at := r.requested
			if s, _ := r.completed.(string); s != "" {
				at = s
			}
			latest = &IngestRun{Status: r.status, At: at}
		}
		for _, r := range c.runs { // no case queues behind a completed run
			active = active || ingestActive[r.status]
			queued = queued || r.status == "queued"
		}
		want := IngestThread{ThreadID: "zs-" + c.id, Title: "Settled case", Project: "chief",
			UpdatedAt: "2026-11-06T10:00:00.000Z", LatestRun: latest, Active: active, Queued: queued,
			Settled: th.Settled, PRs: []IngestPR{}}
		if !reflect.DeepEqual(th, want) {
			t.Errorf("%s: thread = %+v, want %+v", c.id, th, want)
		}
	}
	if len(snap.Pending) != 3 {
		t.Errorf("pending = %+v, want the fixture's three", snap.Pending)
	}
}

// Against the parity fixture, only settled may differ from expected.json, and
// settled follows the rule there too. The snapshot's shape is unchanged.
func TestIngestSnapshotSettledKeepsEveryOtherFixtureField(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "ingest", "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := comparable(t, raw).(map[string]any)
	got := comparable(t, buildFixture(t, fixtureDB(t))).(map[string]any)
	if g, w := sortedKeys(got), sortedKeys(want); !reflect.DeepEqual(g, w) {
		t.Fatalf("snapshot keys = %v, want %v", g, w)
	}
	if !reflect.DeepEqual(got["pending"], want["pending"]) {
		t.Errorf("pending differs: got %v, want %v", got["pending"], want["pending"])
	}
	settled := map[string]bool{"t-settled": true, "t-settled-norun": true, "t-settled-offset": true, "t-settled-fraction": true}
	gotThreads, wantThreads := got["threads"].([]any), want["threads"].([]any)
	if len(gotThreads) != len(wantThreads) {
		t.Fatalf("%d threads, want %d", len(gotThreads), len(wantThreads))
	}
	for i := range wantThreads {
		g, w := gotThreads[i].(map[string]any), wantThreads[i].(map[string]any)
		id, _ := g["threadId"].(string)
		if !reflect.DeepEqual(sortedKeys(g), sortedKeys(w)) {
			t.Errorf("%s keys = %v, want %v", id, sortedKeys(g), sortedKeys(w))
		}
		if g["settled"] != settled[id] {
			t.Errorf("%s: settled = %v, want %v", id, g["settled"], settled[id])
		}
		if _, isBool := g["settled"].(bool); !isBool {
			t.Errorf("%s: settled = %#v, want a bool", id, g["settled"])
		}
		delete(g, "settled")
		delete(w, "settled")
		if !reflect.DeepEqual(g, w) {
			gb, _ := json.Marshal(g)
			wb, _ := json.Marshal(w)
			t.Errorf("thread %d differs beyond settled:\n got %s\nwant %s", i, gb, wb)
		}
	}
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
