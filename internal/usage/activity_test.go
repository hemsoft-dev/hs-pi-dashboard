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

func TestGrokActivitySupportedPayloadV2Schema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "statev2.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	// These columns mirror internal/t3/v2_test.go's supported projection.
	for _, statement := range []string{
		"CREATE TABLE orchestration_v2_projection_provider_threads(provider_thread_id TEXT PRIMARY KEY,thread_id TEXT,driver TEXT,payload_json TEXT,provider TEXT)",
		"CREATE TABLE orchestration_v2_projection_provider_turns(provider_thread_id TEXT,ordinal INTEGER,payload_json TEXT)",
		"INSERT INTO orchestration_v2_projection_provider_threads VALUES('pt','thread','grok','{}','grok')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	payload := fmt.Sprintf(`{"startedAt":%q,"nativeTurnRef":{"nativeId":"session:turn:1"}}`, now.Format(time.RFC3339Nano))
	if _, err := db.Exec("INSERT INTO orchestration_v2_projection_provider_turns VALUES('pt',1,?)", payload); err != nil {
		t.Fatal(err)
	}
	count := 0
	if err := readGrokTurns(context.Background(), path, now.Add(-time.Hour), func(id string, at time.Time) {
		count++
		if id != "session:turn:1" || !at.Equal(now) {
			t.Fatal(id, at)
		}
	}); err != nil || count != 1 {
		t.Fatal(err, count)
	}
	// Missing historical timestamps must stay explicit rather than using a
	// thread update time as an invented turn timestamp.
	if _, err := db.Exec("UPDATE orchestration_v2_projection_provider_turns SET payload_json='{}'"); err != nil {
		t.Fatal(err)
	}
	if err := readGrokTurns(context.Background(), path, now.Add(-time.Hour), func(string, time.Time) {}); err == nil {
		t.Fatal("invented unknown turn time")
	}
}

func TestGrokHistoryDiscoveryHasAggregateBound(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 300; i++ {
		dir := filepath.Join(root, "workspace", fmt.Sprint(i))
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "usage.json"), []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	paths, bounded, err := grokUsageFiles(context.Background(), root)
	if err != nil || !bounded || len(paths) != 256 {
		t.Fatal(len(paths), bounded, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := grokUsageFiles(ctx, root); err == nil {
		t.Fatal("cancelled scan continued")
	}
}

func TestGrokFinishedTurnsUseCompletionAcrossSevenDayCutoff(t *testing.T) {
	for _, layout := range []string{"payload", "native", "native-payload"} {
		t.Run(layout, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "statev2.sqlite")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			start := time.Now().UTC().Add(-8 * 24 * time.Hour)
			end := time.Now().UTC().Add(-time.Hour)
			payload := fmt.Sprintf(`{"startedAt":%q,"completedAt":%q,"nativeTurnRef":{"nativeId":"finished"}}`, start.Format(time.RFC3339Nano), end.Format(time.RFC3339Nano))
			var statements []string
			switch layout {
			case "payload":
				statements = []string{"CREATE TABLE orchestration_v2_projection_provider_threads(provider_thread_id TEXT,driver TEXT,provider TEXT,payload_json TEXT)", "CREATE TABLE orchestration_v2_projection_provider_turns(provider_thread_id TEXT,ordinal INTEGER,payload_json TEXT)", "INSERT INTO orchestration_v2_projection_provider_threads VALUES('pt','grok','grok','{}')"}
			default:
				columns := ""
				if layout == "native" {
					columns = ",completed_at TEXT"
				}
				statements = []string{"CREATE TABLE orchestration_v2_projection_runs(run_id TEXT,provider TEXT)", "CREATE TABLE orchestration_v2_projection_nodes(node_id TEXT,run_id TEXT)", "CREATE TABLE orchestration_v2_projection_provider_turns(provider_turn_id TEXT,node_id TEXT,started_at TEXT,payload_json TEXT" + columns + ")", "INSERT INTO orchestration_v2_projection_runs VALUES('run','grok')", "INSERT INTO orchestration_v2_projection_nodes VALUES('node','run')"}
			}
			for _, statement := range statements {
				if _, err := db.Exec(statement); err != nil {
					t.Fatal(err)
				}
			}
			switch layout {
			case "payload":
				_, err = db.Exec("INSERT INTO orchestration_v2_projection_provider_turns VALUES('pt',1,?)", payload)
			case "native":
				_, err = db.Exec("INSERT INTO orchestration_v2_projection_provider_turns VALUES('id','node',?,?,?)", start.Format(time.RFC3339Nano), `{"nativeTurnRef":{"nativeId":"finished"}}`, end.Format(time.RFC3339Nano))
			default:
				_, err = db.Exec("INSERT INTO orchestration_v2_projection_provider_turns VALUES('id','node',?,?)", start.Format(time.RFC3339Nano), payload)
			}
			if err != nil {
				t.Fatal(err)
			}
			count := 0
			if err := readGrokTurns(context.Background(), path, time.Now().Add(-7*24*time.Hour), func(id string, at time.Time) {
				count++
				if id != "finished" || !at.Equal(end) {
					t.Fatal(id, at)
				}
			}); err != nil || count != 1 {
				t.Fatal(err, count)
			}
		})
	}
}

func TestGrokPayloadDriverAndNativeThreadIdentity(t *testing.T) {
	for _, driver := range []string{`"driver":"grok",`, ``} {
		t.Run(driver, func(t *testing.T) {
			home := t.TempDir()
			dir := filepath.Join(home, ".t3", "userdata")
			if err := os.MkdirAll(dir, 0700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "statev2.sqlite")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			for _, stmt := range []string{"CREATE TABLE orchestration_v2_projection_provider_threads(provider_thread_id TEXT,driver TEXT,provider TEXT,payload_json TEXT)", "CREATE TABLE orchestration_v2_projection_provider_turns(provider_thread_id TEXT,ordinal INTEGER,payload_json TEXT)"} {
				if _, err := db.Exec(stmt); err != nil {
					t.Fatal(err)
				}
			}
			thread := `{` + driver + `"nativeThreadRef":{"driver":"grok","nativeId":"session"}}`
			if _, err := db.Exec("INSERT INTO orchestration_v2_projection_provider_threads VALUES('internal',NULL,NULL,?)", thread); err != nil {
				t.Fatal(err)
			}
			at := time.Now().UTC().Add(-time.Hour)
			payload := fmt.Sprintf(`{"completedAt":%q}`, at.Format(time.RFC3339Nano))
			if _, err := db.Exec("INSERT INTO orchestration_v2_projection_provider_turns VALUES('internal',1,?)", payload); err != nil {
				t.Fatal(err)
			}
			count := 0
			if err := readGrokTurns(context.Background(), path, time.Now().Add(-7*24*time.Hour), func(id string, _ time.Time) {
				count++
				if id != "session:turn:1" {
					t.Fatal(id)
				}
			}); err != nil || count != 1 {
				t.Fatal(err, count)
			}
			cli := filepath.Join(home, ".grok", "sessions", "workspace", "session")
			if err := os.MkdirAll(cli, 0700); err != nil {
				t.Fatal(err)
			}
			report := fmt.Sprintf(`{"sessionId":"session","turns":[{"turnNumber":1,"endedAt":%q}]}`, at.Format(time.RFC3339Nano))
			if err := os.WriteFile(filepath.Join(cli, "usage.json"), []byte(report), 0600); err != nil {
				t.Fatal(err)
			}
			card := (&Fetcher{Home: home}).grokActivity(context.Background())
			if card.Error != "" || card.Activity.SourcesAvailable != 2 || len(card.Activity.Events) != 1 {
				t.Fatal(card)
			}
		})
	}
}
