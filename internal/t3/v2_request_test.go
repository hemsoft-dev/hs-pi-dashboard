package t3

import (
	"strings"
	"testing"
)

// T3 leaves a runtime request pending after its run ends, and the V2 shell
// still reports it. activityRunStatus is null exactly when no run is live
// (#113). Older servers omit the field. Rows go through Collector.Poll as raw
// shell JSON so null and absent fields stay distinct.
func TestV2PendingRequestCountsOnlyOnLiveRuns(t *testing.T) {
	const omitted = "<omitted>"
	request := func(kind string) map[string]any { return map[string]any{"kind": kind} }
	monitor := []any{map[string]any{"kind": "monitor"}}
	command := []any{map[string]any{"kind": "command"}}
	tests := []struct {
		id, status string
		activity   any // a run status, nil for JSON null, or omitted
		request    map[string]any
		background []any
		archived   bool
		state      string
		active     bool
		label      string
	}{
		{id: "live-running", status: "running", activity: "running", request: request("user_input"), state: "waiting", active: true, label: "Input"},
		{id: "live-starting", status: "starting", activity: "starting", request: request("approval"), state: "waiting", active: true, label: "Approval"},
		{id: "live-preparing", status: "preparing", activity: "preparing", request: request("approval"), state: "waiting", active: true, label: "Approval"},
		{id: "live-waiting", status: "waiting", activity: "waiting", request: request("user_input"), state: "waiting", active: true, label: "Input"},
		{id: "live-behind-queued", status: "queued", activity: "running", request: request("user_input"), state: "waiting", active: true, label: "Input"},
		{id: "orphan-completed-input", status: "completed", activity: nil, request: request("user_input"), state: "idle", label: "Input"},
		{id: "orphan-completed-approval", status: "completed", activity: nil, request: request("approval"), state: "idle", label: "Approval"},
		{id: "orphan-idle-input", status: "idle", activity: nil, request: request("user_input"), state: "idle", label: "Input"},
		{id: "orphan-idle-approval", status: "idle", activity: nil, request: request("approval"), state: "idle", label: "Approval"},
		{id: "orphan-monitor", status: "completed", activity: nil, request: request("user_input"), background: monitor, state: "monitoring", active: true, label: "Input"},
		{id: "orphan-command", status: "completed", activity: nil, request: request("approval"), background: command, state: "working", active: true, label: "Approval"},
		{id: "omitted-completed-input", status: "completed", activity: omitted, request: request("user_input"), state: "waiting", active: true, label: "Input"},
		{id: "omitted-idle-approval", status: "idle", activity: omitted, request: request("approval"), state: "waiting", active: true, label: "Approval"},
		{id: "failed", status: "failed", activity: nil, request: request("approval"), state: "error", label: "Approval"},
		{id: "cancelled", status: "cancelled", activity: nil, request: request("user_input"), state: "stopped", label: "Input"},
		{id: "interrupted", status: "interrupted", activity: omitted, request: request("approval"), state: "stopped", label: "Approval"},
		{id: "rolled-back", status: "rolled_back", activity: nil, request: request("user_input"), state: "stopped", label: "Input"},
		{id: "archived-live", status: "running", activity: "running", request: request("approval"), archived: true, state: "stopped", label: ""},
	}
	db, c, now := v2Fixture(t)
	f, _ := attachRuntime(t, c)
	f.v2 = true
	f.rawThreads, f.rawArchived = []any{}, []any{}
	stamp := now.Format("2006-01-02T15:04:05Z07:00")
	wantActive := 0
	for _, tc := range tests {
		v2Thread(t, db, tc.id, now)
		background := tc.background
		if background == nil {
			background = []any{}
		}
		row := map[string]any{"id": tc.id, "status": tc.status, "latestRunId": "run-" + tc.id, "activeProviderThreadId": "provider-" + tc.id,
			"updatedAt": stamp, "latestRunCompletedAt": nil, "pendingRuntimeRequest": tc.request, "pendingBackgroundTasks": background}
		if tc.activity != omitted {
			row["activityRunStatus"] = tc.activity
		}
		if tc.archived {
			row["archivedAt"] = stamp
			f.rawArchived = append(f.rawArchived, row)
		} else {
			f.rawThreads = append(f.rawThreads, row)
		}
		if tc.active {
			wantActive++
		}
	}
	snap := c.Poll()
	if snap.Sources[0].State != "healthy" || len(snap.Sessions) != len(tests) {
		t.Fatalf("snapshot: %+v", snap)
	}
	rows := map[string]int{}
	for i, row := range snap.Sessions {
		rows[strings.TrimPrefix(row.ID, c.prefix())] = i
	}
	for _, tc := range tests {
		i, ok := rows[tc.id]
		if !ok {
			t.Errorf("%s: row missing", tc.id)
			continue
		}
		row := snap.Sessions[i]
		label := "<nil>"
		if row.T3Status != nil {
			label = *row.T3Status
		}
		if row.Status != tc.state || row.Active != tc.active || label != tc.label {
			t.Errorf("%s: status=%s active=%v label=%q, want %s %v %q", tc.id, row.Status, row.Active, label, tc.state, tc.active, tc.label)
		}
	}
	if snap.ActiveSessions != wantActive {
		t.Errorf("activeSessions = %d, want %d", snap.ActiveSessions, wantActive)
	}
}
