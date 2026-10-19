// Package t3 reads T3 Code's local SQLite projections and authenticated live
// states. It reports metadata and counters, never prompts or tool payloads.
package t3

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/sessions"
	_ "modernc.org/sqlite"
)

// Collector owns the incremental activity cache for one T3 environment.
type Collector struct {
	path        string
	runtime     *runtimeSource
	defaultPath bool
	window      time.Duration
	now         func() time.Time
	mu          sync.Mutex
	file        os.FileInfo
	rowID       int64
	activities  map[string]*activity
	last        sessions.Snapshot
}

type activity struct {
	tools map[string]bool
	usage *sessions.T3Usage
	at    time.Time
}

// New uses the default T3 userdata path when path is empty. Each configured
// environment gets its own collector and namespaced thread identities.
func New(path string, window time.Duration, binary string) *Collector {
	defaultPath := path == ""
	if path == "" {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, ".t3", "userdata", "state.sqlite")
	}
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[2:])
	}
	if absolute, err := filepath.Abs(path); err == nil {
		path = absolute
	}
	if window <= 0 {
		window = sessions.DefaultActiveWindow
	}
	return &Collector{path: path, runtime: newRuntime(path, binary), defaultPath: defaultPath, window: window, now: time.Now, activities: map[string]*activity{}}
}

// Poll reads a consistent projection snapshot. A failed source retains cached
// metadata but clears activity so it cannot inflate Pulse or look live.
func (c *Collector) Poll() sessions.Snapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.defaultPath {
		// V2 copies the legacy database once; the files then evolve independently.
		// Never present that frozen V1 copy as the default live environment.
		v2 := filepath.Join(filepath.Dir(c.path), "statev2.sqlite")
		if _, err := os.Stat(v2); err == nil || !os.IsNotExist(err) {
			c.path = v2
		}
	}
	info, err := os.Stat(c.path)
	if err != nil {
		return c.unavailable(now, "T3 database unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	// Escape the complete native path, including Windows drive letters and
	// backslashes. URL.Path would render drive paths as opaque file URIs.
	dsn := "file:" + strings.ReplaceAll(url.QueryEscape(c.path), "+", "%20") + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return c.unavailable(now, "T3 database could not be opened read-only")
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return c.unavailable(now, fmt.Sprintf("T3 database is locked or unreadable: %v", err))
	}
	defer tx.Rollback()
	var v2 int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='orchestration_v2_projection_threads'").Scan(&v2); err != nil {
		return c.unavailable(now, "T3 database schema could not be inspected")
	}
	var rows []sessions.Summary
	var rowID int64
	next := map[string]*activity{}
	if v2 > 0 {
		rows, err = c.readV2Threads(ctx, tx)
		if err != nil {
			return c.unavailable(now, "T3 V2 projections unavailable: incompatible schema or locked database")
		}
	} else {
		if filepath.Base(c.path) == "statev2.sqlite" {
			return c.unavailable(now, "T3 V2 projections unavailable: incompatible schema")
		}
		// V1 versions before event sequence projection leave sequence NULL.
		if err = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(rowid),0) FROM projection_thread_activities").Scan(&rowID); err != nil {
			return c.unavailable(now, "T3 projections unavailable: incompatible schema or locked database")
		}
		old := c.activities
		after := c.rowID
		if c.file == nil || !os.SameFile(c.file, info) || rowID < after {
			old = nil
			after = 0
		}
		next = cloneActivities(old)
		if err = readActivities(ctx, tx, after, next); err != nil {
			return c.unavailable(now, "T3 activity projections unavailable")
		}
		rows, err = c.readThreads(ctx, tx, next, now)
		if err != nil {
			return c.unavailable(now, "T3 thread projections unavailable: incompatible schema or locked database")
		}
	}
	if err = tx.Commit(); err != nil {
		return c.unavailable(now, "T3 database snapshot interrupted")
	}
	runtimeError := ""
	if c.runtime != nil {
		c.runtime.v2 = v2 > 0
		live, err := c.runtime.snapshot(ctx)
		if err != nil {
			runtimeError = err.Error()
			for i := range rows {
				rows[i].Active = false
				rows[i].Status = "unavailable"
			}
			// Keep a thread first discovered through the live API visible if
			// that API fails before SQLite catches up.
			seen := map[string]bool{}
			for _, row := range rows {
				seen[row.ID] = true
			}
			for _, row := range c.last.Sessions {
				if !seen[row.ID] {
					row.Active, row.Status = false, "unavailable"
					rows = append(rows, row)
					seen[row.ID] = true
				}
			}
		} else {
			states := map[string]liveThread{}
			for _, thread := range live {
				states[thread.ID] = thread
			}
			for i := range rows {
				id := strings.TrimPrefix(rows[i].ID, c.prefix())
				if thread, ok := states[id]; ok {
					rows[i].Status, rows[i].Active = thread.state()
					if thread.V2 {
						start := thread.ActivityRunStartedAt
						if start == "" {
							start = thread.LatestRunStartedAt
						}
						if start != "" {
							rows[i].TurnStartedAt = optionalTime(start)
						}
						if thread.LatestRunCompletedAt != "" {
							rows[i].TurnCompletedAt = optionalTime(thread.LatestRunCompletedAt)
						}
					}
					if thread.runningTurn() {
						rows[i].TurnCompletedAt = nil
					}
					delete(states, id)
				} else {
					rows[i].Active = false
					rows[i].Status = "stopped"
				}
			}
			// A thread may start after the SQLite transaction. Include it from
			// the live snapshot now; the next poll fills in persisted counters.
			for _, thread := range live {
				if _, ok := states[thread.ID]; !ok {
					continue
				}
				status, active := thread.state()
				provider := thread.ModelSelection.Instance
				if thread.Session != nil && thread.Session.ProviderName != "" {
					provider = thread.Session.ProviderName
				}
				effort, _ := modelEffort(thread.ModelSelection.Options)
				// The shell API deliberately omits provider resume identifiers.
				// Re-read them after the earlier transaction so a newly started
				// Claude thread can still deduplicate its native transcript.
				var sessionID, providerID string
				var identityErr error
				if v2 > 0 {
					identityErr = db.QueryRowContext(ctx, v2IdentityQuery, thread.ID).Scan(&providerID)
					sessionID = providerID
				} else {
					identityErr = db.QueryRowContext(ctx, `SELECT COALESCE(provider_session_id,''),COALESCE(provider_thread_id,'') FROM projection_thread_sessions WHERE thread_id=?`, thread.ID).Scan(&sessionID, &providerID)
				}
				if identityErr != nil && !errors.Is(identityErr, sql.ErrNoRows) {
					return c.unavailable(now, "T3 provider identities unavailable")
				}
				rows = append(rows, sessions.Summary{ID: c.prefix() + thread.ID, Source: "t3", Name: thread.Title,
					Project: thread.Project, Cwd: thread.Cwd, StartedAt: parseTime(thread.CreatedAt), LastActivity: parseTime(thread.UpdatedAt),
					Provider: provider, ProviderSessionID: sessionID, ProviderThreadID: providerID,
					Model: thread.ModelSelection.Model, ThinkingLevel: effort, Status: status, Active: active,
					TurnStartedAt: optionalTime(thread.ActivityRunStartedAt), TurnCompletedAt: optionalTime(thread.LatestRunCompletedAt)})
			}
		}
	}
	seen := map[string]bool{}
	stale := 0
	for _, row := range rows {
		seen[strings.TrimPrefix(row.ID, c.prefix())] = true
		if row.Status == "stale" {
			stale++
		}
	}
	for id := range next {
		if !seen[id] {
			delete(next, id)
		}
	}
	c.file, c.rowID, c.activities = info, rowID, next
	health := sessions.SourceHealth{Source: "t3", Path: c.path, State: "healthy", LastSuccessAt: now}
	if runtimeError != "" {
		health.State, health.Error = "unavailable", runtimeError
		if len(c.last.Sources) > 0 {
			health.LastSuccessAt = c.last.Sources[0].LastSuccessAt
		} else {
			health.LastSuccessAt = time.Time{}
		}
	} else if stale > 0 {
		health.State = "stale"
		health.Error = fmt.Sprintf("%d quiet running thread(s); liveness unavailable", stale)
	}
	snap := sessions.Snapshot{GeneratedAt: now, Sessions: rows, Sources: []sessions.SourceHealth{health}}
	for _, row := range rows {
		if row.Active {
			snap.ActiveSessions++
		}
	}
	c.last = snap
	return snap
}

func (c *Collector) prefix() string {
	path := c.path
	// V2 imports the same app thread IDs from V1; keep their dashboard identity.
	if filepath.Base(path) == "statev2.sqlite" {
		path = filepath.Join(filepath.Dir(path), "state.sqlite")
	}
	sum := sha256.Sum256([]byte(path))
	return fmt.Sprintf("t3:%x:", sum[:8])
}

func (c *Collector) unavailable(now time.Time, reason string) sessions.Snapshot {
	snap := sessions.Snapshot{GeneratedAt: now, Sources: []sessions.SourceHealth{{Source: "t3", Path: c.path, State: "unavailable", Error: reason}}}
	if len(c.last.Sources) > 0 {
		snap.Sources[0].LastSuccessAt = c.last.Sources[0].LastSuccessAt
	}
	for _, row := range c.last.Sessions {
		row.Active, row.Status = false, "unavailable"
		snap.Sessions = append(snap.Sessions, row)
	}
	return snap
}

func cloneActivities(old map[string]*activity) map[string]*activity {
	out := make(map[string]*activity, len(old))
	for id, a := range old {
		copied := &activity{tools: make(map[string]bool, len(a.tools)), usage: a.usage, at: a.at}
		for tool := range a.tools {
			copied.tools[tool] = true
		}
		out[id] = copied
	}
	return out
}

func readActivities(ctx context.Context, tx *sql.Tx, after int64, cache map[string]*activity) error {
	rows, err := tx.QueryContext(ctx, `SELECT a.thread_id, a.activity_id, a.kind,
 CASE WHEN a.kind='context-window.updated' THEN a.payload_json
 ELSE json_object('toolCallId',json_extract(a.payload_json,'$.toolCallId')) END, a.created_at
 FROM projection_thread_activities a JOIN projection_threads t ON t.thread_id=a.thread_id
 WHERE a.rowid > ? AND t.deleted_at IS NULL
 AND (a.kind='tool.started' OR a.rowid IN (
 SELECT MAX(rowid) FROM projection_thread_activities WHERE rowid > ? AND kind='context-window.updated' GROUP BY thread_id))
 ORDER BY a.rowid`, after, after)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, activityID, kind, payload, created string
		if err := rows.Scan(&id, &activityID, &kind, &payload, &created); err != nil {
			return err
		}
		a := cache[id]
		if a == nil {
			a = &activity{tools: map[string]bool{}}
			cache[id] = a
		}
		at := parseTime(created)
		if at.After(a.at) {
			a.at = at
		}
		if kind == "context-window.updated" {
			var p struct {
				Input     *int64 `json:"inputTokens"`
				Output    *int64 `json:"outputTokens"`
				Total     *int64 `json:"totalProcessedTokens"`
				Cache     *int64 `json:"cachedInputTokens"`
				Reasoning *int64 `json:"reasoningOutputTokens"`
				Used      *int64 `json:"usedTokens"`
				Max       *int64 `json:"maxTokens"`
			}
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				return fmt.Errorf("invalid context counters: %w", err)
			}
			a.usage = &sessions.T3Usage{InputTokens: p.Input, OutputTokens: p.Output, TotalTokens: p.Total, CacheReadTokens: p.Cache, ReasoningTokens: p.Reasoning, ContextTokens: p.Used, ContextLimit: p.Max}
		} else {
			var p struct {
				ID string `json:"toolCallId"`
			}
			if err := json.Unmarshal([]byte(payload), &p); err != nil {
				return fmt.Errorf("invalid tool identity: %w", err)
			}
			if p.ID != "" {
				a.tools[p.ID] = true
			} else if kind == "tool.started" {
				a.tools[activityID] = true
			}
		}
	}
	return rows.Err()
}

const threadsQuery = `SELECT t.thread_id, t.title, p.title,
 COALESCE(t.worktree_path,p.workspace_root,''), t.created_at,t.updated_at,
 COALESCE(t.archived_at,''),t.model_selection_json,
 t.pending_approval_count+t.pending_user_input_count,
 COALESCE(s.provider_name,''),COALESCE(s.provider_session_id,''),COALESCE(s.provider_thread_id,''),
 COALESCE(s.status,''),COALESCE(s.active_turn_id,''),COALESCE(s.last_error,''),COALESCE(s.updated_at,''),
 COALESCE(r.last_seen_at,''),COALESCE(v.state,''),COALESCE(v.started_at,v.requested_at,''),COALESCE(v.completed_at,''),
 (SELECT COUNT(*) FROM projection_thread_messages m WHERE m.thread_id=t.thread_id AND m.role IN ('user','assistant'))
 FROM projection_threads t JOIN projection_projects p ON p.project_id=t.project_id
 LEFT JOIN projection_thread_sessions s ON s.thread_id=t.thread_id
 LEFT JOIN provider_session_runtime r ON r.thread_id=t.thread_id
 LEFT JOIN projection_turns v ON v.rowid = CASE WHEN s.status='starting' THEN
 (SELECT rowid FROM projection_turns WHERE thread_id=t.thread_id AND state='pending' ORDER BY requested_at DESC,rowid DESC LIMIT 1)
 ELSE (SELECT rowid FROM projection_turns WHERE thread_id=t.thread_id AND turn_id=COALESCE(NULLIF(s.active_turn_id,''),t.latest_turn_id) LIMIT 1) END
 WHERE t.deleted_at IS NULL ORDER BY t.thread_id`

func (c *Collector) readThreads(ctx context.Context, tx *sql.Tx, cache map[string]*activity, now time.Time) ([]sessions.Summary, error) {
	rows, err := tx.QueryContext(ctx, threadsQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []sessions.Summary
	for rows.Next() {
		var id, created, updated, archived, model, providerStatus, turnID, lastError, sessionAt, runtimeAt, turnState, turnStart, turnEnd string
		var pending int
		var row sessions.Summary
		if err := rows.Scan(&id, &row.Name, &row.Project, &row.Cwd, &created, &updated, &archived, &model, &pending,
			&row.Provider, &row.ProviderSessionID, &row.ProviderThreadID, &providerStatus, &turnID, &lastError, &sessionAt,
			&runtimeAt, &turnState, &turnStart, &turnEnd, &row.MessageCount); err != nil {
			return nil, err
		}
		row.ID, row.Source = c.prefix()+id, "t3"
		row.StartedAt, row.LastActivity = parseTime(created), parseTime(updated)
		var selection struct {
			Instance string          `json:"instanceId"`
			Model    string          `json:"model"`
			Options  json.RawMessage `json:"options"`
		}
		if err := json.Unmarshal([]byte(model), &selection); err != nil {
			return nil, fmt.Errorf("invalid model selection: %w", err)
		}
		row.Model = selection.Model
		if row.Provider == "" {
			row.Provider = selection.Instance
		}
		row.ThinkingLevel, err = modelEffort(selection.Options)
		if err != nil {
			return nil, err
		}

		for _, stamp := range []string{sessionAt, runtimeAt, turnStart, turnEnd} {
			at := parseTime(stamp)
			if at.After(row.LastActivity) {
				row.LastActivity = at
			}
		}
		if at := parseTime(turnStart); !at.IsZero() {
			row.TurnStartedAt = &at
		}
		if at := parseTime(turnEnd); !at.IsZero() {
			row.TurnCompletedAt = &at
		}
		if a := cache[id]; a != nil {
			row.ToolCallCount, row.T3Usage = len(a.tools), a.usage
			if a.at.After(row.LastActivity) {
				row.LastActivity = a.at
			}
		}
		row.Status = "idle"
		activeTurn := providerStatus == "running" && (turnState == "running" || turnState == "pending" ||
			(turnID != "" && turnState == ""))
		switch {
		case lastError != "" || providerStatus == "error" || turnState == "error":
			row.Status = "error"
		case archived != "" || providerStatus == "stopped" || turnState == "interrupted":
			row.Status = "stopped"
		case pending > 0:
			row.Status = "waiting"
		case providerStatus == "starting":
			row.Status = "connecting"
		case activeTurn:
			// A resumed T3 turn can retain an earlier completed_at value.
			// A nonterminal turn state takes precedence over that timestamp.
			row.Status = "working"
		}
		if row.Status == "working" || row.Status == "waiting" || row.Status == "connecting" {
			if activeTurn || providerStatus == "starting" {
				row.TurnCompletedAt = nil
			}
			age := now.Sub(row.LastActivity)
			row.Active = age >= 0 && age <= c.window
			if !row.Active {
				row.Status = "stale"
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// T3 migrated model options from an object to an array of id/value pairs.
// Decode either encoding while ignoring provider options unrelated to effort.
func modelEffort(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return "", nil
	}
	values := map[string]json.RawMessage{}
	if strings.HasPrefix(strings.TrimSpace(string(raw)), "{") {
		if err := json.Unmarshal(raw, &values); err != nil {
			return "", err
		}
		normalized := make(map[string]json.RawMessage, len(values))
		for id, value := range values {
			normalized[strings.TrimSpace(id)] = value
		}
		values = normalized
	} else {
		var options []struct {
			ID    string          `json:"id"`
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(raw, &options); err != nil {
			return "", err
		}
		for _, option := range options {
			values[strings.TrimSpace(option.ID)] = option.Value
		}
	}
	for _, id := range []string{"reasoningEffort", "effort"} {
		var effort string
		if json.Unmarshal(values[id], &effort) == nil && effort != "" {
			return strings.TrimSpace(effort), nil
		}
	}
	return "", nil
}

func parseTime(s string) time.Time {
	at, _ := time.Parse(time.RFC3339Nano, s)
	return at
}
