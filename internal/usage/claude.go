package usage

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

// The endpoint rejects some valid sessions unless the request looks like a
// Claude Code client. CodexBar uses the same fallback version.
const claudeUserAgent = "claude-code/2.1.0"

// After a 429 the endpoint stays closed for a while, so each session waits for
// Retry-After (or a default cooldown, as CodexBar does) before polling again.
// The cap keeps a malformed header from hiding Claude for hours.
const (
	claudeDefaultCooldown = 5 * time.Minute
	claudeMaxCooldown     = time.Hour
)

// claudeRateGate remembers rate-limit cooldowns per session, keyed by token
// hash so credentials are never retained.
type claudeRateGate struct {
	mu    sync.Mutex
	until map[[sha256.Size]byte]time.Time
}

func (g *claudeRateGate) blockedUntil(token string, now time.Time) time.Time {
	g.mu.Lock()
	defer g.mu.Unlock()
	key := sha256.Sum256([]byte(token))
	until := g.until[key]
	if !until.After(now) {
		delete(g.until, key)
		return time.Time{}
	}
	return until
}

func (g *claudeRateGate) block(token, retryAfter string, now time.Time) time.Time {
	wait := claudeDefaultCooldown
	if seconds, err := strconv.Atoi(strings.TrimSpace(retryAfter)); err == nil && seconds > 0 {
		wait = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(retryAfter); err == nil && at.After(now) {
		wait = at.Sub(now)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.until == nil {
		g.until = map[[sha256.Size]byte]time.Time{}
	}
	key := sha256.Sum256([]byte(token))
	until := now.Add(min(wait, claudeMaxCooldown))
	if until.After(g.until[key]) {
		g.until[key] = until
	}
	return g.until[key]
}

func (g *claudeRateGate) clear(token string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.until, sha256.Sum256([]byte(token)))
}

func claudeRateLimited(until, now time.Time) string {
	return fmt.Sprintf("Claude usage rate limited; next attempt in %dm", int(math.Ceil(until.Sub(now).Minutes())))
}

// Read an existing subscription session. Authentication remains owned by Claude
// Code or Pi: this collector never renews tokens or writes credential files.
func (f *Fetcher) claudeToken() string {
	if token := envValue("CLAUDE_CODE_OAUTH_TOKEN"); token != "" {
		return token
	}
	dir := envValue("CLAUDE_CONFIG_DIR")
	if dir == "" {
		dir = filepath.Join(f.Home, ".claude")
	}
	var saved struct {
		OAuth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if f.readJSON(filepath.Join(dir, ".credentials.json"), &saved) {
		if token := strings.TrimSpace(saved.OAuth.AccessToken); token != "" {
			return token
		}
	}
	// A custom Claude profile must not silently switch to a different Pi account.
	if envValue("CLAUDE_CONFIG_DIR") == "" {
		return f.authString("anthropic", "access")
	}
	return ""
}

func (f *Fetcher) claude(ctx context.Context) Card {
	card := Card{Key: "claude", Label: "Claude", Kind: KindQuota, FetchedAt: time.Now()}
	token := f.claudeToken()
	if token == "" {
		card.Error = "no Claude subscription session; sign in with Claude Code or Pi"
		return card
	}
	if until := f.claudeGate.blockedUntil(token, card.FetchedAt); !until.IsZero() {
		card.Error = claudeRateLimited(until, card.FetchedAt)
		return card
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, claudeUsageURL, nil)
	if err != nil {
		card.Error = "could not prepare Claude usage request"
		return card
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-beta", "oauth-2025-04-20")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", claudeUserAgent)
	// Transport errors can contain response URLs or sensitive proxy details.
	// Publish only a fixed diagnosis, never provider bodies.
	resp, err := f.Client.Do(req)
	if err != nil {
		card.Error = "Claude usage request failed"
		return card
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		until := f.claudeGate.block(token, resp.Header.Get("Retry-After"), card.FetchedAt)
		card.Error = claudeRateLimited(until, card.FetchedAt)
		return card
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		card.Error = "Claude session expired or lacks usage access; sign in again with Claude Code"
		return card
	case resp.StatusCode < 200 || resp.StatusCode > 299 || err != nil:
		card.Error = "Claude usage request failed"
		return card
	}
	f.claudeGate.clear(token)
	card.Gauges, err = parseClaudeWindows(body, card.FetchedAt)
	if err != nil {
		card.Error = err.Error()
	}
	return card
}

// The OAuth schema is also consumed by CodexBar. See README for pinned source
// provenance. Decode windows separately so a malformed optional sibling cannot
// discard valid plan readings. Extra-usage dollars are not quota percentages.
func parseClaudeWindows(body []byte, now time.Time) ([]Gauge, error) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || root == nil {
		return nil, fmt.Errorf("invalid Claude usage response")
	}
	parseWindow := func(label string, duration float64, raw json.RawMessage, percentKey string) (Gauge, bool) {
		var w map[string]json.RawMessage
		if json.Unmarshal(raw, &w) != nil || w == nil {
			return Gauge{}, false
		}
		g := Gauge{Label: label, WindowSeconds: duration}
		// Over-limit readings are real: display is clamped to 100% while the
		// projection keeps the raw value, as the other collectors do.
		var used float64
		if data := w[percentKey]; len(data) > 0 && string(data) != "null" && json.Unmarshal(data, &used) == nil && !math.IsInf(used, 0) && !math.IsNaN(used) && used >= 0 {
			shown := clamp(used, 0, 100)
			g.UsedPercent = &shown
		}
		var reset string
		if json.Unmarshal(w["resets_at"], &reset) == nil {
			g.ResetAt, _ = time.Parse(time.RFC3339Nano, reset)
		}
		if g.UsedPercent == nil && g.ResetAt.IsZero() {
			return Gauge{}, false
		}
		if g.UsedPercent != nil && !g.ResetAt.IsZero() {
			if p, ok := projectEndPercent(label, used, g.ResetAt.Sub(now).Seconds(), duration, now, false); ok {
				g.ProjectedEnd = &p
			}
		}
		return g, true
	}
	type limitEntry struct {
		Kind, Group string
		Scope       struct {
			Model struct {
				ID   string `json:"id"`
				Name string `json:"display_name"`
			} `json:"model"`
		} `json:"scope"`
		raw json.RawMessage
	}
	var rawLimits []json.RawMessage
	_ = json.Unmarshal(root["limits"], &rawLimits)
	var limits []limitEntry
	for _, raw := range rawLimits {
		var entry limitEntry
		if json.Unmarshal(raw, &entry) == nil {
			entry.raw = raw
			limits = append(limits, entry)
		}
	}
	// Model-scoped weekly limits are read first: they supersede the legacy
	// per-model windows, which are skipped when a scoped entry covers the same
	// model, so one quota never shows twice.
	var scoped []Gauge
	var scopedModels []string
	seen := map[string]bool{}
	for _, entry := range limits {
		// Classify by kind and model scope; observed payloads use both
		// "weekly" and "model" as the display group.
		if entry.Kind != "weekly_scoped" {
			continue
		}
		name := strings.TrimSpace(entry.Scope.Model.Name)
		id := strings.TrimSpace(entry.Scope.Model.ID)
		if name == "" || strings.EqualFold(name, "All models") || strings.HasSuffix(strings.ToLower(id), "all-models") {
			continue
		}
		if id == "" {
			id = name
		}
		if seen[id] {
			continue
		}
		if g, ok := parseWindow(name+" weekly", 7*86400, entry.raw, "percent"); ok {
			seen[id] = true
			scoped = append(scoped, g)
			scopedModels = append(scopedModels, strings.ToLower(name+" "+id))
		}
	}
	coveredByScoped := func(model string) bool {
		for _, m := range scopedModels {
			if model != "" && strings.Contains(m, model) {
				return true
			}
		}
		return false
	}
	var gauges []Gauge
	// Structured session and weekly entries are canonical; the root
	// five_hour/seven_day windows are compatibility fallbacks, so responses
	// carrying both shapes never report the same quota twice.
	structured := func(label string, duration float64, kind string) bool {
		for _, entry := range limits {
			if entry.Kind != kind || entry.Scope.Model.Name != "" || entry.Scope.Model.ID != "" {
				continue
			}
			if g, ok := parseWindow(label, duration, entry.raw, "percent"); ok {
				gauges = append(gauges, g)
				return true
			}
		}
		return false
	}
	for _, spec := range []struct {
		keys    []string
		label   string
		seconds float64
		kind    string // structured limits entry preferred over the root keys
		model   string // legacy model window superseded by a scoped entry
	}{
		{[]string{"five_hour"}, "5h", 5 * 3600, "session", ""},
		{[]string{"seven_day"}, "Weekly", 7 * 86400, "weekly_all", ""},
		{[]string{"seven_day_sonnet"}, "Sonnet weekly", 7 * 86400, "", "sonnet"},
		{[]string{"seven_day_opus"}, "Opus weekly", 7 * 86400, "", "opus"},
		{[]string{"seven_day_oauth_apps"}, "OAuth apps weekly", 7 * 86400, "", ""},
		// Accounts report Routines under several names, including the Cowork alias.
		{[]string{"seven_day_routines", "seven_day_claude_routines", "claude_routines", "routines", "routine", "seven_day_cowork", "cowork"}, "Routines weekly", 7 * 86400, "", ""},
	} {
		if coveredByScoped(spec.model) || (spec.kind != "" && structured(spec.label, spec.seconds, spec.kind)) {
			continue
		}
		for _, key := range spec.keys {
			if g, ok := parseWindow(spec.label, spec.seconds, root[key], "utilization"); ok {
				gauges = append(gauges, g)
				break
			}
		}
	}
	gauges = append(gauges, scoped...)
	if len(gauges) == 0 {
		return nil, fmt.Errorf("no Claude usage windows reported")
	}
	return gauges, nil
}
