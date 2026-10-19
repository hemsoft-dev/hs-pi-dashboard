package t3

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
)

func TestV2LargeShellRetainsEveryActiveAndArchivedThread(t *testing.T) {
	_, c, now := fixture(t)
	f, _ := attachRuntime(t, c)
	f.v2, c.runtime.v2 = true, true
	// Reproduce the upstream 17 MiB shell with 1,404 unarchived and 207
	// archived threads. Unrelated link/preview bodies must not truncate IDs.
	f.padding = strings.Repeat("x", 17<<20)
	for i := range 1404 {
		state := "idle"
		if i < 3 {
			state = "running"
		}
		f.threads = append(f.threads, liveThread{ID: fmt.Sprintf("active-%d", i), Status: state})
	}
	for i := range 207 {
		f.archived = append(f.archived, liveThread{ID: fmt.Sprintf("archived-%d", i), Status: "completed", ArchivedAt: ptr(now.Format(time.RFC3339Nano))})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rows, err := c.runtime.snapshot(ctx)
	if err != nil {
		t.Fatalf("large V2 shell rejected: %v", err)
	}
	if len(rows) != 1611 {
		t.Fatalf("lost large-shell rows: %d", len(rows))
	}
	seen := map[string]bool{}
	active := 0
	for _, row := range rows {
		if seen[row.ID] {
			t.Fatal("duplicate shell row")
		}
		seen[row.ID] = true
		if _, live := row.state(); live {
			active++
		}
	}
	for i := range 1404 {
		if !seen[fmt.Sprintf("active-%d", i)] {
			t.Fatal("missing unarchived thread")
		}
	}
	for i := range 207 {
		if !seen[fmt.Sprintf("archived-%d", i)] {
			t.Fatal("missing archived thread")
		}
	}
	if active != 3 {
		t.Fatalf("large-shell live count %d", active)
	}
}

func TestRuntimeSizeBoundsPreserveLegacyCap(t *testing.T) {
	_, c, _ := fixture(t)
	f, _ := attachRuntime(t, c)
	f.padding = strings.Repeat("x", 9<<20)
	f.threads = []liveThread{{ID: "legacy", Session: &liveSession{Status: "running"}}}
	if _, err := c.runtime.snapshot(context.Background()); err == nil {
		t.Fatal("legacy size cap removed")
	}
	f.v2, c.runtime.v2 = true, true
	f.padding = ""
	f.threads = []liveThread{{ID: "v2", Status: "running"}}
	f.declaredLength = (64 << 20) + 1
	if _, err := c.runtime.snapshot(context.Background()); err == nil {
		t.Fatal("oversized V2 body accepted")
	}
}

func TestV2ProviderSwitchRetainsAllClaudeIdentities(t *testing.T) {
	db, c, now := v2Fixture(t)
	v2Thread(t, db, "one", now)
	exec(t, db, `INSERT INTO orchestration_v2_projection_provider_threads(provider_thread_id,thread_id,driver,payload_json) VALUES('older-claude','one','claudeAgent','{"nativeThreadRef":{"nativeId":"older-native"}}'),('current-codex','one','codex','{"nativeThreadRef":{"nativeId":"codex-native"}}')`)
	exec(t, db, `UPDATE orchestration_v2_projection_threads SET active_provider_thread_id='current-codex',default_provider='codex' WHERE thread_id='one'`)
	f, _ := attachRuntime(t, c)
	f.v2 = true
	f.threads = []liveThread{{ID: "one", Status: "idle"}}
	snapshot := c.Poll()
	native := []sessions.Summary{
		{ID: "claude-code:native-one", Provider: "claude-code", Active: true, LastActivity: now.Add(-time.Second), Outputs: []sessions.Output{{Text: "older Claude output"}}},
		{ID: "claude-code:older-native", Provider: "claude-code", Active: true, LastActivity: now, Outputs: []sessions.Output{{Text: "newer Claude output"}}},
		// A Codex native ID must not claim an unrelated Claude transcript.
		{ID: "claude-code:codex-native", Provider: "claude-code", Active: true, LastActivity: now},
	}
	for _, reverse := range []bool{false, true} {
		snapshots := []sessions.Snapshot{snapshot, {Sessions: native}}
		if reverse {
			snapshots[0], snapshots[1] = snapshots[1], snapshots[0]
		}
		merged := sessions.MergeSnapshots(snapshots...)
		if len(merged.Sessions) != 2 || merged.ActiveSessions != 2 {
			t.Fatalf("historical Claude duplication: rows=%d active=%d", len(merged.Sessions), merged.ActiveSessions)
		}
		for _, row := range merged.Sessions {
			if row.Source == "t3" && (row.Provider != "codex" || row.ActivitySource != "claude-code" || len(row.Outputs) != 1 || row.Outputs[0].Text != "newer Claude output") {
				t.Fatalf("switched row lost native activity: %+v", row)
			}
		}
	}
}

func TestV2CheckpointWorkDoesNotRequestOperatorInput(t *testing.T) {
	for _, status := range []string{"waiting", "completed"} {
		row := liveThread{V2: true, Status: status, ActivityRunStatus: "waiting"}
		if state, active := row.state(); state != "working" || !active {
			t.Fatalf("checkpoint state %s/%v", state, active)
		}
		row.PendingRequest = &struct {
			Kind string `json:"kind"`
		}{Kind: "user_input"}
		if state, active := row.state(); state != "waiting" || !active {
			t.Fatalf("actionable input state %s/%v", state, active)
		}
	}
}
