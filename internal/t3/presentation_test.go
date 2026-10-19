package t3

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func presentedThread(t *testing.T, fields map[string]any) liveThread {
	t.Helper()
	data := map[string]any{"id": "thread", "status": "completed", "latestRunId": "run", "activeProviderThreadId": "provider", "activityRunStatus": nil, "updatedAt": "2026-10-19T12:00:00Z", "pendingBackgroundTasks": []any{}, "latestRunCompletedAt": nil}
	for k, v := range fields {
		data[k] = v
	}
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	var thread liveThread
	if err = json.Unmarshal(raw, &thread); err != nil {
		t.Fatal(err)
	}
	thread.V2 = true
	return thread
}

func TestSidebarStatusParity(t *testing.T) {
	now := parseTime("2026-10-19T12:00:00Z")
	tests := []struct {
		name            string
		fields          map[string]any
		delegated, want string
	}{
		{"working", map[string]any{"status": "running"}, "", "Working"},
		{"starting", map[string]any{"status": "starting"}, "", "Working"},
		{"preparing", map[string]any{"status": "preparing"}, "", "Working"},
		{"queued", map[string]any{"status": "queued"}, "", "Working"},
		{"checkpoint", map[string]any{"status": "waiting"}, "", "Working"},
		{"activity behind newer run", map[string]any{"status": "queued", "activityRunStatus": "running"}, "", "Working"},
		{"goal", map[string]any{"status": "running", "goal": map[string]any{"status": "active"}}, "", "Goal"},
		{"inactive goal", map[string]any{"status": "running", "goal": map[string]any{"status": "complete"}}, "", "Working"},
		{"idle runtime", map[string]any{"status": "idle"}, "", "Waiting"},
		{"monitor", map[string]any{"pendingBackgroundTasks": []any{map[string]any{"kind": "monitor"}}}, "", "Waiting"},
		{"patched command", map[string]any{"status": "waiting", "pendingBackgroundTasks": []any{map[string]any{"kind": "command"}}}, "", "Waiting"},
		{"unknown task", map[string]any{"pendingBackgroundTasks": []any{map[string]any{"kind": "future"}}}, "", ""},
		{"approval", map[string]any{"status": "running", "pendingRuntimeRequest": map[string]any{"kind": "approval"}}, "", "Approval"},
		{"input", map[string]any{"status": "running", "pendingRuntimeRequest": map[string]any{"kind": "user_input"}}, "", "Input"},
		{"auth is not approval", map[string]any{"status": "running", "pendingRuntimeRequest": map[string]any{"kind": "auth_refresh"}}, "", "Working"},
		{"delegated input", map[string]any{"status": "running"}, "user_input", "Input"},
		{"approval outranks own input", map[string]any{"pendingRuntimeRequest": map[string]any{"kind": "user_input"}}, "approval", "Approval"},
		{"limited", map[string]any{"status": "failed", "lastErrorClass": "usage_limit"}, "", "Limited"},
		{"failed", map[string]any{"status": "failed", "lastErrorClass": "other"}, "", "Failed"},
		{"stale limit on recovered run", map[string]any{"status": "running", "lastErrorClass": "usage_limit"}, "", "Working"},
		{"failed overrides background", map[string]any{"status": "failed", "lastErrorClass": "usage_limit", "pendingBackgroundTasks": []any{map[string]any{"kind": "monitor"}}}, "", "Limited"},
		{"done", map[string]any{"lastVisitedAt": "2026-10-19T10:00:00Z", "latestRunCompletedAt": "2026-10-19T11:00:00Z"}, "", "Done"},
		{"visited completion", map[string]any{"lastVisitedAt": "2026-10-19T11:00:00Z", "latestRunCompletedAt": "2026-10-19T11:00:00Z"}, "", ""},
		{"never visited is not done", map[string]any{"lastVisitedAt": nil, "latestRunCompletedAt": "2026-10-19T11:00:00Z"}, "", ""},
		{"missing completed metadata", map[string]any{"lastVisitedAt": "2026-10-19T10:00:00Z", "latestRunCompletedAt": nil}, "", ""},
		{"wake", map[string]any{"snoozedUntil": "2026-10-19T11:00:00Z", "lastVisitedAt": "2026-10-19T10:00:00Z"}, "", "Woke"},
		{"visited wake", map[string]any{"snoozedUntil": "2026-10-19T11:00:00Z", "lastVisitedAt": "2026-10-19T11:01:00Z"}, "", ""},
		{"future wake", map[string]any{"snoozedUntil": "2026-10-19T13:00:00Z"}, "", ""},
		{"settled wake", map[string]any{"snoozedUntil": "2026-10-19T11:00:00Z", "settledOverride": "settled"}, "", ""},
		{"early wake acknowledged before scheduled time", map[string]any{"snoozedAt": "2026-10-19T09:00:00Z", "snoozedUntil": "2026-10-19T11:00:00Z", "latestRunCompletedAt": "2026-10-19T10:00:00Z", "lastVisitedAt": "2026-10-19T10:30:00Z"}, "", ""},
		{"working over wake and done", map[string]any{"status": "running", "snoozedUntil": "2026-10-19T11:00:00Z", "lastVisitedAt": "2026-10-19T10:00:00Z", "latestRunCompletedAt": "2026-10-19T11:30:00Z"}, "", "Working"},
		{"no runtime", map[string]any{"status": "running", "latestRunId": nil, "activeProviderThreadId": nil}, "", ""},
		{"archived", map[string]any{"status": "running", "archivedAt": "2026-10-19T11:00:00Z"}, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			thread := presentedThread(t, tt.fields)
			got := thread.sidebarLabel(now, tt.delegated)
			if got == nil || *got != tt.want {
				t.Fatalf("want %q got %v", tt.want, got)
			}
		})
	}
	legacy := liveThread{}
	if legacy.sidebarLabel(now, "") != nil {
		t.Fatal("legacy metadata fabricated a source label")
	}
}

func TestDelegatedSidebarRequests(t *testing.T) {
	parent := presentedThread(t, map[string]any{"id": "parent", "status": "running"})
	child := func(id, parent, kind string) liveThread {
		return presentedThread(t, map[string]any{"id": id, "status": "waiting", "activityRunStatus": "waiting", "creationSource": "mcp", "lineage": map[string]any{"parentThreadId": parent, "relationshipToParent": "subagent"}, "pendingRuntimeRequest": map[string]any{"kind": kind}})
	}
	input := child("input", "parent", "user_input")
	approval := child("approval", "input", "approval")
	threads := []liveThread{parent, input, approval}
	for range 2 {
		requests := delegatedRequests(threads)
		if requests["parent"] != "approval" || requests["input"] != "approval" {
			t.Fatal(requests)
		}
		threads[1], threads[2] = threads[2], threads[1]
	}
	for _, tt := range []struct {
		name   string
		change map[string]any
	}{
		{"ended child", map[string]any{"activityRunStatus": nil}},
		{"native child", map[string]any{"creationSource": "provider"}},
		{"auth request", map[string]any{"pendingRuntimeRequest": map[string]any{"kind": "auth_refresh"}}},
		{"archived child", map[string]any{"archivedAt": "2026-10-19T11:00:00Z"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw, _ := json.Marshal(input)
			var data map[string]any
			json.Unmarshal(raw, &data)
			for k, v := range tt.change {
				data[k] = v
			}
			mutated := presentedThread(t, data)
			if len(delegatedRequests([]liveThread{parent, mutated})) != 0 {
				t.Fatal("stale/native/auth child propagated")
			}
		})
	}
	input.Lineage.ParentThreadID = ptr("input")
	if len(delegatedRequests([]liveThread{input})) != 0 {
		t.Fatal("cycle propagated")
	}
}

func TestSidebarLabelsFlowThroughPollAndClearOnFailure(t *testing.T) {
	db, c, now := v2Fixture(t)
	v2Thread(t, db, "thread", now)
	f, _ := attachRuntime(t, c)
	f.v2 = true
	fields := map[string]any{"status": "running"}
	for _, label := range []string{"Working", "Input", "Approval", "Limited", "Working", "Waiting", "Done", ""} {
		fields["pendingRuntimeRequest"] = nil
		fields["pendingBackgroundTasks"] = []any{}
		fields["lastErrorClass"] = ""
		fields["lastVisitedAt"] = "2026-10-19T10:00:00Z"
		fields["latestRunCompletedAt"] = nil
		switch label {
		case "Working":
			fields["status"] = "running"
		case "Input":
			fields["pendingRuntimeRequest"] = map[string]any{"kind": "user_input"}
		case "Approval":
			fields["pendingRuntimeRequest"] = map[string]any{"kind": "approval"}
		case "Limited":
			fields["status"] = "failed"
			fields["lastErrorClass"] = "usage_limit"
		case "Waiting":
			fields["status"] = "completed"
			fields["pendingBackgroundTasks"] = []any{map[string]any{"kind": "command"}}
		case "Done":
			fields["status"] = "completed"
			fields["latestRunCompletedAt"] = "2026-10-19T11:00:00Z"
		case "":
			fields["status"] = "completed"
		}
		f.threads = []liveThread{presentedThread(t, fields)}
		snap := c.Poll()
		if snap.Sources[0].State != "healthy" || len(snap.Sessions) != 1 || snap.Sessions[0].T3Status == nil || *snap.Sessions[0].T3Status != label {
			t.Fatalf("%s: %+v", label, snap)
		}
		// Source labels have no effect on the established activity calculation.
		_, active := f.threads[0].v2State()
		if snap.Sessions[0].Active != active {
			t.Fatal("label changed liveness")
		}
	}
	f.status = http.StatusServiceUnavailable
	snap := c.Poll()
	if snap.Sessions[0].T3Status != nil || snap.ActiveSessions != 0 {
		t.Fatal("failed source retained label")
	}
	f.status = http.StatusOK
	fields["status"] = "running"
	f.threads = []liveThread{presentedThread(t, fields)}
	snap = c.Poll()
	if snap.Sessions[0].T3Status == nil || *snap.Sessions[0].T3Status != "Working" {
		t.Fatal("label did not recover")
	}
	f.beforeShell = func() {
		v2Thread(t, db, "new", now)
		f.threads = append(f.threads, presentedThread(t, map[string]any{"id": "new", "status": "failed", "lastErrorClass": "usage_limit"}))
	}
	snap = c.Poll()
	if len(snap.Sessions) != 2 {
		t.Fatal("new thread missing")
	}
	for _, row := range snap.Sessions {
		if strings.HasSuffix(row.ID, ":new") && (row.T3Status == nil || *row.T3Status != "Limited") {
			t.Fatal("live-only label omitted")
		}
	}
}

func TestOlderSidebarCompletionAndSubagentFallback(t *testing.T) {
	var old liveThread
	err := json.Unmarshal([]byte(`{"id":"old","status":"completed","latestRunId":"r","lastVisitedAt":"2026-10-19T10:00:00Z","updatedAt":"2026-10-19T11:00:00Z"}`), &old)
	if err != nil {
		t.Fatal(err)
	}
	old.V2 = true
	if got := old.sidebarLabel(parseTime("2026-10-19T12:00:00Z"), ""); got == nil || *got != "Done" {
		t.Fatal("old server completion fallback missing")
	}
	err = json.Unmarshal([]byte(`{"id":"child","status":"waiting","creationSource":"mcp","lineage":{"parentThreadId":"parent","relationshipToParent":"subagent"},"pendingRuntimeRequest":{"kind":"user_input"}}`), &old)
	if err != nil {
		t.Fatal(err)
	}
	old.V2 = true
	if delegatedRequests([]liveThread{old})["parent"] != "user_input" {
		t.Fatal("older child runtime fallback missing")
	}
}
