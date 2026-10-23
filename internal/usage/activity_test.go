package usage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGrokActivityDeduplicatesCLIAndNativeT3Turn(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".t3", "userdata")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "statev2.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, statement := range []string{
		"CREATE TABLE orchestration_v2_projection_runs(run_id TEXT,provider TEXT)",
		"CREATE TABLE orchestration_v2_projection_nodes(node_id TEXT,run_id TEXT)",
		"CREATE TABLE orchestration_v2_projection_provider_turns(provider_turn_id TEXT,node_id TEXT,started_at TEXT,payload_json TEXT)",
		"INSERT INTO orchestration_v2_projection_runs VALUES('run','grok')",
		"INSERT INTO orchestration_v2_projection_nodes VALUES('node','run')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if _, err := db.Exec("INSERT INTO orchestration_v2_projection_provider_turns VALUES(?,?,?,?)", "provider-turn", "node", now.Add(-time.Hour).Format(time.RFC3339), `{"nativeTurnRef":{"nativeId":"session:turn:1"}}`); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(home, ".grok", "sessions", "workspace", "session")
	if err := os.MkdirAll(cli, 0755); err != nil {
		t.Fatal(err)
	}
	report := fmt.Sprintf(`{"sessionId":"session","turns":[{"turnNumber":1,"endedAt":%q},{"turnNumber":2,"endedAt":%q},{"turnNumber":3,"endedAt":%q}]}`, now.Add(-time.Hour).Format(time.RFC3339), now.Add(-8*24*time.Hour).Format(time.RFC3339), now.Add(time.Hour).Format(time.RFC3339))
	if err := os.WriteFile(filepath.Join(cli, "usage.json"), []byte(report), 0600); err != nil {
		t.Fatal(err)
	}
	card := (&Fetcher{Home: home}).grokActivity(context.Background())
	if card.Error != "" || card.Activity == nil || len(card.Activity.Events) != 1 || card.Activity.SourcesAvailable != 2 || card.Gauges != nil {
		t.Fatal(card)
	}
	if strings.Contains(card.Activity.Events[0].ID, "session") {
		t.Fatal("raw identity exposed")
	}
}

func TestGrokNoSourcesIsUnavailableRatherThanMeasuredZero(t *testing.T) {
	card := (&Fetcher{Home: t.TempDir()}).grokActivity(context.Background())
	if card.Error == "" || card.Activity == nil || card.Activity.SourcesAvailable != 0 || len(card.Activity.Events) != 0 {
		t.Fatal(card)
	}
}
