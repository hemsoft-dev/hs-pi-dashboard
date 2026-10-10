package t3

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
)

const fixtureSchema = `
 CREATE TABLE projection_projects(project_id TEXT PRIMARY KEY,title TEXT,workspace_root TEXT);
 CREATE TABLE projection_threads(thread_id TEXT PRIMARY KEY,project_id TEXT,title TEXT,worktree_path TEXT,latest_turn_id TEXT,created_at TEXT,updated_at TEXT,deleted_at TEXT,archived_at TEXT,model_selection_json TEXT,pending_approval_count INTEGER,pending_user_input_count INTEGER);
 CREATE TABLE projection_thread_sessions(thread_id TEXT PRIMARY KEY,provider_name TEXT,provider_session_id TEXT,provider_thread_id TEXT,status TEXT,active_turn_id TEXT,last_error TEXT,updated_at TEXT);
 CREATE TABLE provider_session_runtime(thread_id TEXT PRIMARY KEY,last_seen_at TEXT);
 CREATE TABLE projection_turns(thread_id TEXT,turn_id TEXT,state TEXT,started_at TEXT,requested_at TEXT,completed_at TEXT);
 CREATE TABLE projection_thread_messages(thread_id TEXT,role TEXT);
 CREATE TABLE projection_thread_activities(thread_id TEXT,activity_id TEXT,kind TEXT,payload_json TEXT,created_at TEXT,sequence INTEGER);
 INSERT INTO projection_projects VALUES('project','fleet-repo','/repos/fleet-repo');`

func fixture(t *testing.T) (*sql.DB, *Collector, time.Time) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.sqlite")
	db := createDB(t, path)
	c := New(path, time.Minute, "")
	// These fixtures isolate SQLite metadata parsing; live authority is tested separately.
	c.runtime = nil
	now := time.Date(2026, 10, 10, 1, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	return db, c, now
}

func createDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	exec(t, db, fixtureSchema)
	return db
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func thread(t *testing.T, db *sql.DB, id string, at time.Time) {
	t.Helper()
	stamp := at.Format(time.RFC3339Nano)
	exec(t, db, `INSERT INTO projection_threads VALUES(?, 'project', ?, NULL,'turn',?,?,NULL,NULL,?,0,0)`, id, "Thread "+id, stamp, stamp, `{"instanceId":"codex","model":"gpt-test","options":[{"id":"reasoningEffort","value":"high"},{"id":"booleanOption","value":true}]}`)
	exec(t, db, `INSERT INTO projection_thread_sessions VALUES(?, 'codex',NULL,NULL,'running','turn','',?)`, id, stamp)
	exec(t, db, `INSERT INTO provider_session_runtime VALUES(?,?)`, id, stamp)
	exec(t, db, `INSERT INTO projection_turns VALUES(?,'turn','running',?,?,NULL)`, id, stamp, stamp)
	exec(t, db, `INSERT INTO projection_thread_messages VALUES(?,'user'),(?,'assistant'),(?,'system')`, id, id, id)
}

func TestStatesAndThreadMetadata(t *testing.T) {
	db, c, now := fixture(t)
	for _, id := range []string{"working", "waiting", "idle", "stopped", "error", "stale", "deleted", "archived", "without-session"} {
		thread(t, db, id, now)
	}
	exec(t, db, `UPDATE projection_turns SET completed_at=? WHERE thread_id IN ('working','waiting')`, now.Add(-time.Minute).Format(time.RFC3339Nano))
	exec(t, db, `UPDATE projection_threads SET pending_user_input_count=1 WHERE thread_id='waiting'`)
	exec(t, db, `UPDATE projection_thread_sessions SET active_turn_id=NULL WHERE thread_id='idle'`)
	exec(t, db, `UPDATE projection_turns SET state='completed', completed_at=? WHERE thread_id='idle'`, now.Add(10*time.Second).Format(time.RFC3339Nano))
	exec(t, db, `UPDATE projection_thread_sessions SET status='stopped' WHERE thread_id='stopped'`)
	exec(t, db, `UPDATE projection_turns SET state='error' WHERE thread_id='error'`)
	old := now.Add(-2 * time.Minute).Format(time.RFC3339Nano)
	for _, table := range []string{"projection_threads", "projection_thread_sessions"} {
		exec(t, db, "UPDATE "+table+" SET updated_at=? WHERE thread_id='stale'", old)
	}
	exec(t, db, `UPDATE provider_session_runtime SET last_seen_at=? WHERE thread_id='stale'`, old)
	exec(t, db, `UPDATE projection_turns SET started_at=?, requested_at=? WHERE thread_id='stale'`, old, old)
	exec(t, db, `UPDATE projection_threads SET deleted_at=? WHERE thread_id='deleted'`, old)
	exec(t, db, `UPDATE projection_threads SET archived_at=? WHERE thread_id='archived'`, old)
	exec(t, db, `DELETE FROM projection_thread_sessions WHERE thread_id='without-session'`)
	exec(t, db, `DELETE FROM projection_turns WHERE thread_id='without-session'`)
	snap := c.Poll()
	if len(snap.Sessions) != 8 || snap.ActiveSessions != 2 {
		t.Fatalf("snapshot: %+v", snap)
	}
	expected := map[string]string{"working": "working", "waiting": "waiting", "idle": "idle", "stopped": "stopped", "error": "error", "stale": "stale", "archived": "stopped", "without-session": "idle"}
	for _, s := range snap.Sessions {
		id := s.ID[len(c.prefix()):]
		if s.Status != expected[id] || s.Name != "Thread "+id || s.Project != "fleet-repo" || s.Cwd != "/repos/fleet-repo" || s.Model != "gpt-test" || s.ThinkingLevel != "high" || s.MessageCount != 2 {
			t.Fatalf("thread %s: %+v", id, s)
		}
		if (id == "working" || id == "waiting") && s.TurnCompletedAt != nil {
			t.Fatal("resumed active turn retained its earlier completion")
		}
		if s.T3Usage != nil {
			t.Fatal("invented usage")
		}
		if s.Status == "idle" && id == "idle" && s.TurnCompletedAt == nil {
			t.Fatal("latest completed turn missing")
		}
	}
	if snap.Sources[0].State != "stale" {
		t.Fatalf("health: %+v", snap.Sources)
	}
}

func TestIncrementalCountersAreSnapshotsAndToolsAreUnique(t *testing.T) {
	db, c, now := fixture(t)
	thread(t, db, "one", now)
	event := func(seq int, kind, payload string) {
		exec(t, db, `INSERT INTO projection_thread_activities VALUES('one',?,?,?,?,?)`, fmt.Sprint(seq), kind, payload, now.Format(time.RFC3339Nano), seq)
	}
	event(1, "tool.started", `{"toolCallId":"tool-1"}`)
	event(2, "tool.updated", `{"toolCallId":"tool-1"}`)
	event(3, "context-window.updated", `{"inputTokens":100,"outputTokens":20,"totalProcessedTokens":120,"usedTokens":30,"maxTokens":200,"cachedInputTokens":5,"reasoningOutputTokens":2}`)
	exec(t, db, `UPDATE projection_thread_activities SET sequence=NULL`)
	for range 2 {
		s := c.Poll().Sessions[0]
		if s.ToolCallCount != 1 || *s.T3Usage.TotalTokens != 120 || *s.T3Usage.ContextTokens != 30 {
			t.Fatalf("counters: %+v", s)
		}
	}
	event(4, "context-window.updated", `{"inputTokens":150,"outputTokens":25,"totalProcessedTokens":175,"usedTokens":40,"maxTokens":200}`)
	event(5, "tool.completed", `{"toolCallId":"tool-1"}`)
	event(6, "tool.started", `{"toolCallId":"tool-2"}`)
	s := c.Poll().Sessions[0]
	if s.ToolCallCount != 2 || *s.T3Usage.TotalTokens != 175 || s.T3Usage.CacheReadTokens != nil {
		t.Fatalf("latest reported stats: %+v", s)
	}
	if c.rowID != 6 {
		t.Fatal("incremental watermark")
	}
	exec(t, db, `UPDATE projection_threads SET deleted_at=? WHERE thread_id='one'`, now.Format(time.RFC3339Nano))
	if len(c.Poll().Sessions) != 0 || len(c.activities) != 0 {
		t.Fatal("deleted thread or cache retained")
	}
}

func TestReadOnlyFailureRecoveryAndSchemaChange(t *testing.T) {
	db, c, now := fixture(t)
	thread(t, db, "one", now)
	before, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Poll().ActiveSessions != 1 {
		t.Fatal("initial live snapshot")
	}
	after, err := os.ReadFile(c.path)
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("collector wrote the database")
	}
	exec(t, db, `BEGIN EXCLUSIVE`)
	failed := c.Poll()
	if failed.ActiveSessions != 0 || len(failed.Sessions) != 1 || failed.Sessions[0].Active || failed.Sessions[0].Status != "unavailable" || failed.Sources[0].State != "unavailable" {
		t.Fatalf("locked cache: %+v", failed)
	}
	exec(t, db, `ROLLBACK`)
	if c.Poll().ActiveSessions != 1 {
		t.Fatal("did not recover after unlock")
	}
	exec(t, db, `ALTER TABLE projection_threads RENAME TO incompatible_threads`)
	if c.Poll().Sources[0].State != "unavailable" {
		t.Fatal("incompatible schema appeared healthy")
	}
	exec(t, db, `ALTER TABLE incompatible_threads RENAME TO projection_threads`)
	if c.Poll().ActiveSessions != 1 {
		t.Fatal("schema recovery")
	}
	missing := New(filepath.Join(t.TempDir(), "missing.sqlite"), time.Minute, "")
	missing.runtime = nil
	if missing.Poll().Sources[0].State != "unavailable" {
		t.Fatal("missing source appeared healthy")
	}
	if _, err := os.Stat(missing.path); !os.IsNotExist(err) {
		t.Fatal("collector created missing database")
	}
	_ = createDB(t, missing.path)
	if missing.Poll().Sources[0].State != "healthy" {
		t.Fatal("missing source did not recover")
	}
}

func TestDatabaseReplacementAndSequenceReset(t *testing.T) {
	db, c, now := fixture(t)
	thread(t, db, "one", now)
	exec(t, db, `INSERT INTO projection_thread_activities VALUES('one','a','tool.started','{"toolCallId":"old"}',?,10)`, now.Format(time.RFC3339Nano))
	if c.Poll().Sessions[0].ToolCallCount != 1 {
		t.Fatal("initial cache")
	}
	exec(t, db, `DELETE FROM projection_thread_activities`)
	if c.Poll().Sessions[0].ToolCallCount != 0 {
		t.Fatal("watermark reset did not invalidate usage")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(c.path, c.path+".old"); err != nil {
		t.Fatal(err)
	}
	replacement := createDB(t, c.path)
	thread(t, replacement, "two", now)
	snap := c.Poll()
	if len(snap.Sessions) != 1 || snap.Sessions[0].Name != "Thread two" {
		t.Fatalf("replacement: %+v", snap)
	}
}

func TestEveryThreadAndLiveRowSurviveHistoryCapAndDedup(t *testing.T) {
	db, c, now := fixture(t)
	for i := range 45 {
		thread(t, db, fmt.Sprint(i), now)
	}
	exec(t, db, `UPDATE projection_thread_sessions SET provider_name='claudeAgent',provider_session_id='claude-shared' WHERE thread_id='0'`)
	t3Snap := c.Poll()
	transcript := sessions.Snapshot{GeneratedAt: now, Sessions: []sessions.Summary{{ID: "claude-code:claude-shared", Provider: "claude-code", Active: true}, {ID: "pi-shared", Provider: "openai-codex", Active: true}}}
	merged := sessions.MergeSnapshots(transcript, t3Snap, t3Snap)
	if len(merged.Sessions) != 46 || merged.ActiveSessions != 46 {
		t.Fatalf("dedup/live cap: %d/%d", len(merged.Sessions), merged.ActiveSessions)
	}
	exec(t, db, `UPDATE projection_thread_sessions SET status='stopped'`)
	merged = sessions.MergeSnapshots(transcript, c.Poll())
	if len(merged.Sessions) != 46 || merged.ActiveSessions != 2 {
		t.Fatalf("all quiet threads: %d/%d", len(merged.Sessions), merged.ActiveSessions)
	}
}

func TestReadOnlyPathWithURICharacters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "T3 data # & +", "state.sqlite")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	db := createDB(t, path)
	now := time.Now()
	thread(t, db, "one", now)
	c := New(path, time.Minute, "")
	c.runtime = nil
	snap := c.Poll()
	if len(snap.Sessions) != 1 || snap.ActiveSessions != 1 || snap.Sources[0].State != "healthy" {
		t.Fatalf("escaped native path: %+v", snap.Sources)
	}
}

func TestTerminalTurnsOverrideLaggingActiveIdentity(t *testing.T) {
	db, c, now := fixture(t)
	for _, state := range []string{"completed", "interrupted"} {
		thread(t, db, state, now)
		exec(t, db, `UPDATE projection_turns SET state=?, completed_at=? WHERE thread_id=?`, state, now.Format(time.RFC3339Nano), state)
	}
	snap := c.Poll()
	if snap.ActiveSessions != 0 || len(snap.Sessions) != 2 {
		t.Fatalf("terminal threads counted: %+v", snap)
	}
	for _, s := range snap.Sessions {
		if s.TurnCompletedAt == nil || (s.Status != "idle" && s.Status != "stopped") {
			t.Fatalf("lost terminal state: %+v", s)
		}
	}
}

func TestLegacyAndCurrentModelOptionsCoexist(t *testing.T) {
	db, c, now := fixture(t)
	for id, model := range map[string]string{
		"legacy":  `{"instanceId":"claudeAgent","model":"claude-test","options":{" effort ":" high ","other":true,"nested":{"ignore":1}}}`,
		"array":   `{"instanceId":"codex","model":"codex-test","options":[{"id":"reasoningEffort","value":"high"}]}`,
		"missing": `{"instanceId":"codex","model":"codex-test"}`,
	} {
		thread(t, db, id, now)
		exec(t, db, `UPDATE projection_threads SET model_selection_json=? WHERE thread_id=?`, model, id)
	}
	snap := c.Poll()
	if len(snap.Sessions) != 3 || snap.Sources[0].State != "healthy" {
		t.Fatalf("legacy source unavailable: %+v", snap.Sources)
	}
	for _, s := range snap.Sessions {
		if s.Name != "Thread missing" && s.ThinkingLevel != "high" {
			t.Fatalf("model options: %+v", s)
		}
	}
}

func TestStartingSessionsUsePendingRequestAndBecomeStale(t *testing.T) {
	db, c, now := fixture(t)
	thread(t, db, "starting", now.Add(-2*time.Minute))
	exec(t, db, `UPDATE projection_thread_sessions SET status='starting',active_turn_id=NULL WHERE thread_id='starting'`)
	exec(t, db, `UPDATE projection_turns SET state='completed',completed_at=? WHERE thread_id='starting'`, now.Add(-2*time.Minute).Format(time.RFC3339Nano))
	requested := now.Add(-10 * time.Second)
	exec(t, db, `INSERT INTO projection_turns VALUES('starting',NULL,'pending',NULL,?,NULL)`, requested.Format(time.RFC3339Nano))
	snap := c.Poll()
	if len(snap.Sessions) != 1 || snap.ActiveSessions != 1 || snap.Sessions[0].Status != "connecting" || snap.Sessions[0].TurnStartedAt == nil || !snap.Sessions[0].TurnStartedAt.Equal(requested) || snap.Sessions[0].TurnCompletedAt != nil {
		t.Fatalf("connecting snapshot: %+v", snap)
	}
	c.now = func() time.Time { return now.Add(2 * time.Minute) }
	snap = c.Poll()
	if snap.ActiveSessions != 0 || snap.Sessions[0].Status != "stale" {
		t.Fatalf("stalled startup: %+v", snap)
	}
}

func TestStartingWithoutPendingTurnDoesNotReuseCompletedTiming(t *testing.T) {
	db, c, now := fixture(t)
	thread(t, db, "starting", now)
	exec(t, db, `UPDATE projection_thread_sessions SET status='starting',active_turn_id=NULL WHERE thread_id='starting'`)
	exec(t, db, `UPDATE projection_turns SET state='completed',completed_at=?`, now.Format(time.RFC3339Nano))
	snap := c.Poll()
	s := snap.Sessions[0]
	if !s.Active || s.Status != "connecting" || s.TurnStartedAt != nil || s.TurnCompletedAt != nil {
		t.Fatalf("startup: %+v", s)
	}
}

func TestDefaultDetectsV2OnUpgradeAndNeverFallsBackToFrozenV1(t *testing.T) {
	db, c, now := fixture(t)
	c.defaultPath = true
	thread(t, db, "v1", now)
	if c.Poll().ActiveSessions != 1 {
		t.Fatal("V1 unavailable before upgrade")
	}
	v2path := filepath.Join(filepath.Dir(c.path), "statev2.sqlite")
	v2 := createDB(t, v2path)
	exec(t, v2, `CREATE TABLE orchestration_v2_projection_threads(thread_id TEXT)`)
	snap := c.Poll()
	if snap.ActiveSessions != 0 || snap.Sources[0].State != "unavailable" || snap.Sources[0].Path != v2path || !strings.Contains(snap.Sources[0].Error, "V2") || snap.Sessions[0].Status != "unavailable" {
		t.Fatalf("upgrade: %+v", snap)
	}
	// An explicitly selected legacy environment remains independently monitorable.
	explicit := New(filepath.Join(filepath.Dir(v2path), "state.sqlite"), time.Minute, "")
	explicit.runtime = nil
	explicit.now = c.now
	if explicit.Poll().ActiveSessions != 1 {
		t.Fatal("explicit V1 instance lost")
	}
	// Removing the V2 source must not silently re-enable the frozen legacy file.
	if err := v2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(v2path); err != nil {
		t.Fatal(err)
	}
	if snap = c.Poll(); snap.ActiveSessions != 0 || snap.Sources[0].Path != v2path || snap.Sources[0].State != "unavailable" {
		t.Fatalf("V2 disappearance: %+v", snap)
	}
}

func TestExplicitV2WithLegacyTablesIsUnsupported(t *testing.T) {
	db, c, now := fixture(t)
	thread(t, db, "legacy-copy", now)
	exec(t, db, `CREATE TABLE orchestration_v2_projection_threads(thread_id TEXT)`)
	snap := c.Poll()
	if snap.ActiveSessions != 0 || len(snap.Sessions) != 0 || snap.Sources[0].State != "unavailable" || !strings.Contains(snap.Sources[0].Error, "V2") {
		t.Fatalf("explicit V2: %+v", snap)
	}
}

func TestReadyProviderDoesNotActivateRetainedNonterminalTurn(t *testing.T) {
	for _, state := range []string{"running", "pending"} {
		t.Run(state, func(t *testing.T) {
			db, c, now := fixture(t)
			thread(t, db, "ready", now)
			exec(t, db, `UPDATE projection_thread_sessions SET status='ready'`)
			exec(t, db, `UPDATE projection_turns SET state=?`, state)
			snap := c.Poll()
			if snap.ActiveSessions != 0 || snap.Sessions[0].Active || snap.Sessions[0].Status != "idle" {
				t.Fatalf("ready with retained %s turn: %+v", state, snap)
			}
			exec(t, db, `UPDATE projection_thread_sessions SET status='running'`)
			if snap = c.Poll(); snap.ActiveSessions != 1 || snap.Sessions[0].Status != "working" {
				t.Fatalf("provider resumed: %+v", snap)
			}
		})
	}
}
