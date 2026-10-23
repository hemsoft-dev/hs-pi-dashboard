package usage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
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
		if len(events) >= 10000 {
			truncated = true
			return
		}
		hash := fmt.Sprintf("%x", sha256.Sum256([]byte("grok:"+id)))
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
	paths, err := filepath.Glob(filepath.Join(f.Home, ".grok", "sessions", "*", "*", "usage.json"))
	if err != nil || len(paths) == 0 {
		failures = append(failures, "Grok CLI activity unavailable")
	}
	cliAvailable := false
	for _, path := range paths {
		if ctx.Err() != nil {
			failures = append(failures, "activity collection cancelled")
			break
		}
		file, err := os.Open(path)
		if err != nil {
			failures = append(failures, "Grok CLI activity unreadable")
			continue
		}
		data, err := io.ReadAll(io.LimitReader(file, (4<<20)+1))
		_ = file.Close()
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
	rows, err := db.QueryContext(c, `SELECT t.provider_turn_id,
	 COALESCE(json_extract(t.payload_json,'$.nativeTurnRef.nativeId'),''),
	 t.started_at
	 FROM orchestration_v2_projection_provider_turns t
	 JOIN orchestration_v2_projection_nodes n ON n.node_id=t.node_id
	 JOIN orchestration_v2_projection_runs r ON r.run_id=n.run_id
	 WHERE r.provider='grok' AND julianday(t.started_at)>=julianday(?)`, cutoff.UTC().Format(time.RFC3339Nano))
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
			continue
		}
		if native != "" {
			id = native
		}
		add(id, at)
	}
	return rows.Err()
}
