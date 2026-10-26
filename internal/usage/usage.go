// Package usage fetches provider plan/balance data for the dashboard cards:
// Codex, Claude, OpenCode Go and Muse Code plan windows, Antigravity quota pools, and OpenCode
// Zen + Moonshot prepaid balances.
//
// The fetchers port the proven logic from ~/.pi/agent/extensions/statusline.ts
// (which in turn ports CodexBar iOS). Credentials are read from the same
// places the statusline reads them: pi's auth.json, ~/.codex/auth.json,
// ~/.pi/agent/opencode-go.json, and environment variables.
package usage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Card kinds.
const (
	KindQuota   = "quota"   // one or more Gauge windows
	KindBalance = "balance" // prepaid dollars remaining
	KindUnknown = "unknown" // no data source exists yet
)

// Gauge is one usage window: UsedPercent is 0-100 used, or nil when absent. The dashboard fills
// the gauge with UsedPercent on the statusline's green→red scale and, when
// the window timing allows it, shows ProjectedEnd — where usage lands at the
// reset if the current pace holds (can exceed 100).
type Gauge struct {
	BucketID      string    `json:"bucketId,omitempty"` // canonical Antigravity pool identity
	Label         string    `json:"label"`
	UsedPercent   *float64  `json:"usedPercent"`
	ResetAt       time.Time `json:"resetAt,omitzero"`
	WindowSeconds float64   `json:"windowSeconds,omitempty"`
	ProjectedEnd  *float64  `json:"projectedEnd,omitempty"`
}

// Card is one provider's dashboard card.
type Card struct {
	Codex                *CodexProvenance `json:"codex,omitempty"`
	codexIdentity        [32]byte         // local cache boundary; never serialized
	SubscriptionPriceUSD *float64         `json:"subscriptionPriceUsd"`
	Activity             *Activity        `json:"activity,omitempty"`
	FreshForSeconds      float64          `json:"freshForSeconds,omitempty"`
	Billing              *Billing         `json:"billing,omitempty"`
	Key                  string           `json:"key"`
	Label                string           `json:"label"`
	Kind                 string           `json:"kind"`
	BalanceUSD           *float64         `json:"balanceUsd,omitempty"`
	Gauges               []Gauge          `json:"gauges,omitempty"`
	Plan                 string           `json:"plan,omitempty"`            // provider-reported tier, allowlisted text
	AllowanceSource      string           `json:"allowanceSource,omitempty"` // provider-reported source token
	ObservedAt           time.Time        `json:"observedAt,omitzero"`       // when the provider observed the reading
	Warning              string           `json:"warning,omitempty"`
	Error                string           `json:"error,omitempty"`
	FetchedAt            time.Time        `json:"fetchedAt"`
}

// Snapshot is what the agent /usage endpoint returns.
type Snapshot struct {
	Machine     string    `json:"machine"`
	GeneratedAt time.Time `json:"generatedAt"`
	Cards       []Card    `json:"cards"`
}

const (
	firstRefreshWait = 25 * time.Second // how long /usage waits for the first poll
	fetchTimeout     = 20 * time.Second
	codexUsageURL    = "https://chatgpt.com/backend-api/wham/usage"
	agQuotaURL       = "https://daily-cloudcode-pa.googleapis.com/v1internal:retrieveUserQuotaSummary"
	agTokenURL       = "https://oauth2.googleapis.com/token"
	moonshotBalance  = "https://api.moonshot.ai/v1/users/me/balance"
	opencodeBase     = "https://opencode.ai"
)

// Fetcher reads credentials from disk and fetches every provider card.
type Fetcher struct {
	Client *http.Client
	Home   string // defaults to the current user's home

	githubCacheMu   sync.Mutex
	githubCache     map[string]billingCacheEntry
	agyRun          func(context.Context) ([]byte, error)
	githubRun       billingRunner
	githubRateMu    sync.Mutex
	githubRateUntil map[string]time.Time
	claudeGate      claudeRateGate
	muse            museCache
	museRun         func(context.Context) ([]byte, error)
}

// New returns a fetcher with defaults.
func New() *Fetcher {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return &Fetcher{Client: &http.Client{Timeout: fetchTimeout}, Home: home}
}

// Store caches a usage snapshot and refreshes it on a timer so the agent's
// /usage endpoint answers instantly without hammering provider APIs.
type Store struct {
	fetcher   *Fetcher
	machine   string
	pollEvery time.Duration

	mu        sync.Mutex
	snapshot  Snapshot
	readyOnce sync.Once
	ready     chan struct{}
}

// NewStore builds a store that refreshes every pollEvery interval.
func NewStore(fetcher *Fetcher, machine string, pollEvery time.Duration) *Store {
	if pollEvery <= 0 {
		pollEvery = time.Minute
	}
	return &Store{fetcher: fetcher, machine: machine, pollEvery: pollEvery, ready: make(chan struct{})}
}

// Start runs the refresh loop until ctx is cancelled. The first fetch happens
// immediately; Snapshot() callers wait for it (bounded) instead of seeing an
// empty snapshot.
func (s *Store) Start(ctx context.Context) {
	s.refresh(ctx)
	go func() {
		ticker := time.NewTicker(s.pollEvery)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.refresh(ctx)
			}
		}
	}()
}

func (s *Store) refresh(ctx context.Context) {
	cards := s.fetcher.FetchAll(ctx)
	s.mu.Lock()
	s.snapshot = Snapshot{Machine: s.machine, GeneratedAt: time.Now(), Cards: s.retain(cards)}
	s.mu.Unlock()
	s.readyOnce.Do(func() { close(s.ready) })
}

// retain keeps each failed card's last reading or authenticated Codex
// diagnostic from the current snapshot. Callers hold s.mu.
func (s *Store) retain(cards []Card) []Card {
	previous := make(map[string]Card, len(s.snapshot.Cards))
	for _, card := range s.snapshot.Cards {
		previous[card.Key] = card
	}
	for i, card := range cards {
		old, ok := previous[card.Key]
		if card.Key == "codex" {
			if !ok || old.Kind != card.Kind || card.Error == "" ||
				card.codexIdentity == [32]byte{} || card.codexIdentity != old.codexIdentity ||
				card.Codex == nil || card.Codex.PlanSource != "" ||
				old.Codex == nil || old.Codex.PlanSource != "usage-response" {
				// Every new response replaces old evidence, even when unsupported.
				// Failed requests retain only authenticated, same-identity evidence.
				continue
			}
			// Rejected Free/unsupported responses have no gauges, but their plan
			// and windows still diagnose account mismatch. Keep their original
			// age independently of usable quota, and preserve the new error.
			card.Codex, card.Plan, card.Gauges, card.FetchedAt = old.Codex, old.Plan, old.Gauges, old.FetchedAt
			cards[i] = card
			continue
		}
		sameBillingMonth := old.Billing != nil && old.Billing.Month == card.FetchedAt.UTC().Format("2006-01")
		if card.Error != "" && len(card.Gauges) == 0 && card.BalanceUSD == nil && card.Billing == nil && ok && old.Kind == card.Kind && (len(old.Gauges) > 0 || old.BalanceUSD != nil || sameBillingMonth) {
			// Keep the reading's original age, including through repeated failures.
			// Snapshots handed to callers remain immutable.
			card.Gauges, card.BalanceUSD, card.Billing, card.FetchedAt = old.Gauges, old.BalanceUSD, old.Billing, old.FetchedAt
			card.Plan, card.AllowanceSource, card.ObservedAt = old.Plan, old.AllowanceSource, old.ObservedAt
			cards[i] = card
		}
	}
	return cards
}

// Snapshot returns the most recent snapshot. Before the first refresh lands
// it waits up to firstRefreshWait for one to finish, so pollers never cache
// an empty snapshot.
func (s *Store) Snapshot() Snapshot {
	select {
	case <-s.ready:
	case <-time.After(firstRefreshWait):
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshot
}

// FetchAll returns the card list in dashboard display order. Cards fetch
// concurrently so the slowest provider bounds the wall time. Providers that
// fail return a card carrying the error; nothing panics or blocks long.
func (f *Fetcher) FetchAll(ctx context.Context) []Card {
	fetchers := [](func(context.Context) Card){
		f.codex, f.opencodeGo, f.antigravity, f.opencodeZen, f.moonshot, f.claude, f.museCode, f.grokActivity,
	}
	if org := envValue("GITHUB_BILLING_ORG"); org != "" {
		fetchers = append(fetchers, func(ctx context.Context) Card { return f.cachedGitHubBilling(ctx, org, time.Now()) })
	}
	cards := make([]Card, len(fetchers))
	var wg sync.WaitGroup
	for i, get := range fetchers {
		wg.Add(1)
		go func(i int, get func(context.Context) Card) {
			defer wg.Done()
			cards[i] = get(ctx)
			cards[i].SubscriptionPriceUSD = subscriptionPrice(cards[i].Key, time.Now())
		}(i, get)
	}
	wg.Wait()
	return cards
}

// --- credential helpers -----------------------------------------------------

func (f *Fetcher) readJSON(path string, into any) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(data, into) == nil
}

func (f *Fetcher) piAuth() map[string]any {
	var root map[string]any
	if f.readJSON(filepath.Join(f.Home, ".pi", "agent", "auth.json"), &root) {
		return root
	}
	return nil
}

func (f *Fetcher) authString(provider, key string) string {
	if auth := f.piAuth(); auth != nil {
		if entry, ok := auth[provider].(map[string]any); ok {
			if v, ok := entry[key].(string); ok && strings.TrimSpace(v) != "" {
				return strings.TrimSpace(v)
			}
		}
	}
	return ""
}

func envValue(names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// antigravityClient returns the Antigravity CLI's OAuth client credentials —
// needed to renew non-pi-owned sessions — from env or
// ~/.pi/agent/antigravity-client.json. They live in config, never in the
// repo (the values are extracted from the CLI's public binary).
func (f *Fetcher) antigravityClient() (string, string) {
	id := envValue("ANTIGRAVITY_OAUTH_CLIENT_ID")
	secret := envValue("ANTIGRAVITY_OAUTH_CLIENT_SECRET")
	if id != "" && secret != "" {
		return id, secret
	}
	var file struct {
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
	}
	if f.readJSON(filepath.Join(f.Home, ".pi", "agent", "antigravity-client.json"), &file) {
		return strings.TrimSpace(file.ClientID), strings.TrimSpace(file.ClientSecret)
	}
	return "", ""
}

// isWeeklyBoundary reports whether t lands within tol of a Monday 00:00 UTC
// reset (OpenCode's weekly window is a fixed calendar week).
func isWeeklyBoundary(t time.Time, tol time.Duration) bool {
	utc := t.UTC()
	secIntoDay := utc.Hour()*3600 + utc.Minute()*60 + utc.Second()
	day := int(utc.Weekday())
	secToMonday := ((8-day)%7)*86400 - secIntoDay
	secSinceMonday := ((day+6)%7)*86400 + secIntoDay
	tolSec := int64(tol.Seconds())
	d1, d2 := int64(secToMonday), int64(secSinceMonday)
	if d1 < 0 {
		d1 = -d1
	}
	if d2 < 0 {
		d2 = -d2
	}
	return min64(d1, d2) <= tolSec
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// projectEndPercent projects where usage lands at the end of the window if
// the current pace holds: usage% / elapsedFraction. It refuses only when the
// window start cannot be trusted at all (less than two minutes elapsed, or a
// fixed weekly window whose reset is not a clean Monday-midnight boundary).
// Unlike the statusline there is no minimum elapsed fraction — early-window
// projections are noisy but the dashboard labels them as pace estimates.
// Ported from the statusline extension.
func projectEndPercent(label string, usagePercent, resetInSec, durationSec float64,
	now time.Time, checkWeeklyBoundary bool) (float64, bool) {
	if resetInSec <= 0 || resetInSec > durationSec || durationSec <= 0 {
		return 0, false
	}
	if checkWeeklyBoundary && (label == "Weekly" || label == "W") {
		if !isWeeklyBoundary(now.Add(time.Duration(resetInSec*float64(time.Second))), 10*time.Minute) {
			return 0, false
		}
	}
	elapsedSec := durationSec - resetInSec
	if elapsedSec < 120 {
		return 0, false
	}
	return usagePercent / (elapsedSec / durationSec), true
}

// toNumber accepts the numeric shapes provider APIs emit (JSON numbers and
// numeric strings).
func toNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(n), 64); err == nil {
			return f, true
		}
	}
	return 0, false
}

// codexAccountIDFromJWT extracts the ChatGPT account id claim from the
// namespaced JWT payload, keeping multi-workspace requests deterministic.
func codexAccountIDFromJWT(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload, err := base64URLDecode(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			ChatGPTAccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.ChatGPTAccountID
}

func base64URLDecode(s string) ([]byte, error) {
	s = strings.TrimRight(s, "=")
	s = strings.NewReplacer("-", "+", "_", "/").Replace(s)
	for len(s)%4 != 0 {
		s += "="
	}
	return base64.StdEncoding.DecodeString(s)
}

// --- OpenCode Go / Zen ------------------------------------------------------

type openCodeConfig struct {
	WorkspaceID string
	Cookie      string
}

func (f *Fetcher) openCodeConfig() *openCodeConfig {
	ws := envValue("OPENCODE_GO_WORKSPACE_ID", "OPENCODE_ZEN_WORKSPACE_ID")
	cookie := envValue("OPENCODE_GO_AUTH_COOKIE", "OPENCODE_ZEN_AUTH_COOKIE")
	var file struct {
		WorkspaceID string `json:"workspaceId"`
		AuthCookie  string `json:"authCookie"`
	}
	if f.readJSON(filepath.Join(f.Home, ".pi", "agent", "opencode-go.json"), &file) {
		if ws == "" {
			ws = file.WorkspaceID
		}
		if cookie == "" {
			cookie = file.AuthCookie
		}
	}
	ws, cookie = normalizeWorkspaceID(ws), normalizeCookie(cookie)
	if ws == "" || cookie == "" {
		return nil
	}
	return &openCodeConfig{WorkspaceID: ws, Cookie: cookie}
}

func normalizeWorkspaceID(raw string) string {
	id := strings.TrimSpace(raw)
	if m := regexp.MustCompile(`/workspace/([^/?#]+)`).FindStringSubmatch(id); m != nil {
		id = m[1]
	}
	id = regexp.MustCompile(`/(billing|go)/?$`).ReplaceAllString(id, "")
	return strings.Trim(id, "/")
}

func normalizeCookie(raw string) string {
	c := strings.TrimSpace(raw)
	c = regexp.MustCompile(`(?i)^(set-cookie:|cookie:|authorization:|bearer)\s*`).ReplaceAllString(c, "")
	if m := regexp.MustCompile(`(?:^|;\s*)auth=([^;]+)`).FindStringSubmatch(c); m != nil {
		c = m[1]
	}
	return strings.TrimSpace(c)
}

func (f *Fetcher) dashboardPage(ctx context.Context, cfg *openCodeConfig, page string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		opencodeBase+"/workspace/"+cfg.WorkspaceID+"/"+page, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Cookie", "auth="+cfg.Cookie)
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) Gecko/20100101 Firefox/148.0")
	body, err := doRequest(f.Client, req)
	if err != nil {
		return nil, err
	}
	if bytes.Contains(lower(body), []byte("<title>openauth</title>")) || bytes.Contains(lower(body), []byte("openauth.js.org")) {
		return nil, fmt.Errorf("auth expired")
	}
	return body, nil
}

func (f *Fetcher) opencodeGo(ctx context.Context) Card {
	card := Card{Key: "opencode-go", Label: "OpenCode Go", Kind: KindQuota, FetchedAt: time.Now()}
	cfg := f.openCodeConfig()
	if cfg == nil {
		card.Error = "not configured"
		return card
	}
	body, err := f.dashboardPage(ctx, cfg, "go")
	if err != nil {
		card.Error = err.Error()
		return card
	}
	gauges := parseGoWindows(string(body))
	if gauges == nil {
		card.Error = "parse failed"
		return card
	}
	card.Gauges = gauges
	return card
}

type goWindowSpec struct {
	sourceKey string
	label     string
	duration  float64
	// weekly uses a fixed calendar week, so end-of-window projections are only
	// drawn when the reset lands on a Monday 00:00 UTC boundary (statusline
	// parity); the rolling windows project unconditionally.
	boundaryCheck bool
}

// goWindowSpecs mirrors the /go hydration keys in display order. duration is
// used to sanity-check resetInSec and to project end-of-window usage.
var goWindowSpecs = []goWindowSpec{
	{"rollingUsage", "5h", 5 * 3600, false},
	{"weeklyUsage", "Weekly", 7 * 86400, true},
	{"monthlyUsage", "Monthly", 30 * 86400, true},
}

var (
	zenBalance = regexp.MustCompile(`(?i)(?:\\?")?balance(?:\\?")?\s*:\s*(\d+)`)
)

// parseGoWindows scrapes usagePercent/resetInSec per window from the /go
// page. The page can carry several same-named keys (e.g. a units counter
// `monthlyUsage:455539320` before the hydration block), so every occurrence
// is tried until one yields valid data. A nil result means parse failure.
func parseGoWindows(text string) []Gauge {
	now := time.Now()
	gauges := make([]Gauge, 0, len(goWindowSpecs))
	for _, spec := range goWindowSpecs {
		gauge, ok := parseGoWindow(text, spec, now)
		if !ok {
			return nil
		}
		gauges = append(gauges, gauge)
	}
	return gauges
}

func parseGoWindow(text string, spec goWindowSpec, now time.Time) (Gauge, bool) {
	searchFrom := 0
	for {
		loc := findKeyIndexFrom(text, spec.sourceKey, searchFrom)
		if loc < 0 {
			return Gauge{}, false
		}
		end := min(loc+1500, len(text))
		for _, other := range goWindowSpecs {
			if other.sourceKey == spec.sourceKey {
				continue
			}
			if next := findKeyIndexFrom(text[loc+1:], other.sourceKey, 0); next >= 0 {
				if abs := loc + 1 + next; abs < end {
					end = abs
				}
			}
		}
		segment := text[loc:end]
		usage, reset := numericField(segment, "usagePercent"), numericField(segment, "resetInSec")
		if usage != nil && reset != nil && *usage >= 0 && *reset >= 0 && *reset <= spec.duration*2 {
			value := clamp(*usage, 0, 100)
			gauge := Gauge{
				Label:         spec.label,
				UsedPercent:   &value,
				ResetAt:       now.Add(time.Duration(*reset) * time.Second).UTC(),
				WindowSeconds: spec.duration,
			}
			if p, ok := projectEndPercent(spec.label, *usage, *reset, spec.duration, now, spec.boundaryCheck); ok {
				gauge.ProjectedEnd = &p
			}
			return gauge, true
		}
		searchFrom = loc + 1 // advance past this occurrence and try the next
	}
}

// findKeyIndexFrom locates `"key":` at or after `from` — tolerating
// backslash-escaped quotes and non-quoted keys — without matching substrings
// such as `timeMonthlyUsageUpdated`.
func findKeyIndexFrom(text, key string, from int) int {
	re := regexp.MustCompile(`\\?["']?` + key + `\\?["']?\s*:`)
	if m := re.FindStringIndex(text[from:]); m != nil {
		return from + m[0]
	}
	return -1
}

func numericField(segment, name string) *float64 {
	re := regexp.MustCompile(`(?i)(?:\\?")?` + name + `(?:\\?")?\s*:\s*(-?\d+(?:\.\d+)?)`)
	if m := re.FindStringSubmatch(segment); m != nil {
		if v, err := strconv.ParseFloat(m[1], 64); err == nil {
			return &v
		}
	}
	return nil
}

func (f *Fetcher) opencodeZen(ctx context.Context) Card {
	card := Card{Key: "opencode-zen", Label: "OpenCode Zen", Kind: KindBalance, FetchedAt: time.Now()}
	cfg := f.openCodeConfig()
	if cfg == nil {
		card.Error = "not configured"
		return card
	}
	body, err := f.dashboardPage(ctx, cfg, "billing")
	if err != nil {
		card.Error = err.Error()
		return card
	}
	return zenCardFromPage(body, time.Now())
}

// zenCardFromPage scrapes the prepaid balance from the /billing page HTML.
// The hydration JSON carries `balance:<integer>` in 1e-8 dollar units
// (e.g. 13151171185 = $131.51); the regex anchors on the hydration shape so
// page prose ("balance reaches") and boolean flags like `Balance:!0` are
// skipped.
func zenCardFromPage(body []byte, now time.Time) Card {
	card := Card{Key: "opencode-zen", Label: "OpenCode Zen", Kind: KindBalance, FetchedAt: now}
	if m := zenBalance.FindSubmatch(body); m != nil {
		raw, err := strconv.ParseFloat(string(m[1]), 64)
		if err == nil {
			balance := raw / 1e8
			card.BalanceUSD = &balance
			return card
		}
	}
	card.Error = "parse failed"
	return card
}

// --- Antigravity ------------------------------------------------------------

type agCreds struct {
	AccessToken  string
	RefreshToken string
	ExpiryMs     int64
	Source       string // "pi-auth" | "file" | "vault"
}

func (f *Fetcher) antigravityCandidates() []*agCreds {
	var out []*agCreds
	if c := f.piAntigravityCreds(); c != nil {
		out = append(out, c)
	}
	if c := f.fileAntigravityCreds(); c != nil {
		out = append(out, c)
	}
	if c := f.vaultAntigravityCreds(); c != nil {
		out = append(out, c)
	}
	return out
}

// piAntigravityCreds reads pi-antigravity's own OAuth session from
// ~/.pi/agent/auth.json — the account the active model actually uses.
func (f *Fetcher) piAntigravityCreds() *agCreds {
	auth := f.piAuth()
	if auth == nil {
		return nil
	}
	entry, ok := auth["antigravity"].(map[string]any)
	if !ok || entry["type"] != "oauth" {
		return nil
	}
	access, _ := entry["access"].(string)
	if strings.TrimSpace(access) == "" {
		return nil
	}
	c := &agCreds{AccessToken: strings.TrimSpace(access), Source: "pi-auth"}
	if r, ok := entry["refresh"].(string); ok {
		c.RefreshToken = r
	}
	if exp, ok := entry["expires"].(float64); ok {
		c.ExpiryMs = int64(exp)
	}
	return c
}

// fileAntigravityCreds reads a CodexBar-style export (flat or CLI-nested).
func (f *Fetcher) fileAntigravityCreds() *agCreds {
	path := envValue("ANTIGRAVITY_CREDENTIALS_FILE")
	if path == "" {
		path = filepath.Join(f.Home, ".codexbar", "antigravity", "oauth_creds.json")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var root map[string]any
	if json.Unmarshal(data, &root) != nil {
		return nil
	}
	tok := root
	if nested, ok := root["token"].(map[string]any); ok {
		tok = nested
	}
	return credsFromMap(tok, "file")
}

// vaultAntigravityCreds reads the Antigravity CLI session from Windows
// Credential Manager (same approach as the statusline extension).
func (f *Fetcher) vaultAntigravityCreds() *agCreds {
	if runtime.GOOS != "windows" {
		return nil
	}
	out := credManagerAntigravity()
	if out == "" {
		return nil
	}
	var root map[string]any
	if json.Unmarshal([]byte(out), &root) != nil {
		return nil
	}
	return credsFromMap(root, "vault")
}

func credsFromMap(m map[string]any, source string) *agCreds {
	// The CLI profile nests the session under "token" with the OAuth client
	// fields at the top level; desktop exports are flat. Accept both.
	if nested, ok := m["token"].(map[string]any); ok {
		m = nested
	}
	access, _ := m["access_token"].(string)
	if strings.TrimSpace(access) == "" {
		return nil
	}
	c := &agCreds{AccessToken: strings.TrimSpace(access), Source: source}
	if r, ok := m["refresh_token"].(string); ok {
		c.RefreshToken = r
	}
	if exp, ok := m["expiry"].(float64); ok {
		c.ExpiryMs = int64(exp)
	} else if exp, ok := m["expiry_date"].(float64); ok {
		c.ExpiryMs = int64(exp)
	} else if exp, ok := m["expiry"].(string); ok {
		if ts, err := time.Parse(time.RFC3339, exp); err == nil {
			c.ExpiryMs = ts.UnixMilli()
		}
	}
	return c
}

// credManagerAntigravity reads the CLI's "gemini:antigravity" credential blob
// from Windows Credential Manager (same approach as the statusline).
func credManagerAntigravity() string {
	script := `
Add-Type -TypeDefinition @'
using System;
using System.Runtime.InteropServices;
public class CredMan {
    [DllImport("advapi32.dll", EntryPoint="CredReadW", CharSet=CharSet.Unicode, SetLastError=true)]
    public static extern bool CredRead(string target, uint type, uint flags, out IntPtr cred);
    [DllImport("advapi32.dll")] public static extern void CredFree(IntPtr cred);
    [StructLayout(LayoutKind.Sequential, CharSet=CharSet.Unicode)]
    public struct CREDENTIAL {
        public uint Flags; public uint Type; public string TargetName; public string Comment;
        public System.Runtime.InteropServices.ComTypes.FILETIME LastWritten;
        public uint CredentialBlobSize; public IntPtr CredentialBlob;
        public uint Persist; public uint AttributeCount; public IntPtr Attributes;
        public string TargetAlias; public string UserName;
    }
}
'@ | Out-Null
$ptr = [IntPtr]::Zero
if (-not [CredMan]::CredRead('gemini:antigravity', 1, 0, [ref]$ptr)) { exit 1 }
$cred = [Runtime.InteropServices.Marshal]::PtrToStructure($ptr, [type][CredMan+CREDENTIAL])
$bytes = New-Object byte[] $cred.CredentialBlobSize
[Runtime.InteropServices.Marshal]::Copy($cred.CredentialBlob, $bytes, 0, $cred.CredentialBlobSize)
[CredMan]::CredFree($ptr)
[Console]::OutputEncoding = [Text.Encoding]::UTF8
[Console]::Out.Write([Text.Encoding]::UTF8.GetString($bytes))
`
	out, err := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

type agBucket struct {
	BucketID          string   `json:"bucketId"`
	Window            string   `json:"window"`
	Disabled          bool     `json:"disabled"`
	RemainingFraction *float64 `json:"remainingFraction"`
	ResetTime         string   `json:"resetTime"`
}

// agy printmode uses snake_case; the quota endpoint uses camelCase.
func (b *agBucket) UnmarshalJSON(data []byte) error {
	type quotaBucket agBucket
	var wire struct {
		quotaBucket
		BucketID  *string  `json:"bucketId"`
		Fraction  *float64 `json:"remainingFraction"`
		ResetTime *string  `json:"resetTime"`
		ID        *string  `json:"id"`
		Remaining *float64 `json:"remaining_fraction"`
		Reset     *string  `json:"reset_time"`
	}
	if err := json.Unmarshal(data, &wire); err != nil {
		return err
	}
	*b = agBucket(wire.quotaBucket)
	if wire.BucketID != nil {
		b.BucketID = *wire.BucketID
	}
	if wire.Fraction != nil {
		b.RemainingFraction = wire.Fraction
	}
	if wire.ResetTime != nil {
		b.ResetTime = *wire.ResetTime
	}
	if wire.ID != nil && wire.BucketID != nil && *wire.ID != *wire.BucketID ||
		wire.Remaining != nil && wire.Fraction != nil && *wire.Remaining != *wire.Fraction ||
		wire.Reset != nil && wire.ResetTime != nil && *wire.Reset != *wire.ResetTime {
		return fmt.Errorf("ambiguous antigravity bucket")
	}
	if wire.ID != nil {
		b.BucketID = *wire.ID
	}
	if wire.Remaining != nil {
		b.RemainingFraction = wire.Remaining
	}
	if wire.Reset != nil {
		b.ResetTime = *wire.Reset
	}
	return nil
}

var agSpecs = []struct {
	key, window, label string
}{
	{"gemini-5h", "5h", "Gemini 5h"},
	{"gemini-weekly", "weekly", "Gemini W"},
}

func (f *Fetcher) antigravityLegacy(ctx context.Context) Card {
	card := Card{Key: "antigravity", Label: "Antigravity", Kind: KindQuota, FetchedAt: time.Now()}
	candidates := f.antigravityCandidates()
	if len(candidates) == 0 {
		card.Error = "no session"
		return card
	}
	var lastErr error
	for _, creds := range candidates {
		// Only self-refresh sessions the CLI/file owns. pi-owned sessions are
		// rotated by pi itself; refreshing them here would invalidate pi's
		// stored refresh token.
		if creds.Source != "pi-auth" && creds.RefreshToken != "" &&
			(creds.ExpiryMs == 0 || creds.ExpiryMs <= time.Now().UnixMilli()+60_000) {
			if clientID, clientSecret := f.antigravityClient(); clientID != "" && clientSecret != "" {
				if renewed := f.refreshAntigravity(ctx, creds, clientID, clientSecret); renewed != nil {
					creds = renewed
				}
			}
		}
		gauges, err := f.fetchAntigravityQuota(ctx, creds.AccessToken)
		if err == nil {
			card.Gauges = gauges
			return card
		}
		lastErr = err
		if isAuthFailure(err) && creds.Source == "pi-auth" {
			// pi may have rotated the token since we read it; retry once with a
			// fresh read before falling through to the file/vault sources.
			if fresh := f.piAntigravityCreds(); fresh != nil && fresh.AccessToken != creds.AccessToken {
				if g2, e2 := f.fetchAntigravityQuota(ctx, fresh.AccessToken); e2 == nil {
					card.Gauges = g2
					return card
				} else {
					lastErr = e2
				}
			}
		}
	}
	card.Error = lastErr.Error()
	return card
}

func (f *Fetcher) fetchAntigravityQuota(ctx context.Context, token string) ([]Gauge, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agQuotaURL, strings.NewReader("{}"))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "antigravity")

	client := *f.Client
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}
	body, err := doRequest(&client, req)
	if err != nil {
		return nil, err
	}
	return parseAntigravityWindows(body)
}

func parseAntigravityWindows(body []byte) ([]Gauge, error) {
	var root struct {
		Groups []struct {
			Buckets []json.RawMessage `json:"buckets"`
		} `json:"groups"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, fmt.Errorf("parse failed")
	}
	now := time.Now()
	var gauges []Gauge
	for _, spec := range agSpecs {
		var matches []json.RawMessage
		for _, g := range root.Groups {
			for _, raw := range g.Buckets {
				// Count identities before decoding values, so a malformed duplicate
				// cannot make an ambiguous pool look like a single valid reading.
				var identity map[string]json.RawMessage
				if json.Unmarshal(raw, &identity) != nil {
					continue
				}
				var bucketID, id string
				_ = json.Unmarshal(identity["bucketId"], &bucketID)
				_ = json.Unmarshal(identity["id"], &id)
				if bucketID == spec.key || id == spec.key {
					matches = append(matches, raw)
				}
			}
		}
		if len(matches) != 1 {
			continue
		}
		var b agBucket
		if json.Unmarshal(matches[0], &b) != nil {
			continue
		}
		if b.Window != spec.window || b.Disabled || b.RemainingFraction == nil || *b.RemainingFraction < 0 || *b.RemainingFraction > 1 {
			continue
		}
		resetAt, err := time.Parse(time.RFC3339, b.ResetTime)
		if err != nil || !resetAt.After(now) {
			continue
		}
		duration := 5 * 3600
		if spec.window == "weekly" {
			duration = 7 * 86400
		}
		value := math.Round(100*(1-*b.RemainingFraction)*100) / 100
		gauge := Gauge{
			BucketID:      spec.key,
			Label:         spec.label,
			UsedPercent:   &value,
			ResetAt:       resetAt.UTC(),
			WindowSeconds: float64(duration),
		}
		if p, ok := projectEndPercent(spec.label, 100*(1-*b.RemainingFraction),
			time.Until(resetAt).Seconds(), float64(duration), now, false); ok {
			gauge.ProjectedEnd = &p
		}
		gauges = append(gauges, gauge)
	}
	if len(gauges) == 0 {
		return nil, fmt.Errorf("antigravity Gemini Models quota unavailable: no valid Gemini buckets")
	}
	return gauges, nil
}

func isAuthFailure(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "HTTP 400") ||
		strings.Contains(err.Error(), "HTTP 401") || strings.Contains(err.Error(), "HTTP 403"))
}

func (f *Fetcher) refreshAntigravity(ctx context.Context, creds *agCreds, clientID, clientSecret string) *agCreds {
	form := fmt.Sprintf("grant_type=refresh_token&refresh_token=%s&client_id=%s&client_secret=%s",
		creds.RefreshToken, clientID, clientSecret)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, agTokenURL, strings.NewReader(form))
	if err != nil {
		return nil
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	body, err := doRequest(f.Client, req)
	if err != nil {
		return nil
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if json.Unmarshal(body, &tok) != nil || tok.AccessToken == "" ||
		tok.ExpiresIn <= 0 || tok.ExpiresIn > 86_400 {
		return nil
	}
	refresh := creds.RefreshToken
	return &agCreds{AccessToken: tok.AccessToken, RefreshToken: refresh,
		ExpiryMs: time.Now().UnixMilli() + tok.ExpiresIn*1000, Source: creds.Source}
}

// --- Moonshot ---------------------------------------------------------------

func (f *Fetcher) moonshot(ctx context.Context) Card {
	card := Card{Key: "moonshot", Label: "Moonshot AI", Kind: KindBalance, FetchedAt: time.Now()}
	apiKey := envValue("MOONSHOT_API_KEY", "KIMI_K3_API_KEY")
	if apiKey == "" {
		apiKey = f.authString("moonshotai", "key")
	}
	if apiKey == "" {
		card.Error = "no API key"
		return card
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, moonshotBalance, nil)
	if err != nil {
		card.Error = err.Error()
		return card
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("User-Agent", "hs-pi-dashboard/0.1")

	body, err := doRequest(f.Client, req)
	if err != nil {
		card.Error = err.Error()
		return card
	}
	var root struct {
		Data struct {
			AvailableBalance any `json:"available_balance"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &root) != nil {
		card.Error = "parse failed"
		return card
	}
	if balance, ok := parseMoonshotBalance(root.Data.AvailableBalance); ok {
		card.BalanceUSD = &balance
		return card
	}
	card.Error = "parse failed"
	return card
}

// parseMoonshotBalance accepts the balance endpoint's number-or-string
// available_balance (the API has returned both shapes).
func parseMoonshotBalance(raw any) (float64, bool) {
	switch v := raw.(type) {
	case float64:
		return v, true
	case string:
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			return n, true
		}
	}
	return 0, false
}

// --- shared helpers ---------------------------------------------------------

func doRequest(client *http.Client, req *http.Request) ([]byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return body, nil
}

func lower(b []byte) []byte {
	return []byte(strings.ToLower(string(b)))
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
