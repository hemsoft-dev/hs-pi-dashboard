package t3

import (
	"encoding/json"
	"time"
)

// Match the current SidebarThreadRow, not the older resolveThreadStatusPill.
// Source: pingdotgg/t3code 1451e7cb2303, Sidebar.logic.ts, models.ts,
// threadSettled.ts and threadShell.ts. The fleet's client patch also lets
// background command tasks hold completion, so all known task kinds wait.
// Keep these labels separate from state(): a Waiting badge is not proof of work.
func (t liveThread) sidebarLabel(now time.Time, delegated string) *string {
	label := ""
	if !t.V2 {
		return nil
	}
	if t.ArchivedAt != nil {
		return &label
	}
	approval, input := t.requests(delegated)
	status, runtime := t.sidebarRuntime()
	switch {
	case approval:
		label = "Approval"
	case input:
		label = "Input"
	case runtime && (status == "preparing" || status == "queued" || status == "starting" || status == "running" || status == "waiting"):
		label = "Working"
		if t.Goal != nil && t.Goal.Status == "active" {
			label = "Goal"
		}
	case runtime && status == "idle":
		label = "Waiting"
	case runtime && status == "failed":
		label = "Failed"
		if t.LastErrorClass == "usage_limit" {
			label = "Limited"
		}
	default:
		visited := value(t.LastVisitedAt)
		completed := t.completionAt()
		wake := t.wakeAt(now, approval, input, status, runtime)
		if t.SettledOverride != nil && *t.SettledOverride == "settled" {
			wake = ""
		}
		if wake != "" && (parseTime(visited).IsZero() || parseTime(wake).After(parseTime(visited))) {
			label = "Woke"
		} else if visited != "" && !parseTime(completed).IsZero() &&
			(t.LatestRunID != nil) && (parseTime(visited).IsZero() || parseTime(completed).After(parseTime(visited))) {
			label = "Done"
		}
	}
	return &label
}

func value(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func (t liveThread) requests(delegated string) (approval, input bool) {
	if t.PendingRequest != nil {
		input = t.PendingRequest.Kind == "user_input"
		approval = t.PendingRequest.Kind != "user_input" && t.PendingRequest.Kind != "auth_refresh"
	}
	return approval || delegated == "approval", input || delegated == "user_input"
}

func (t liveThread) sidebarRuntime() (string, bool) {
	park := false
	for _, task := range t.PendingBackground {
		switch task.Kind {
		case "command", "subagent", "monitor", "background_task":
			park = t.Status != "failed"
		}
	}
	if park {
		return "idle", true
	}
	if t.LatestRunID == nil && t.ActiveProviderThreadID == nil {
		return "", false
	}
	status := t.Status
	if t.ActivityRunStatus != "" {
		status = t.ActivityRunStatus
	}
	return status, true
}

func (t liveThread) wakeAt(now time.Time, approval, input bool, status string, runtime bool) string {
	wake := value(t.SnoozedUntil)
	if parseTime(wake).IsZero() {
		return ""
	}
	snoozed := parseTime(value(t.SnoozedAt))
	completed := t.LatestRunID != nil && (t.Status == "idle" || t.Status == "completed")
	// models.ts treats idle's latest run as completed and falls back to
	// updatedAt when older servers omit latestRunCompletedAt.
	completedAt := t.completionAt()
	raised := approval || input || (runtime && status == "failed" &&
		(t.SnoozedAt == nil || parseTime(t.UpdatedAt).After(snoozed))) ||
		(t.SnoozedAt != nil && completed && parseTime(completedAt).After(snoozed))
	if raised {
		if t.SnoozedAt != nil && completed && parseTime(completedAt).After(snoozed) {
			return completedAt
		}
		if runtime {
			return t.UpdatedAt
		}
		return value(t.SnoozedAt)
	}
	if !parseTime(wake).After(now) {
		return wake
	}
	return ""
}

// Omitted activityRunStatus is a legacy fallback; explicit null means that
// the child run is finished even if its old asynchronous question remains.
func (t *liveThread) UnmarshalJSON(data []byte) error {
	type wire liveThread
	var decoded struct {
		*wire
		Activity   json.RawMessage `json:"activityRunStatus"`
		Completion json.RawMessage `json:"latestRunCompletedAt"`
	}
	decoded.wire = (*wire)(t)
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	t.completionPresent = len(decoded.Completion) > 0
	if t.completionPresent {
		if err := json.Unmarshal(decoded.Completion, &t.LatestRunCompletedAt); err != nil {
			return err
		}
	}
	t.activityStatusPresent = len(decoded.Activity) > 0
	if t.activityStatusPresent {
		return json.Unmarshal(decoded.Activity, &t.ActivityRunStatus)
	}
	return nil
}

func delegatedRequests(threads []liveThread) map[string]string {
	requests := map[string]string{}
	byID := map[string]liveThread{}
	for _, t := range threads {
		byID[t.ID] = t
	}
	for _, t := range threads {
		if !t.V2 || t.ArchivedAt != nil || t.PendingRequest == nil || t.PendingRequest.Kind == "auth_refresh" ||
			t.Lineage.Relationship != "subagent" || t.CreationSource == "provider" {
			continue
		}
		live := t.runningTurn() || t.Status == "waiting"
		if t.activityStatusPresent {
			live = t.ActivityRunStatus != ""
		}
		if !live {
			continue
		}
		request := "approval"
		if t.PendingRequest.Kind == "user_input" {
			request = "user_input"
		}
		seen := map[string]bool{t.ID: true}
		for t.Lineage.Relationship == "subagent" && t.Lineage.ParentThreadID != nil {
			parent := *t.Lineage.ParentThreadID
			if seen[parent] {
				break
			}
			seen[parent] = true
			if requests[parent] != "approval" {
				requests[parent] = request
			}
			var ok bool
			t, ok = byID[parent]
			if !ok {
				break
			}
		}
	}
	return requests
}

// models.ts uses updatedAt only when the field is absent on older servers.
func (t liveThread) completionAt() string {
	if t.completionPresent || t.LatestRunCompletedAt != "" {
		return t.LatestRunCompletedAt
	}
	if t.LatestRunID != nil {
		switch t.Status {
		case "idle", "completed", "interrupted", "failed", "cancelled", "rolled_back":
			return t.UpdatedAt
		}
	}
	return ""
}
