package usage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type ActivityEvent struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
}
type Activity struct {
	SourcesAvailable int             `json:"sourcesAvailable"`
	Events           []ActivityEvent `json:"events"`
	Days             int             `json:"days"`
	Scope            string          `json:"scope"`
}

// Only local turn identities/timestamps are read. No prompts, tokens or quota
// estimates are collected. Hashes let the fleet deduplicate shared native turns.
func (f *Fetcher) grokActivity(ctx context.Context) Card {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	now := time.Now()
	card := Card{Key: "grok", Label: "Grok / X", Kind: "activity", FetchedAt: now}
	events := map[string]ActivityEvent{}
	truncated := false
	sources := 0
	cutoff := now.Add(-7 * 24 * time.Hour)
	add := func(id string, at time.Time) {
		if id == "" || at.Before(cutoff) || at.After(now) {
			return
		}
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte("grok:"+id)))
		if _, exists := events[hash]; !exists && len(events) >= 10000 {
			truncated = true
			return
		}
		events[hash] = ActivityEvent{ID: hash, At: at}
	}
	var failures []string
	dbPath := envValue("HS_PI_DASHBOARD_GROK_T3_DB")
	if dbPath == "" {
		dbPath = filepath.Join(f.Home, ".t3", "userdata", "statev2.sqlite")
	}
	if err := readGrokTurns(ctx, dbPath, cutoff, add); err != nil {
		failures = append(failures, "T3 activity unavailable")
	} else {
		sources++
	}
	paths, bounded, err := grokUsageFiles(ctx, filepath.Join(f.Home, ".grok", "sessions"))
	if bounded {
		failures = append(failures, "Grok CLI history scan limited")
	}
	if err != nil || len(paths) == 0 {
		failures = append(failures, "Grok CLI activity unavailable")
	}
	cliAvailable := false
	remainingBytes := 16 << 20
	for _, path := range paths {
		if remainingBytes <= 0 || truncated {
			failures = append(failures, "Grok CLI history scan limited")
			break
		}
		if ctx.Err() != nil {
			failures = append(failures, "activity collection cancelled")
			break
		}
		file, err := os.Open(path)
		if err != nil {
			failures = append(failures, "Grok CLI activity unreadable")
			continue
		}
		data, err := io.ReadAll(io.LimitReader(file, int64(min(4<<20, remainingBytes)+1)))
		_ = file.Close()
		remainingBytes -= len(data)
		if err != nil || len(data) > 4<<20 {
			failures = append(failures, "Grok CLI activity unreadable")
			continue
		}
		var report struct {
			SessionID string `json:"sessionId"`
			Turns     []struct {
				Number  int       `json:"turnNumber"`
				EndedAt time.Time `json:"endedAt"`
			} `json:"turns"`
		}
		if json.Unmarshal(data, &report) != nil || report.SessionID == "" {
			failures = append(failures, "Grok CLI activity unreadable")
			continue
		}
		cliAvailable = true
		for _, turn := range report.Turns {
			if turn.Number > 0 {
				add(fmt.Sprintf("%s:turn:%d", report.SessionID, turn.Number), turn.EndedAt)
			}
		}
	}
	if cliAvailable {
		sources++
	}
	card.Activity = &Activity{SourcesAvailable: sources, Days: 7, Scope: "Observed local T3 + Grok CLI turns; files currently present", Events: []ActivityEvent{}}
	for _, event := range events {
		card.Activity.Events = append(card.Activity.Events, event)
	}
	sort.Slice(card.Activity.Events, func(i, j int) bool { return card.Activity.Events[i].ID < card.Activity.Events[j].ID })
	// Partial coverage is explicit; the observed count is still useful.
	if truncated {
		failures = append(failures, "activity limited to 10000 observed turns")
	}
	if len(failures) > 0 {
		card.Error = joinActivityFailures(failures)
	}
	return card
}

func joinActivityFailures(failures []string) string {
	seen := map[string]bool{}
	result := ""
	for _, failure := range failures {
		if !seen[failure] {
			if result != "" {
				result += "; "
			}
			result += failure
			seen[failure] = true
		}
	}
	return result
}

func readGrokTurns(ctx context.Context, path string, cutoff time.Time, add func(string, time.Time)) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	dsn := "file:" + strings.ReplaceAll(url.QueryEscape(path), "+", "%20") + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	c, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	// The newer projection uses native turn columns. Older supported V2
	// schemas retain turn metadata in payload_json and link via provider thread.
	columns, err := db.QueryContext(c, "PRAGMA table_info(orchestration_v2_projection_provider_turns)")
	if err != nil {
		return err
	}
	hasStarted := false
	for columns.Next() {
		var cid, notnull, pk int
		var name, kind string
		var def any
		if err := columns.Scan(&cid, &name, &kind, &notnull, &def, &pk); err != nil {
			columns.Close()
			return err
		}
		if name == "started_at" {
			hasStarted = true
		}
	}
	if err := columns.Err(); err != nil {
		columns.Close()
		return err
	}
	columns.Close()
	query := `SELECT t.provider_turn_id,
	 COALESCE(json_extract(t.payload_json,'$.nativeTurnRef.nativeId'),''),
	 t.started_at
	 FROM orchestration_v2_projection_provider_turns t
	 JOIN orchestration_v2_projection_nodes n ON n.node_id=t.node_id
	 JOIN orchestration_v2_projection_runs r ON r.run_id=n.run_id
	 WHERE r.provider='grok' AND julianday(t.started_at)>=julianday(?) LIMIT 10001`
	if !hasStarted {
		query = `WITH metadata AS (
		 SELECT pt.provider_thread_id || ':turn:' || t.ordinal AS id,
		 COALESCE(json_extract(t.payload_json,'$.nativeTurnRef.nativeId'),'') AS native,
		 COALESCE(json_extract(t.payload_json,'$.startedAt'),json_extract(t.payload_json,'$.completedAt'),'') AS stamp
		 FROM orchestration_v2_projection_provider_turns t
		 JOIN orchestration_v2_projection_provider_threads pt ON pt.provider_thread_id=t.provider_thread_id
		 WHERE COALESCE(pt.driver,pt.provider)='grok')
		 SELECT id,native,stamp FROM metadata
		 WHERE stamp='' OR julianday(stamp)>=julianday(?) LIMIT 10001`
	}
	rows, err := db.QueryContext(c, query, cutoff.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id, native, stamp string
		if err := rows.Scan(&id, &native, &stamp); err != nil {
			return err
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return errors.New("T3 activity timestamp unavailable")
		}
		if native != "" {
			id = native
		}
		add(id, at)
	}
	return rows.Err()
}

// Stream directory entries rather than globbing an unbounded retained history.
// Session layout is workspace/session/usage.json; symlink directories are skipped.
func grokUsageFiles(ctx context.Context, root string) ([]string, bool, error) {
	var paths []string
	entries := 0
	bounded := false
	var walk func(string, int) error
	walk = func(dir string, depth int) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		f, err := os.Open(dir)
		if err != nil {
			return err
		}
		defer f.Close()
		for {
			batch, err := f.ReadDir(32)
			for _, entry := range batch {
				entries++
				if entries > 4096 || len(paths) >= 256 {
					bounded = true
					return nil
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				path := filepath.Join(dir, entry.Name())
				if entry.IsDir() && depth < 2 {
					if err := walk(path, depth+1); err != nil {
						return err
					}
					if bounded {
						return nil
					}
				} else if depth == 2 && entry.Name() == "usage.json" && entry.Type().IsRegular() {
					paths = append(paths, path)
				}
			}
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	err := walk(root, 0)
	return paths, bounded, err
}
