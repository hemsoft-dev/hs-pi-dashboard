package t3

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
)

// Column and JSON field names mirror T3's Orchestrator V2 projections. The V1
// tables deliberately coexist so accidentally reading the frozen import fails.
const v2Schema = `
CREATE TABLE orchestration_v2_projection_threads(thread_id TEXT PRIMARY KEY,project_id TEXT,title TEXT,default_provider TEXT,active_provider_thread_id TEXT,created_at TEXT,updated_at TEXT,archived_at TEXT,deleted_at TEXT,payload_json TEXT);
CREATE TABLE orchestration_v2_projection_provider_threads(provider_thread_id TEXT PRIMARY KEY,thread_id TEXT,driver TEXT,payload_json TEXT);
CREATE TABLE orchestration_v2_projection_runs(thread_id TEXT,ordinal INTEGER,requested_at TEXT,completed_at TEXT,payload_json TEXT);
CREATE TABLE orchestration_v2_projection_provider_turns(provider_thread_id TEXT,ordinal INTEGER,payload_json TEXT);
CREATE TABLE orchestration_v2_projection_messages(thread_id TEXT,role TEXT,payload_json TEXT);
CREATE TABLE orchestration_v2_projection_nodes(node_id TEXT PRIMARY KEY,thread_id TEXT,kind TEXT,payload_json TEXT);
CREATE INDEX v2_runs_thread ON orchestration_v2_projection_runs(thread_id,ordinal);
CREATE INDEX v2_turns_thread ON orchestration_v2_projection_provider_turns(provider_thread_id,ordinal);
CREATE INDEX v2_nodes_thread ON orchestration_v2_projection_nodes(thread_id);
CREATE INDEX v2_messages_thread ON orchestration_v2_projection_messages(thread_id);`

func v2Fixture(t *testing.T) (*sql.DB, *Collector, time.Time) {
	t.Helper()
	db, c, now := fixture(t)
	exec(t, db, v2Schema)
	thread(t, db, "frozen-v1-only", now)
	return db, c, now
}

func v2Thread(t *testing.T, db *sql.DB, id string, at time.Time) {
	t.Helper()
	stamp := at.Format(time.RFC3339Nano)
	exec(t, db, `INSERT INTO orchestration_v2_projection_threads VALUES(?,'project',?,'claudeAgent',?,?,?,NULL,NULL,?)`, id, "V2 "+id, "provider-"+id, stamp, stamp,
		`{"worktreePath":"/repos/worktree","modelSelection":{"instanceId":"claude","model":"claude-test","options":{"effort":"high"}},"prompt":"private-prompt-secret"}`)
	exec(t, db, `INSERT INTO orchestration_v2_projection_provider_threads VALUES(?,?,'claudeAgent',?)`, "provider-"+id, id,
		fmt.Sprintf(`{"nativeThreadRef":{"nativeId":%q},"contextUsage":{"usedTokens":30,"maxTokens":200},"nativeMetadata":{"prompt":"private-prompt-secret"}}`, "native-"+id))
	exec(t, db, `INSERT INTO orchestration_v2_projection_runs VALUES(?,1,?,NULL,?)`, id, stamp, fmt.Sprintf(`{"startedAt":%q}`, stamp))
	exec(t, db, `INSERT INTO orchestration_v2_projection_provider_turns VALUES(?,1,?)`, "provider-"+id,
		`{"tokenUsage":{"inputTokens":100,"outputTokens":0,"cachedInputTokens":5,"reasoningOutputTokens":2}}`)
	exec(t, db, `INSERT INTO orchestration_v2_projection_messages VALUES(?,'user','private-prompt-secret'),(?,'assistant','private-output-secret')`, id, id)
	exec(t, db, `INSERT INTO orchestration_v2_projection_nodes VALUES(?,?,'tool_call','private-tool-secret')`, "node-"+id, id)
}

func TestV2CompleteCoverageCountersPrivacyAndDedup(t *testing.T) {
	db, c, now := v2Fixture(t)
	for i := range 75 {
		v2Thread(t, db, fmt.Sprint(i), now)
	}
	exec(t, db, `UPDATE orchestration_v2_projection_threads SET archived_at=? WHERE thread_id='1'`, now.Format(time.RFC3339Nano))
	exec(t, db, `UPDATE orchestration_v2_projection_threads SET deleted_at=? WHERE thread_id='2'`, now.Format(time.RFC3339Nano))
	exec(t, db, `DELETE FROM orchestration_v2_projection_messages WHERE thread_id='3'`)
	exec(t, db, `DELETE FROM orchestration_v2_projection_provider_threads WHERE thread_id='3'`)
	before, _ := os.ReadFile(c.path)
	for range 2 {
		snap := c.Poll()
		if snap.Sources[0].State != "healthy" || snap.ActiveSessions != 0 || len(snap.Sessions) != 74 {
			t.Fatalf("coverage: rows=%d health=%+v", len(snap.Sessions), snap.Sources)
		}
		seen := map[string]bool{}
		for _, row := range snap.Sessions {
			if seen[row.ID] || strings.HasSuffix(row.ID, ":2") || strings.Contains(row.ID, "frozen") {
				t.Fatalf("duplicate/deleted/legacy row: %s", row.ID)
			}
			seen[row.ID] = true
			if row.Name == "V2 3" {
				if row.MessageCount != 0 || row.ProviderThreadID != "" {
					t.Fatal("no-output thread not retained")
				}
				continue
			}
			if row.Cwd != "/repos/worktree" || row.Project != "fleet-repo" || row.Model != "claude-test" || row.ThinkingLevel != "high" || row.MessageCount != 2 || row.ToolCallCount != 1 || row.T3Usage == nil || *row.T3Usage.InputTokens != 100 || *row.T3Usage.OutputTokens != 0 || row.T3Usage.TotalTokens != nil || *row.T3Usage.ContextTokens != 30 {
				t.Fatalf("metadata/counters: %+v", row)
			}
		}
		native := sessions.Summary{ID: "claude-code:native-0", Provider: "claude-code", Active: true, LastActivity: now}
		merged := sessions.MergeSnapshots(snap, sessions.Snapshot{Sessions: []sessions.Summary{native}})
		if len(merged.Sessions) != 74 || merged.ActiveSessions != 1 {
			t.Fatal("native transcript not deduplicated")
		}
		encoded, _ := json.Marshal(snap)
		if strings.Contains(string(encoded), "secret") {
			t.Fatal("private payload leaked")
		}
	}
	after, _ := os.ReadFile(c.path)
	if string(before) != string(after) {
		t.Fatal("V2 database changed")
	}
	// Updated materialized snapshots replace counters, including explicit zero.
	exec(t, db, `UPDATE orchestration_v2_projection_provider_turns SET payload_json='{"tokenUsage":{"inputTokens":0}}' WHERE provider_thread_id='provider-0'`)
	for _, row := range c.Poll().Sessions {
		if row.Name == "V2 0" && (*row.T3Usage.InputTokens != 0 || row.T3Usage.OutputTokens != nil || row.ToolCallCount != 1) {
			t.Fatalf("counters accumulated: %+v", row.T3Usage)
		}
	}
}

func TestV2VerifiedStatesQuietWorkFailureAndRecovery(t *testing.T) {
	db, c, now := v2Fixture(t)
	f, _ := attachRuntime(t, c)
	f.v2 = true
	states := map[string]string{"quiet": "running", "connect": "starting", "wait": "waiting", "monitor": "completed", "idle": "idle", "done": "completed", "cancel": "cancelled", "failed": "failed", "queue": "queued", "archived": "running"}
	for id, state := range states {
		v2Thread(t, db, id, now.Add(-time.Hour))
		live := liveThread{ID: id, Status: state}
		if id == "monitor" {
			live.PendingBackground = append(live.PendingBackground, struct {
				Kind string `json:"kind"`
			}{Kind: "monitor"})
		}
		if id == "archived" {
			live.ArchivedAt = ptr(now.Format(time.RFC3339Nano))
			f.archived = append(f.archived, live)
		} else {
			f.threads = append(f.threads, live)
		}
	}
	check := func(want int) sessions.Snapshot {
		t.Helper()
		snap := c.Poll()
		if snap.ActiveSessions != want || len(snap.Sessions) != len(states) {
			t.Fatalf("states: %+v", snap)
		}
		return snap
	}
	snap := check(4)
	want := map[string]string{"quiet": "working", "connect": "connecting", "wait": "waiting", "monitor": "monitoring", "idle": "idle", "done": "idle", "cancel": "stopped", "failed": "error", "queue": "idle", "archived": "stopped"}
	for _, row := range snap.Sessions {
		if row.Status != want[strings.TrimPrefix(row.ID, c.prefix())] {
			t.Fatalf("state %+v", row)
		}
	}
	c.now = func() time.Time { return now.Add(10 * time.Minute) }
	check(4)
	f.status = http.StatusUnauthorized
	if snap = check(0); snap.Sources[0].State != "unavailable" {
		t.Fatal("auth failure hidden")
	}
	f.status = http.StatusOK
	check(4)
	if f.grants != 2 {
		t.Fatal("failed auth did not renew")
	}
	for i := range f.threads {
		if f.threads[i].ID == "quiet" {
			f.threads[i].Status = "completed"
		}
	}
	check(3)
	// A corrupt live schema must fail closed even with freshly persisted runs.
	f.threads[0].Status = "future-unknown-state"
	if snap = check(0); snap.Sources[0].State != "unavailable" {
		t.Fatal("unknown runtime state accepted")
	}
}

func TestV2UpgradeIdentityAndNoFrozenFallback(t *testing.T) {
	db, c, now := fixture(t)
	thread(t, db, "shared", now)
	c.defaultPath = true
	oldID := c.Poll().Sessions[0].ID
	v2path := filepath.Join(filepath.Dir(c.path), "statev2.sqlite")
	v2 := createDB(t, v2path)
	exec(t, v2, v2Schema)
	v2Thread(t, v2, "shared", now)
	v2Thread(t, v2, "new", now)
	snap := c.Poll()
	if len(snap.Sessions) != 2 || snap.Sources[0].Path != v2path {
		t.Fatalf("migration %+v", snap)
	}
	if snap.Sessions[1].ID != oldID {
		t.Fatal("migration changed identity")
	}
	if err := v2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(v2path); err != nil {
		t.Fatal(err)
	}
	snap = c.Poll()
	if snap.ActiveSessions != 0 || len(snap.Sessions) != 2 || snap.Sources[0].State != "unavailable" {
		t.Fatalf("frozen fallback %+v", snap)
	}
	other := New(filepath.Join(t.TempDir(), "statev2.sqlite"), time.Minute, "")
	if other.prefix() == c.prefix() {
		t.Fatal("independent environments collide")
	}
}

func TestV2LiveCreationTimingAndTerminalPrecedence(t *testing.T) {
	db, c, now := v2Fixture(t)
	f, _ := attachRuntime(t, c)
	f.v2 = true
	f.threads = []liveThread{{ID: "between-snapshots", Status: "running", Title: "New V2", ActivityRunStartedAt: now.Format(time.RFC3339Nano)}}
	f.beforeShell = func() { v2Thread(t, db, "between-snapshots", now) }
	snap := c.Poll()
	if len(snap.Sessions) != 1 || snap.ActiveSessions != 1 || snap.Sessions[0].ProviderThreadID != "native-between-snapshots" {
		t.Fatalf("live creation: %+v", snap)
	}
	// A queued run must not replace the actual active run's duration.
	exec(t, db, `INSERT INTO orchestration_v2_projection_runs VALUES('between-snapshots',2,?,NULL,'{}')`, now.Add(time.Minute).Format(time.RFC3339Nano))
	snap = c.Poll()
	if snap.Sessions[0].TurnStartedAt == nil || !snap.Sessions[0].TurnStartedAt.Equal(now) {
		t.Fatal("live turn timing lost to queued projection")
	}
	f.threads[0].Status = "cancelled"
	f.threads[0].PendingRequest = &struct {
		Kind string `json:"kind"`
	}{Kind: "approval"}
	f.threads[0].PendingBackground = append(f.threads[0].PendingBackground, struct {
		Kind string `json:"kind"`
	}{Kind: "monitor"})
	snap = c.Poll()
	if snap.ActiveSessions != 0 || snap.Sessions[0].Status != "stopped" {
		t.Fatal("terminal work resurrected")
	}
	// Recovery must replace metadata, not append a second copy of the live row.
	f.threads[0].Status = "idle"
	f.threads[0].PendingRequest = nil
	f.threads[0].PendingBackground = nil
	if got := c.Poll(); len(got.Sessions) != 1 {
		t.Fatal("live row duplicated")
	}
}
