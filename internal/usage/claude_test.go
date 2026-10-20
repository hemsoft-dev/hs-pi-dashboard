package usage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClaudeWindows(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	body := `{"five_hour":{"utilization":40,"resets_at":"2026-10-19T12:00:00Z"},"seven_day":{"utilization":25,"resets_at":"2026-10-22T10:00:00Z"},"seven_day_sonnet":{"utilization":0},"seven_day_opus":{"utilization":true},"extra_usage":{"utilization":100,"used_credits":200},"limits":[false,{"kind":"weekly_scoped","group":"weekly","percent":55,"scope":{"model":{"id":"demo","display_name":"Demo model"}}},{"kind":"weekly_scoped","group":"weekly","percent":60,"scope":{"model":{"id":"demo","display_name":"Duplicate"}}},{"kind":"weekly_scoped","group":"weekly","percent":80,"scope":{"model":{"display_name":"All models"}}}]}`
	gauges, err := parseClaudeWindows([]byte(body), now)
	if err != nil || len(gauges) != 4 {
		t.Fatalf("gauges=%+v err=%v", gauges, err)
	}
	if gauges[0].Label != "5h" || *gauges[0].UsedPercent != 40 || gauges[0].ResetAt.Hour() != 12 || gauges[0].WindowSeconds != 18000 || gauges[0].ProjectedEnd == nil || *gauges[0].ProjectedEnd < 66.66 || *gauges[0].ProjectedEnd > 66.67 {
		t.Fatalf("5h=%+v", gauges[0])
	}
	if gauges[1].Label != "Weekly" || gauges[1].WindowSeconds != 604800 || gauges[1].ProjectedEnd == nil {
		t.Fatalf("weekly=%+v", gauges[1])
	}
	if *gauges[2].UsedPercent != 0 || gauges[2].ProjectedEnd != nil || !gauges[2].ResetAt.IsZero() {
		t.Fatalf("zero without reset=%+v", gauges[2])
	}
	if gauges[3].Label != "Demo model · weekly" || *gauges[3].UsedPercent != 55 {
		t.Fatalf("scoped=%+v", gauges[3])
	}
}

func TestClaudeRoutinesAliasesAndLimitFallbacks(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	labels := func(g []Gauge) string {
		var out []string
		for _, x := range g {
			out = append(out, fmt.Sprintf("%s=%v", x.Label, *x.UsedPercent))
		}
		return strings.Join(out, ",")
	}
	// The Cowork alias is used when the canonical Routines key is absent or unusable.
	g, err := parseClaudeWindows([]byte(`{"seven_day_routines":[],"seven_day_cowork":{"utilization":7}}`), now)
	if err != nil || labels(g) != "Routines · weekly=7" {
		t.Fatalf("cowork=%s err=%v", labels(g), err)
	}
	// Limits-only responses still report the session and weekly windows.
	g, err = parseClaudeWindows([]byte(`{"limits":[null,{"kind":"session","group":"session","percent":"11"},{"kind":"session","group":"session","percent":12,"resets_at":"2026-10-19T12:00:00Z","scope":null},{"kind":"weekly_all","group":"weekly","percent":9,"scope":null},{"kind":"weekly_scoped","group":"weekly","percent":5,"scope":{"model":{"display_name":"Fable"}}}]}`), now)
	if err != nil || labels(g) != "5h=12,Weekly=9,Fable · weekly=5" || g[0].WindowSeconds != 18000 || g[0].ResetAt.IsZero() {
		t.Fatalf("limits-only=%s err=%v", labels(g), err)
	}
	// Root windows win when both shapes are present, so nothing is reported twice.
	g, err = parseClaudeWindows([]byte(`{"five_hour":{"utilization":40},"seven_day":{"utilization":25},"limits":[{"kind":"session","group":"session","percent":11},{"kind":"weekly_all","group":"weekly","percent":9}]}`), now)
	if err != nil || labels(g) != "5h=40,Weekly=25" {
		t.Fatalf("both shapes=%s err=%v", labels(g), err)
	}
}

func TestClaudePartialAndInvalidWindows(t *testing.T) {
	now := time.Now()
	for _, body := range []string{`null`, `false`, `[]`, `{`, `{}`, `{"extra_usage":{"utilization":0}}`, `{"five_hour":{"utilization":"50"}}`, `{"five_hour":{"utilization":true}}`, `{"five_hour":{"utilization":null}}`, `{"five_hour":{"utilization":-1}}`, `{"five_hour":{"utilization":101,"resets_at":"invalid"}}`} {
		if g, e := parseClaudeWindows([]byte(body), now); e == nil || len(g) != 0 {
			t.Errorf("accepted %s: %+v %v", body, g, e)
		}
	}
	g, e := parseClaudeWindows([]byte(`{"five_hour":[],"seven_day":{"utilization":12,"resets_at":false},"seven_day_sonnet":{"utilization":null,"resets_at":"2099-01-01T00:00:00Z"}}`), now)
	if e != nil || len(g) != 2 || g[0].Label != "Weekly" || *g[0].UsedPercent != 12 || g[0].ProjectedEnd != nil || g[1].UsedPercent != nil {
		t.Fatalf("partial=%+v %v", g, e)
	}
	for _, reset := range []time.Time{now.Add(time.Hour * 6), now.Add(-time.Hour), now.Add(time.Hour*5 - time.Second*60)} {
		g, e := parseClaudeWindows(fmt.Appendf(nil, `{"five_hour":{"utilization":50,"resets_at":%q}}`, reset.Format(time.RFC3339)), now)
		if e != nil || g[0].ProjectedEnd != nil {
			t.Fatalf("untrusted timing forecast=%+v %v", g, e)
		}
	}
}

type claudeTransport func(*http.Request) (*http.Response, error)

func (f claudeTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func claudeHome(t *testing.T) string {
	t.Helper()
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	return t.TempDir()
}
func writeClaudeJSON(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeCredentialsReadOnlyPrecedence(t *testing.T) {
	home := claudeHome(t)
	f := &Fetcher{Home: home}
	pi := filepath.Join(home, ".pi", "agent", "auth.json")
	own := filepath.Join(home, ".claude", ".credentials.json")
	writeClaudeJSON(t, pi, `{"anthropic":{"access":"pi-session"}}`)
	if got := f.claudeToken(); got != "pi-session" {
		t.Fatal(got)
	}
	saved := `{"claudeAiOauth":{"accessToken":"native-session","refreshToken":"do-not-use"}}`
	writeClaudeJSON(t, own, saved)
	if got := f.claudeToken(); got != "native-session" {
		t.Fatal(got)
	}
	profile := filepath.Join(home, "other-profile")
	t.Setenv("CLAUDE_CONFIG_DIR", profile)
	if got := f.claudeToken(); got != "" {
		t.Fatalf("cross-profile fallback %q", got)
	}
	writeClaudeJSON(t, filepath.Join(profile, ".credentials.json"), `{"claudeAiOauth":{"accessToken":"profile-session"}}`)
	if got := f.claudeToken(); got != "profile-session" {
		t.Fatal(got)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "explicit-session")
	if got := f.claudeToken(); got != "explicit-session" {
		t.Fatal(got)
	}
	after, err := os.ReadFile(own)
	if err != nil || string(after) != saved {
		t.Fatal("credential bytes changed", err)
	}
}

func TestClaudeFetchErrorsAndHeaders(t *testing.T) {
	home := claudeHome(t)
	calls := 0
	f := &Fetcher{Home: home, Client: &http.Client{Transport: claudeTransport(func(r *http.Request) (*http.Response, error) { calls++; return nil, errors.New("unexpected call") })}}
	if c := f.claude(context.Background()); c.Error == "" || c.Gauges != nil || calls != 0 {
		t.Fatalf("missing auth=%+v calls=%d", c, calls)
	}
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "synthetic-secret")
	for _, status := range []int{200, 401, 403, 429, 500, 0} {
		calls = 0
		f.Client.Transport = claudeTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			if r.Method != "GET" || r.URL.String() != claudeUsageURL || r.Header.Get("Authorization") != "Bearer synthetic-secret" || r.Header.Get("anthropic-beta") != "oauth-2025-04-20" || r.Header.Get("User-Agent") != claudeUserAgent || r.Header.Get("Content-Type") != "application/json" {
				t.Fatalf("bad request method/url/headers")
			}
			if status == 0 {
				return nil, errors.New("network failed: synthetic-secret")
			}
			body := `{"seven_day":{"utilization":50}}`
			if status != 200 {
				body = "synthetic-secret"
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})
		c := f.claude(context.Background())
		if calls != 1 || c.Key != "claude" || c.Kind != KindQuota || c.FetchedAt.IsZero() || strings.Contains(c.Error, "synthetic-secret") {
			t.Fatalf("card=%+v calls=%d", c, calls)
		}
		if status == 200 {
			if c.Error != "" || len(c.Gauges) != 1 || *c.Gauges[0].UsedPercent != 50 {
				t.Fatalf("success=%+v", c)
			}
		} else if c.Error == "" || len(c.Gauges) != 0 {
			t.Fatalf("failure=%+v", c)
		}
	}
	f.Client.Transport = claudeTransport(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c := f.claude(ctx); c.Error == "" {
		t.Fatal("canceled request succeeded")
	}
}

func TestClaudeCacheKeepsReadingAgeAcrossFailuresAndRecovers(t *testing.T) {
	home := claudeHome(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "synthetic-session")
	mode := 0
	f := &Fetcher{Home: home, Client: &http.Client{Transport: claudeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != claudeUsageURL {
			return nil, errors.New("unused provider")
		}
		status := 200
		body := `{"seven_day":{"utilization":35}}`
		if mode == 1 {
			status = 503
			body = "temporary failure"
		}
		if mode == 2 {
			body = `{"seven_day":{"utilization":40}}`
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}}
	store := NewStore(f, "demo", time.Minute)
	find := func() Card {
		t.Helper()
		for _, c := range store.Snapshot().Cards {
			if c.Key == "claude" {
				return c
			}
		}
		t.Fatal("Claude missing from FetchAll")
		return Card{}
	}
	store.refresh(context.Background())
	initial := find()
	if initial.Error != "" || len(initial.Gauges) != 1 || *initial.Gauges[0].UsedPercent != 35 {
		t.Fatalf("initial=%+v", initial)
	}
	mode = 1
	for range 2 {
		store.refresh(context.Background())
		cached := find()
		if cached.Error == "" || !cached.FetchedAt.Equal(initial.FetchedAt) || *cached.Gauges[0].UsedPercent != 35 {
			t.Fatalf("cache=%+v", cached)
		}
	}
	mode = 2
	store.refresh(context.Background())
	recovered := find()
	if recovered.Error != "" || *recovered.Gauges[0].UsedPercent != 40 || !recovered.FetchedAt.After(initial.FetchedAt) {
		t.Fatalf("recovery=%+v", recovered)
	}
}

func TestClaudeMalformedResponseDoesNotLeakBody(t *testing.T) {
	home := claudeHome(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "synthetic-session")
	f := &Fetcher{Home: home, Client: &http.Client{Transport: claudeTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("synthetic-secret")), Header: make(http.Header)}, nil
	})}}
	c := f.claude(context.Background())
	if c.Error != "invalid Claude usage response" || len(c.Gauges) != 0 {
		t.Fatalf("card=%+v", c)
	}
}
