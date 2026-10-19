package t3

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
)

// Select only metadata from V2 projections. Messages, tool inputs/outputs and
// event payloads never cross the SQLite boundary. The legacy tables retained in
// statev2.sqlite are an import snapshot, not a source of current threads.
const v2ThreadsQuery = `SELECT t.thread_id,t.title,COALESCE(p.title,''),
 COALESCE(json_extract(t.payload_json,'$.worktreePath'),p.workspace_root,''),
 t.created_at,t.updated_at,COALESCE(t.archived_at,''),
 COALESCE(json_extract(t.payload_json,'$.modelSelection'),'{}'),
 COALESCE(pt.driver,t.default_provider),
 COALESCE(json_extract(pt.payload_json,'$.nativeThreadRef.nativeId'),''),
 COALESCE(json_extract(r.payload_json,'$.startedAt'),r.requested_at,''),COALESCE(r.completed_at,''),
 (SELECT COUNT(*) FROM orchestration_v2_projection_messages m WHERE m.thread_id=t.thread_id AND m.role IN ('user','assistant')),
 (SELECT COUNT(*) FROM orchestration_v2_projection_nodes n WHERE n.thread_id=t.thread_id AND n.kind='tool_call'),
 json_object(
 'inputTokens',json_extract(turn.payload_json,'$.tokenUsage.inputTokens'),
 'outputTokens',json_extract(turn.payload_json,'$.tokenUsage.outputTokens'),
 'totalTokens',json_extract(turn.payload_json,'$.tokenUsage.totalProcessedTokens'),
 'cacheReadTokens',json_extract(turn.payload_json,'$.tokenUsage.cachedInputTokens'),
 'reasoningTokens',json_extract(turn.payload_json,'$.tokenUsage.reasoningOutputTokens'),
 'contextTokens',COALESCE(json_extract(pt.payload_json,'$.contextUsage.usedTokens'),json_extract(turn.payload_json,'$.tokenUsage.usedTokens')),
 'contextLimit',COALESCE(json_extract(pt.payload_json,'$.contextUsage.maxTokens'),json_extract(turn.payload_json,'$.tokenUsage.maxTokens')))
 FROM orchestration_v2_projection_threads t
 LEFT JOIN projection_projects p ON p.project_id=t.project_id
 LEFT JOIN orchestration_v2_projection_provider_threads pt ON pt.provider_thread_id=t.active_provider_thread_id
 LEFT JOIN orchestration_v2_projection_runs r ON r.rowid=(
 SELECT rowid FROM orchestration_v2_projection_runs WHERE thread_id=t.thread_id ORDER BY ordinal DESC,rowid DESC LIMIT 1)
 LEFT JOIN orchestration_v2_projection_provider_turns turn ON turn.rowid=(
 SELECT rowid FROM orchestration_v2_projection_provider_turns WHERE provider_thread_id=t.active_provider_thread_id ORDER BY ordinal DESC,rowid DESC LIMIT 1)
 WHERE t.deleted_at IS NULL ORDER BY t.thread_id`

const v2IdentityQuery = `SELECT COALESCE(json_extract(pt.payload_json,'$.nativeThreadRef.nativeId'),'')
 FROM orchestration_v2_projection_threads t
 LEFT JOIN orchestration_v2_projection_provider_threads pt ON pt.provider_thread_id=t.active_provider_thread_id
 WHERE t.thread_id=? AND t.deleted_at IS NULL`

func (c *Collector) readV2Threads(ctx context.Context, tx *sql.Tx) ([]sessions.Summary, error) {
	rows, err := tx.QueryContext(ctx, v2ThreadsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessions.Summary
	for rows.Next() {
		var row sessions.Summary
		var id, created, updated, archived, model, started, completed, usage string
		if err := rows.Scan(&id, &row.Name, &row.Project, &row.Cwd, &created, &updated, &archived,
			&model, &row.Provider, &row.ProviderThreadID, &started, &completed, &row.MessageCount, &row.ToolCallCount, &usage); err != nil {
			return nil, err
		}
		var selection struct {
			Model   string          `json:"model"`
			Options json.RawMessage `json:"options"`
		}
		if err := json.Unmarshal([]byte(model), &selection); err != nil {
			return nil, err
		}
		row.Model = selection.Model
		row.ThinkingLevel, err = modelEffort(selection.Options)
		if err != nil {
			return nil, err
		}
		var counters sessions.T3Usage
		if err := json.Unmarshal([]byte(usage), &counters); err != nil {
			return nil, err
		}
		if counters != (sessions.T3Usage{}) {
			row.T3Usage = &counters
		}
		row.ID, row.Source = c.prefix()+id, "t3"
		row.ProviderSessionID = row.ProviderThreadID
		row.StartedAt, row.LastActivity = parseTime(created), parseTime(updated)
		row.TurnStartedAt, row.TurnCompletedAt = optionalTime(started), optionalTime(completed)
		// Persisted work is never liveness authority. The verified runtime below
		// supplies current states, including long quiet turns and monitoring.
		row.Status = "idle"
		if archived != "" {
			row.Status = "stopped"
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func optionalTime(value string) *time.Time {
	at := parseTime(value)
	if at.IsZero() {
		return nil
	}
	return &at
}
