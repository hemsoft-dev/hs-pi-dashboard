package t3

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// IngestSnapshot is pmd's T3 snapshot (POST /v1/ingest/t3/{machine}). It is a
// port of project-manager's reference sender, scripts/t3/snapshot.py: the same
// facts from the same tables, with no prompts, messages or tool payloads.
// Values T3 stores as JSON are passed through as they are, so the output matches
// the reference field for field.
type IngestSnapshot struct {
	Environment string          `json:"environment"`
	GeneratedAt string          `json:"generatedAt"`
	Threads     []IngestThread  `json:"threads"`
	Pending     []IngestPending `json:"pending"`
}

// IngestThread is one live (unarchived, undeleted) thread, subagents included.
type IngestThread struct {
	ThreadID       string     `json:"threadId"`
	Title          string     `json:"title"`
	Project        string     `json:"project"`
	Branch         any        `json:"branch"`
	CreationSource any        `json:"creationSource"`
	Root           any        `json:"root"`
	Parent         any        `json:"parent"`
	Model          any        `json:"model"`
	UpdatedAt      string     `json:"updatedAt"`
	LatestRun      *IngestRun `json:"latestRun"`
	Active         bool       `json:"active"`
	Queued         bool       `json:"queued"`
	Settled        bool       `json:"settled"`
	PRs            []IngestPR `json:"prs"`
}

// IngestRun is the thread's highest-ordinal run.
type IngestRun struct {
	Status string `json:"status"`
	At     string `json:"at"`
}

// IngestPR is a pull request the thread knows about.
type IngestPR struct {
	Repo   any  `json:"repo"`
	Number any  `json:"number"`
	State  any  `json:"state"`
	Watch  bool `json:"watch"`
}

// IngestPending is a pending runtime request whose run hasn't finished.
// Question text isn't in the database, so Questions is always empty here.
type IngestPending struct {
	ThreadID  string `json:"threadId"`
	RequestID any    `json:"requestId"`
	Kind      string `json:"kind"`
	CreatedAt string `json:"createdAt"`
	Questions []any  `json:"questions"`
}

var (
	ingestActive   = map[string]bool{"preparing": true, "starting": true, "running": true}
	ingestFinished = map[string]bool{"completed": true, "interrupted": true, "failed": true, "cancelled": true}
	// The request ID carries the run ordinal, URL-encoded 0-3 times.
	requestOrdinal = regexp.MustCompile(`ordinal%(?:25)*3A([0-9]+)`)
)

const ingestLive = "t.archived_at IS NULL AND t.deleted_at IS NULL"

// DefaultIngestDB is the V2 database of the default T3 install.
func DefaultIngestDB() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".t3", "userdata", "statev2.sqlite")
}

// BuildIngestSnapshot reads one T3 V2 database read-only, in one read
// transaction so threads, runs and requests agree. now stamps generatedAt.
func BuildIngestSnapshot(ctx context.Context, path, environment string, now func() time.Time) (IngestSnapshot, error) {
	snap := IngestSnapshot{Environment: environment, Threads: []IngestThread{}, Pending: []IngestPending{}}
	if _, err := os.Stat(path); err != nil {
		return snap, fmt.Errorf("T3 database unavailable: %w", err)
	}
	dsn := "file:" + strings.ReplaceAll(url.QueryEscape(path), "+", "%20") + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return snap, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return snap, err
	}
	defer tx.Rollback()
	snap.GeneratedAt = now().UTC().Format("2006-01-02T15:04:05.000Z")

	var hasPRTable int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE name='projection_thread_pull_requests'").Scan(&hasPRTable); err != nil {
		return snap, err
	}
	runs, err := ingestRuns(ctx, tx)
	if err != nil {
		return snap, fmt.Errorf("T3 runs: %w", err)
	}
	var tablePRs map[string][]IngestPR
	if hasPRTable > 0 {
		if tablePRs, err = ingestTablePRs(ctx, tx); err != nil {
			return snap, fmt.Errorf("T3 pull requests: %w", err)
		}
	}
	if snap.Threads, err = ingestThreads(ctx, tx, runs, tablePRs); err != nil {
		return snap, fmt.Errorf("T3 threads: %w", err)
	}
	if snap.Pending, err = ingestPending(ctx, tx, runs); err != nil {
		return snap, fmt.Errorf("T3 runtime requests: %w", err)
	}
	return snap, tx.Commit()
}

type ingestRunRow struct {
	status  string
	ordinal int64
	at      string
}

func ingestRuns(ctx context.Context, tx *sql.Tx) (map[string][]ingestRunRow, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.thread_id,r.status,r.ordinal,r.requested_at,r.completed_at
 FROM orchestration_v2_projection_runs r JOIN orchestration_v2_projection_threads t ON t.thread_id=r.thread_id
 WHERE `+ingestLive+` ORDER BY r.rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]ingestRunRow{}
	for rows.Next() {
		var id, status, requested string
		var completed sql.NullString
		var ordinal int64
		if err := rows.Scan(&id, &status, &ordinal, &requested, &completed); err != nil {
			return nil, err
		}
		at := requested
		if completed.String != "" {
			at = completed.String
		}
		out[id] = append(out[id], ingestRunRow{status: status, ordinal: ordinal, at: at})
	}
	return out, rows.Err()
}

// ingestTablePRs reads every thread's table PRs at once, in the primary key
// order a per-thread lookup returns.
func ingestTablePRs(ctx context.Context, tx *sql.Tx) (map[string][]IngestPR, error) {
	rows, err := tx.QueryContext(ctx, `SELECT thread_id,repository,number,snapshot_json FROM projection_thread_pull_requests
 ORDER BY thread_id,host,repository,number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]IngestPR{}
	for rows.Next() {
		var id string
		var repo, number any
		var snapshot sql.NullString
		if err := rows.Scan(&id, &repo, &number, &snapshot); err != nil {
			return nil, err
		}
		var state any
		if snapshot.String != "" {
			value, err := decodeJSON(snapshot.String)
			if err != nil {
				return nil, fmt.Errorf("thread %s pull request snapshot: %w", id, err)
			}
			object, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("thread %s pull request snapshot is not an object", id)
			}
			state = object["state"]
		}
		out[id] = append(out[id], IngestPR{Repo: sqlValue(repo), Number: sqlValue(number), State: state})
	}
	return out, rows.Err()
}

func ingestThreads(ctx context.Context, tx *sql.Tx, runs map[string][]ingestRunRow, tablePRs map[string][]IngestPR) ([]IngestThread, error) {
	rows, err := tx.QueryContext(ctx, `SELECT t.thread_id,t.title,COALESCE(p.title,''),t.payload_json,t.updated_at
 FROM orchestration_v2_projection_threads t LEFT JOIN projection_projects p ON p.project_id=t.project_id
 WHERE `+ingestLive+` ORDER BY t.thread_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IngestThread{}
	for rows.Next() {
		var id, project string
		var title, payloadJSON, updated sql.NullString
		if err := rows.Scan(&id, &title, &project, &payloadJSON, &updated); err != nil {
			return nil, err
		}
		payload, err := decodeObject(payloadJSON.String)
		if err != nil {
			return nil, fmt.Errorf("thread %s payload: %w", id, err)
		}
		mine := runs[id]
		lastDone := int64(-1)
		var latest *ingestRunRow
		for i := range mine {
			r := &mine[i]
			if r.status == "completed" && r.ordinal > lastDone {
				lastDone = r.ordinal
			}
			// The first run with the highest ordinal, as Python's max() picks.
			if latest == nil || r.ordinal > latest.ordinal {
				latest = r
			}
		}
		thread := IngestThread{
			ThreadID: id, Title: title.String, Project: project,
			Branch: payload["branch"], CreationSource: payload["creationSource"],
			Root: member(payload["lineage"], "rootThreadId"), Parent: member(payload["lineage"], "parentThreadId"),
			Model:     member(payload["modelSelection"], "model"),
			UpdatedAt: updated.String,
		}
		if latest != nil {
			thread.LatestRun = &IngestRun{Status: latest.status, At: latest.at}
		}
		for _, r := range mine {
			thread.Active = thread.Active || ingestActive[r.status]
			thread.Queued = thread.Queued || (r.status == "queued" && r.ordinal > lastDone)
		}
		if thread.LatestRun != nil && payload["settledOverride"] == "settled" {
			settledAt, _ := payload["settledAt"].(string)
			thread.Settled = settledAt >= thread.LatestRun.At
		}
		thread.PRs = payloadPRs(payload, tablePRs[id])
		out = append(out, thread)
	}
	return out, rows.Err()
}

// payloadPRs takes payload.pullRequests[], linkedPullRequest and
// branchPullRequest (objects only), then the table's rows, dropping exact
// duplicates.
func payloadPRs(payload map[string]any, table []IngestPR) []IngestPR {
	var raw []any
	if list, ok := payload["pullRequests"].([]any); ok {
		raw = append(raw, list...)
	}
	raw = append(raw, payload["linkedPullRequest"], payload["branchPullRequest"])
	all := []IngestPR{}
	for _, item := range raw {
		pr, ok := item.(map[string]any)
		if !ok {
			continue
		}
		all = append(all, IngestPR{Repo: pr["repository"], Number: pr["number"],
			State: member(pr["snapshot"], "state"), Watch: truthy(pr["watch"])})
	}
	all = append(all, table...)
	unique := []IngestPR{}
	seen := map[string]bool{}
	for _, pr := range all {
		key, _ := json.Marshal(pr)
		if !seen[string(key)] {
			seen[string(key)] = true
			unique = append(unique, pr)
		}
	}
	return unique
}

func ingestPending(ctx context.Context, tx *sql.Tx, runs map[string][]ingestRunRow) ([]IngestPending, error) {
	rows, err := tx.QueryContext(ctx, `SELECT r.thread_id,r.kind,r.created_at,r.payload_json FROM orchestration_v2_projection_runtime_requests r
 JOIN orchestration_v2_projection_threads t ON t.thread_id=r.thread_id
 WHERE r.status='pending' AND `+ingestLive+` ORDER BY r.created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IngestPending{}
	for rows.Next() {
		var id, kind, created string
		var payloadJSON sql.NullString
		if err := rows.Scan(&id, &kind, &created, &payloadJSON); err != nil {
			return nil, err
		}
		request, err := decodeObject(payloadJSON.String)
		if err != nil {
			return nil, fmt.Errorf("thread %s request payload: %w", id, err)
		}
		requestID, _ := request["id"].(string)
		if requestRunFinished(requestID, runs[id]) {
			// T3 leaves a request on a finished run pending; it can never be answered.
			continue
		}
		out = append(out, IngestPending{ThreadID: id, RequestID: request["id"], Kind: kind, CreatedAt: created, Questions: []any{}})
	}
	return out, rows.Err()
}

func requestRunFinished(requestID string, runs []ingestRunRow) bool {
	match := requestOrdinal.FindStringSubmatch(requestID)
	if match == nil {
		return false
	}
	ordinal, err := strconv.ParseInt(match[1], 10, 64)
	if err != nil {
		return false
	}
	for _, r := range runs {
		if r.ordinal == ordinal && ingestFinished[r.status] {
			return true
		}
	}
	return false
}

// decodeObject decodes a payload column; an empty column is an empty object.
func decodeObject(text string) (map[string]any, error) {
	if text == "" {
		return map[string]any{}, nil
	}
	value, err := decodeJSON(text)
	if err != nil {
		return nil, err
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, errors.New("not a JSON object")
	}
	return object, nil
}

// decodeJSON keeps numbers as written, so pass-through values re-encode exactly.
func decodeJSON(text string) (any, error) {
	dec := json.NewDecoder(strings.NewReader(text))
	dec.UseNumber()
	var value any
	if err := dec.Decode(&value); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("trailing data after JSON value")
	}
	return value, nil
}

// member reads key from an object; anything else (null, absent) has none.
func member(value any, key string) any {
	object, _ := value.(map[string]any)
	return object[key]
}

// truthy is Python's bool() for a decoded JSON value.
func truthy(value any) bool {
	switch v := value.(type) {
	case nil:
		return false
	case bool:
		return v
	case json.Number:
		f, err := v.Float64()
		return err != nil || f != 0
	case string:
		return v != ""
	case []any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

// sqlValue turns SQLite's byte strings into text for JSON.
func sqlValue(value any) any {
	if b, ok := value.([]byte); ok {
		return string(b)
	}
	return value
}
