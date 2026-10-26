package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// All payloads and credentials here are synthetic. No provider calls.
func TestCodexPlanWindowPolicy(t *testing.T) {
	weekly := `{"used_percent":9,"reset_at":` + fmt.Sprint(time.Now().Add(6*24*time.Hour).Unix()) + `,"limit_window_seconds":604800}`
	short := `{"used_percent":0,"limit_window_seconds":18000}`
	monthly := `{"used_percent":0,"limit_window_seconds":2592000}`
	for _, tt := range []struct {
		name, plan, primary, secondary string
		wantPlan                       string
		count                          int
		shapes                         []string
	}{
		{"paid weekly", `"prolite"`, weekly, "null", "prolite", 1, []string{"weekly"}},
		{"paid both", `"pro"`, short, weekly, "pro", 2, []string{"5h", "weekly"}},
		{"paid short only", `"plus"`, short, "null", "plus", 1, []string{"5h"}},
		{"missing plan", "null", weekly, "null", "unknown", 0, []string{"weekly"}},
		{"object plan", `{}`, weekly, "null", "unknown", 0, []string{"weekly"}},
		{"malicious plan", `"<img onerror=x>@secret.invalid"`, weekly, "null", "unknown", 0, []string{"weekly"}},
		{"unknown plan", `"future-paid-monthly"`, monthly, "null", "unknown", 0, []string{"30d"}},
		{"free monthly", `"free"`, monthly, "null", "free", 0, []string{"30d"}},
		{"free weekly", `"free"`, weekly, "null", "free", 0, []string{"weekly"}},
		{"paid monthly", `"pro"`, monthly, "null", "pro", 0, []string{"30d"}},
		{"mixed unsupported", `"pro"`, weekly, monthly, "pro", 0, []string{"weekly", "30d"}},
		{"almost weekly", `"pro"`, `{"used_percent":0,"limit_window_seconds":604799}`, "null", "pro", 0, []string{"unsupported"}},
		{"no window", `"pro"`, "null", "null", "pro", 0, nil},
		{"malformed window", `"pro"`, `"bad"`, "null", "pro", 0, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"plan_type":%s,"rate_limit":{"primary_window":%s,"secondary_window":%s}}`, tt.plan, tt.primary, tt.secondary)
			gauges, plan, shapes, err := parseCodexWindows([]byte(body))
			if plan != tt.wantPlan || len(gauges) != tt.count || !reflect.DeepEqual(shapes, tt.shapes) || (err == nil) != (tt.count > 0) {
				t.Fatalf("gauges=%+v plan=%s shapes=%v err=%v", gauges, plan, shapes, err)
			}
		})
	}
	for _, body := range []string{`{`, `null`, `{"plan_type":"pro","rate_limit":[]}`, `{"plan_type":"pro"} trailing`} {
		if gauges, _, _, err := parseCodexWindows([]byte(body)); err == nil || len(gauges) != 0 {
			t.Fatalf("malformed response accepted: %s", body)
		}
	}
}

func TestCodexMalformedMetricsStayAbsent(t *testing.T) {
	for _, used := range []string{`null`, `"NaN"`, `"+Inf"`, `-1`, `101`, `{}`, `"secret"`} {
		for _, reset := range []string{`null`, `"NaN"`, `"+Inf"`, `-1`, `1e300`, `1.5`} {
			body := fmt.Sprintf(`{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":%s,"reset_at":%s,"limit_window_seconds":604800}}}`, used, reset)
			gauges, _, _, err := parseCodexWindows([]byte(body))
			if err != nil || len(gauges) != 1 || gauges[0].UsedPercent != nil || gauges[0].ProjectedEnd != nil || !gauges[0].ResetAt.IsZero() {
				t.Fatalf("malformed values became usage: %+v %v", gauges, err)
			}
		}
	}
}

func writeCodexAuth(t *testing.T, home, source, token, account string) {
	t.Helper()
	path := filepath.Join(home, ".pi", "agent", "auth.json")
	value := map[string]any{"openai-codex": map[string]string{"access": token, "accountId": account}}
	if source == "cli" {
		path = filepath.Join(home, ".codex", "auth.json")
		value = map[string]any{"tokens": map[string]string{"access_token": token, "account_id": account}}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(value)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestCodexCredentialPrecedenceAndSafeProvenance(t *testing.T) {
	for _, source := range []string{"pi", "cli", "jwt", "empty-pi", "malformed-pi", "none"} {
		t.Run(source, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("CODEX_HOME", t.TempDir()) // this collector follows the user-home files
			token := "synthetic-token"
			account := "synthetic-private-account"
			wantSource := source
			if source != "none" {
				writeCodexAuth(t, home, "cli", "cli-token", "cli-private-account")
			}
			switch source {
			case "pi":
				writeCodexAuth(t, home, "pi", token, account)
			case "jwt":
				token = base64URLEncode(`{}`) + "." + base64URLEncode(`{"email":"synthetic@private.invalid","iat":1,"https://api.openai.com/auth":{"chatgpt_account_id":"synthetic-private-account","chatgpt_plan_type":"pro"}}`) + ".sig"
				writeCodexAuth(t, home, "pi", token, "")
				wantSource = "pi"
			case "malformed-pi":
				writeCodexAuth(t, home, "pi", "", "pi-private-account")
				if err := os.WriteFile(filepath.Join(home, ".pi", "agent", "auth.json"), []byte(`{"openai-codex":{"access":"do-not-use","accountId":17}}`), 0600); err != nil {
					t.Fatal(err)
				}
				fallthrough
			case "empty-pi":
				if source == "empty-pi" {
					writeCodexAuth(t, home, "pi", " ", "pi-private-account")
				}
				fallthrough
			case "cli":
				token, account, wantSource = "cli-token", "cli-private-account", "cli"
			}
			called := false
			f := &Fetcher{Home: home, Client: &http.Client{Transport: usageRoundTripper(func(req *http.Request) (*http.Response, error) {
				called = true
				if req.Header.Get("Authorization") != "Bearer "+token || req.Header.Get("ChatGPT-Account-Id") != account || req.URL.String() != codexUsageURL {
					t.Fatal("wrong credential selection")
				}
				// JWT says pro, response says free. Only the response counts.
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"plan_type":"free","email":"synthetic@private.invalid","account_id":"synthetic-private-account","rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":2592000}}}`))}, nil
			})}}
			card := f.codex(context.Background())
			if card.Codex.CredentialSource != wantSource || card.Error == "" || len(card.Gauges) != 0 || called != (source != "none") {
				t.Fatalf("unexpected card %+v", card)
			}
			if called && (card.Plan != "free" || card.Codex.PlanSource != "usage-response") {
				t.Fatal("JWT claim replaced authenticated provider plan")
			}
			wire, err := json.Marshal(card)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{token, account, "private.invalid", "accountId", "codexIdentity"} {
				if strings.Contains(string(wire), secret) {
					t.Fatal("private credential data entered card JSON")
				}
			}
		})
	}
}

func TestCodexCacheIdentityBoundary(t *testing.T) {
	home := t.TempDir()
	status, body := 200, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":9,"limit_window_seconds":604800}}}`
	f := &Fetcher{Home: home, Client: &http.Client{Transport: usageRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
	})}}
	writeCodexAuth(t, home, "pi", "old-token", "account-a")
	initial := f.codex(context.Background())
	s := NewStore(f, "synthetic", time.Minute)
	s.snapshot.Cards = []Card{initial}
	status = 401
	writeCodexAuth(t, home, "pi", "refreshed-token", "account-a")
	retained := s.retain([]Card{f.codex(context.Background())})[0]
	if len(retained.Gauges) != 1 || retained.Plan != "pro" || retained.Codex.PlanSource != "usage-response" || !retained.FetchedAt.Equal(initial.FetchedAt) {
		t.Fatal("same-account failure lost cached provenance")
	}
	for _, tt := range []struct{ source, token, account string }{
		{"pi", "new-token", "account-b"},
		{"cli", "cli-token", "account-a"},
		{"pi", "", ""},
	} {
		writeCodexAuth(t, home, "pi", "", "")
		writeCodexAuth(t, home, "cli", "", "")
		writeCodexAuth(t, home, tt.source, tt.token, tt.account)
		got := s.retain([]Card{f.codex(context.Background())})[0]
		if len(got.Gauges) != 0 || got.Plan != "" || got.FetchedAt.Equal(initial.FetchedAt) {
			t.Fatal("old account reading crossed credential boundary")
		}
	}
	writeCodexAuth(t, home, "pi", "old-token", "account-a")
	status, body = 200, `{"plan_type":"free","rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":2592000}}}`
	got := s.retain([]Card{f.codex(context.Background())})[0]
	if got.Plan != "free" || len(got.Gauges) != 0 {
		t.Fatal("successful unsupported response retained old paid quota")
	}
	// With no account ID, any token change conservatively invalidates the cache.
	status, body = 200, `{"plan_type":"pro","rate_limit":{"primary_window":{"used_percent":9,"limit_window_seconds":604800}}}`
	writeCodexAuth(t, home, "pi", "opaque-a", "")
	s.snapshot.Cards = []Card{f.codex(context.Background())}
	status = 401
	writeCodexAuth(t, home, "pi", "opaque-b", "")
	if len(s.retain([]Card{f.codex(context.Background())})[0].Gauges) != 0 {
		t.Fatal("unidentified token switch retained quota")
	}
	// JWT issue time and cached plan claims are irrelevant to account identity.
	jwt := func(iat int) string {
		return base64URLEncode(`{}`) + "." + base64URLEncode(fmt.Sprintf(`{"iat":%d,"https://api.openai.com/auth":{"chatgpt_account_id":"account-a","chatgpt_plan_type":"free"}}`, iat)) + ".sig"
	}
	status = 200
	writeCodexAuth(t, home, "pi", jwt(1), "")
	s.snapshot.Cards = []Card{f.codex(context.Background())}
	status = 401
	writeCodexAuth(t, home, "pi", jwt(999999), "")
	if got := s.retain([]Card{f.codex(context.Background())})[0]; len(got.Gauges) != 1 || got.Plan != "pro" {
		t.Fatal("JWT age/claim difference invalidated same-account reading")
	}
}

func TestCodexTransportErrorsDoNotLeakSecrets(t *testing.T) {
	home := t.TempDir()
	writeCodexAuth(t, home, "pi", "synthetic-secret", "private-account")
	f := &Fetcher{Home: home, Client: &http.Client{Transport: usageRoundTripper(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("synthetic-secret private-account private@invalid.test")
	})}}
	card := f.codex(context.Background())
	wire, _ := json.Marshal(card)
	if card.Error != "Codex request failed" || strings.Contains(string(wire), "private") || strings.Contains(string(wire), "synthetic-secret") {
		t.Fatal("unsafe transport error")
	}
}
