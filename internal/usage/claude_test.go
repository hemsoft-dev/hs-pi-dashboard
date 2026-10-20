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
	if gauges[3].Label != "Demo model weekly" || *gauges[3].UsedPercent != 55 {
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
	if err != nil || labels(g) != "Routines weekly=7" {
		t.Fatalf("cowork=%s err=%v", labels(g), err)
	}
	// Limits-only responses still report the session and weekly windows.
	g, err = parseClaudeWindows([]byte(`{"limits":[null,{"kind":"session","group":"session","percent":"11"},{"kind":"session","group":"session","percent":12,"resets_at":"2026-10-19T12:00:00Z","scope":null},{"kind":"weekly_all","group":"weekly","percent":9,"scope":null},{"kind":"weekly_scoped","group":"weekly","percent":5,"scope":{"model":{"display_name":"Fable"}}}]}`), now)
	if err != nil || labels(g) != "5h=12,Weekly=9,Fable weekly=5" || g[0].WindowSeconds != 18000 || g[0].ResetAt.IsZero() {
		t.Fatalf("limits-only=%s err=%v", labels(g), err)
	}
	// Structured entries win when both shapes are present, so nothing is
	// reported twice; root windows remain the fallback for unusable entries.
	g, err = parseClaudeWindows([]byte(`{"five_hour":{"utilization":40},"seven_day":{"utilization":25},"limits":[{"kind":"session","group":"session","percent":11},{"kind":"weekly_all","group":"weekly","percent":9}]}`), now)
	if err != nil || labels(g) != "5h=11,Weekly=9" {
		t.Fatalf("both shapes=%s err=%v", labels(g), err)
	}
	g, err = parseClaudeWindows([]byte(`{"five_hour":{"utilization":40},"seven_day":{"utilization":25},"limits":[{"kind":"session","percent":null},{"kind":"weekly_all","percent":"9"}]}`), now)
	if err != nil || labels(g) != "5h=40,Weekly=25" {
		t.Fatalf("unusable structured=%s err=%v", labels(g), err)
	}
	// A structured entry with only a reset never hides a measured root reading.
	g, err = parseClaudeWindows([]byte(`{"five_hour":{"utilization":40},"limits":[{"kind":"session","percent":null,"resets_at":"2026-10-19T12:00:00Z"}]}`), now)
	if err != nil || labels(g) != "5h=40" {
		t.Fatalf("reset-only structured=%s err=%v", labels(g), err)
	}
	g, err = parseClaudeWindows([]byte(`{"limits":[{"kind":"session","resets_at":"2026-10-19T12:00:00Z"}]}`), now)
	if err != nil || len(g) != 1 || g[0].UsedPercent != nil || g[0].ResetAt.IsZero() {
		t.Fatalf("reset-only alone=%+v err=%v", g, err)
	}
	// A measured duplicate scoped entry replaces an earlier reset-only one,
	// and still supersedes the legacy model window.
	g, err = parseClaudeWindows([]byte(`{"seven_day_sonnet":{"utilization":30},"limits":[{"kind":"weekly_scoped","percent":null,"resets_at":"2026-10-22T10:00:00Z","scope":{"model":{"display_name":"Sonnet"}}},{"kind":"weekly_scoped","percent":31,"scope":{"model":{"display_name":"Sonnet"}}},{"kind":"weekly_scoped","percent":40,"scope":{"model":{"display_name":"Sonnet"}}}]}`), now)
	if err != nil || labels(g) != "Sonnet weekly=31" {
		t.Fatalf("duplicate scoped=%s err=%v", labels(g), err)
	}
	// Scoped limits count whatever their display group.
	g, err = parseClaudeWindows([]byte(`{"limits":[{"kind":"weekly_scoped","group":"model","percent":17.2,"scope":{"model":{"display_name":"Fable"}}}]}`), now)
	if err != nil || labels(g) != "Fable weekly=17.2" {
		t.Fatalf("model group=%s err=%v", labels(g), err)
	}
}

func TestClaudePartialAndInvalidWindows(t *testing.T) {
	now := time.Now()
	for _, body := range []string{`null`, `false`, `[]`, `{`, `{}`, `{"extra_usage":{"utilization":0}}`, `{"five_hour":{"utilization":"50"}}`, `{"five_hour":{"utilization":true}}`, `{"five_hour":{"utilization":null}}`, `{"five_hour":{"utilization":-1}}`} {
		if g, e := parseClaudeWindows([]byte(body), now); e == nil || len(g) != 0 {
			t.Errorf("accepted %s: %+v %v", body, g, e)
		}
	}
	g, e := parseClaudeWindows([]byte(`{"five_hour":[],"seven_day":{"utilization":12,"resets_at":false},"seven_day_sonnet":{"utilization":null,"resets_at":"2099-01-01T00:00:00Z"}}`), now)
	if e != nil || len(g) != 2 || g[0].Label != "Weekly" || *g[0].UsedPercent != 12 || g[0].ProjectedEnd != nil || g[1].UsedPercent != nil {
		t.Fatalf("partial=%+v %v", g, e)
	}
	// Over-limit readings display as 100% and still forecast from the raw value.
	g, e = parseClaudeWindows([]byte(`{"five_hour":{"utilization":101,"resets_at":"invalid"}}`), now)
	if e != nil || len(g) != 1 || *g[0].UsedPercent != 100 || g[0].ProjectedEnd != nil {
		t.Fatalf("over limit without reset=%+v %v", g, e)
	}
	g, e = parseClaudeWindows(fmt.Appendf(nil, `{"seven_day":{"utilization":120,"resets_at":%q}}`, now.Add(3*24*time.Hour).Format(time.RFC3339)), now)
	if e != nil || len(g) != 1 || *g[0].UsedPercent != 100 || g[0].ProjectedEnd == nil || *g[0].ProjectedEnd <= 120 {
		t.Fatalf("over limit with reset=%+v %v", g, e)
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
		f.claudeGate.clear("synthetic-secret")
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

func TestClaudeRateLimitCooldown(t *testing.T) {
	home := claudeHome(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "synthetic-secret")
	calls, status, retryAfter := 0, 429, "120"
	f := &Fetcher{Home: home, Client: &http.Client{Transport: claudeTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		h := make(http.Header)
		h.Set("Retry-After", retryAfter)
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"seven_day":{"utilization":50}}`)), Header: h}, nil
	})}}
	if c := f.claude(context.Background()); calls != 1 || c.Error != "Claude usage rate limited; next attempt in 2m" {
		t.Fatalf("429=%+v calls=%d", c, calls)
	}
	// Later refreshes during the cooldown send nothing.
	status = 200
	if c := f.claude(context.Background()); calls != 1 || !strings.Contains(c.Error, "rate limited") || len(c.Gauges) != 0 {
		t.Fatalf("cooldown=%+v calls=%d", c, calls)
	}
	// Once the cooldown expires the next refresh succeeds and clears the gate.
	for key := range f.claudeGate.until {
		f.claudeGate.until[key] = time.Now().Add(-time.Second)
	}
	if c := f.claude(context.Background()); calls != 2 || c.Error != "" || len(c.Gauges) != 1 || len(f.claudeGate.until) != 0 {
		t.Fatalf("recovered=%+v calls=%d", c, calls)
	}
	// Retry-After as an HTTP date, missing, or excessive.
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	var g claudeRateGate
	for _, tc := range []struct {
		header string
		want   time.Duration
	}{
		{now.Add(10 * time.Minute).Format(http.TimeFormat), 10 * time.Minute},
		{"0", 0},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0},
		{"", claudeDefaultCooldown},
		{"soon", claudeDefaultCooldown},
		{"86400", claudeMaxCooldown},
		{"9223372037", claudeMaxCooldown},
		{"99999999999999999999", claudeDefaultCooldown},
	} {
		g.clear("t")
		if until := g.block("t", tc.header, now); until.Sub(now) != tc.want {
			t.Errorf("Retry-After %q blocked for %v, want %v", tc.header, until.Sub(now), tc.want)
		}
	}
	// A zero delay leaves the session free to retry on the next refresh.
	g.clear("t")
	if until := g.block("t", "0", now); !g.blockedUntil("t", now).IsZero() || claudeRateLimited(until, now) != "Claude usage rate limited; retrying on the next refresh" {
		t.Error("Retry-After: 0 still blocked the session")
	}
	// A shorter later Retry-After never shortens an existing cooldown.
	g.clear("t")
	g.block("t", "600", now)
	if until := g.block("t", "60", now); until.Sub(now) != 10*time.Minute {
		t.Errorf("cooldown shortened to %v", until.Sub(now))
	}
}

func TestClaudeScopedLimitsReplaceLegacyModelWindows(t *testing.T) {
	now := time.Date(2026, 10, 19, 10, 0, 0, 0, time.UTC)
	body := `{"seven_day":{"utilization":25},"seven_day_sonnet":{"utilization":30},"seven_day_opus":{"utilization":10},"limits":[{"kind":"weekly_scoped","group":"weekly","percent":31,"scope":{"model":{"id":"claude-sonnet-4-5","display_name":"Sonnet"}}},{"kind":"weekly_scoped","group":"weekly","percent":5,"scope":{"model":{"display_name":"Fable"}}}]}`
	g, err := parseClaudeWindows([]byte(body), now)
	var got []string
	for _, x := range g {
		got = append(got, fmt.Sprintf("%s=%v", x.Label, *x.UsedPercent))
	}
	if err != nil || strings.Join(got, ",") != "Weekly=25,Opus weekly=10,Sonnet weekly=31,Fable weekly=5" {
		t.Fatalf("gauges=%v err=%v", got, err)
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
