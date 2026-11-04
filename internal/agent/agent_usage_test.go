package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/usage"
)

type roundTrip func(*http.Request) (*http.Response, error)

func (f roundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// The whole read path a pmd pull sees: Home's store over a fake provider, Home's
// GET /usage, then Mini's agent wired only with the peer setting and no session.
func TestAgentUsagePeerCarriesFreshClaudeToAnotherAgent(t *testing.T) {
	isolate := func() string {
		home := t.TempDir()
		t.Setenv("HOME", home)
		t.Setenv("USERPROFILE", home)
		return home
	}
	for _, name := range []string{"CLAUDE_CODE_OAUTH_TOKEN", "CLAUDE_CONFIG_DIR", "MOONSHOT_API_KEY", "KIMI_K3_API_KEY", "GITHUB_BILLING_ORG",
		"OPENCODE_GO_WORKSPACE_ID", "OPENCODE_ZEN_WORKSPACE_ID", "OPENCODE_GO_AUTH_COOKIE", "OPENCODE_ZEN_AUTH_COOKIE", "ANTIGRAVITY_CREDENTIALS_FILE"} {
		t.Setenv(name, "")
	}
	t.Setenv("PATH", t.TempDir())
	t.Setenv("HS_PI_DASHBOARD_AGY_BIN", filepath.Join(t.TempDir(), "absent"))
	t.Setenv("HS_PI_DASHBOARD_MUSE_BIN", filepath.Join(t.TempDir(), "absent"))

	homeDir := isolate()
	if err := os.MkdirAll(filepath.Join(homeDir, ".claude"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(homeDir, ".claude", ".credentials.json"), []byte(`{"claudeAiOauth":{"accessToken":"synthetic-home-session"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	provider := func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "api.anthropic.com" {
			return nil, errors.New("unused provider")
		}
		if r.Header.Get("Authorization") != "Bearer synthetic-home-session" {
			t.Errorf("Home provider request used the wrong session")
		}
		body := `{"limits":[{"kind":"session","percent":5,"resets_at":"` + reset + `"},{"kind":"weekly_all","percent":14,"resets_at":"` + reset + `"}]}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	homeStore := usage.NewStore(&usage.Fetcher{Home: homeDir, Client: &http.Client{Transport: roundTrip(provider)}}, "home", time.Minute)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	homeStore.Start(ctx)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /usage", usageHandler(homeStore))
	homeAgent := httptest.NewServer(mux)
	defer homeAgent.Close()

	peer, err := usage.ParseClaudePeer("home=" + homeAgent.URL)
	if err != nil {
		t.Fatal(err)
	}
	isolate() // Mini's agent runs with an empty home, as Mini's lost login does.
	miniStore := newUsageStore(Options{Machine: "mini", UsagePoll: time.Minute, ClaudeUsagePeer: &peer})
	miniStore.Start(ctx)
	miniAgent := httptest.NewServer(usageHandler(miniStore))
	defer miniAgent.Close()

	response, err := http.Get(miniAgent.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "synthetic") {
		t.Fatalf("credential reached Mini's output: %s", raw)
	}
	// pmd reads {"cards":[...]} and takes each card's gauges by key.
	var doc struct {
		Machine string `json:"machine"`
		Cards   []struct {
			Key       string    `json:"key"`
			Error     string    `json:"error"`
			FetchedAt time.Time `json:"fetchedAt"`
			Gauges    []struct {
				Label         string    `json:"label"`
				UsedPercent   *float64  `json:"usedPercent"`
				ResetAt       time.Time `json:"resetAt"`
				WindowSeconds float64   `json:"windowSeconds"`
			} `json:"gauges"`
			Claude *struct {
				SourceMachine string `json:"sourceMachine"`
				Status        string `json:"status"`
				PeerStatus    string `json:"peerStatus"`
			} `json:"claude"`
		} `json:"cards"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, card := range doc.Cards {
		if card.Key != "claude" {
			if card.Claude != nil {
				t.Fatalf("provenance on %s", card.Key)
			}
			continue
		}
		found = true
		if card.Error != "" || card.Claude == nil || card.Claude.SourceMachine != "home" || card.Claude.Status != "ok" || card.Claude.PeerStatus != "ok" || time.Since(card.FetchedAt) > time.Minute {
			t.Fatalf("Claude card=%s", raw)
		}
		if len(card.Gauges) != 2 || card.Gauges[0].Label != "5h" || *card.Gauges[0].UsedPercent != 5 || card.Gauges[0].ResetAt.IsZero() || card.Gauges[0].WindowSeconds != 18000 || card.Gauges[1].Label != "Weekly" || *card.Gauges[1].UsedPercent != 14 {
			t.Fatalf("gauges=%+v", card.Gauges)
		}
	}
	if doc.Machine != "mini" || !found {
		t.Fatalf("doc=%s", raw)
	}
}
