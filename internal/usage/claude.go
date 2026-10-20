package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"path/filepath"
	"strings"
	"time"
)

const claudeUsageURL = "https://api.anthropic.com/api/oauth/usage"

// The endpoint rejects some valid sessions unless the request looks like a
// Claude Code client. CodexBar uses the same fallback version.
const claudeUserAgent = "claude-code/2.1.0"

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
	body, err := doRequest(f.Client, req)
	if err != nil {
		// Transport errors can contain response URLs or sensitive proxy details.
		// Publish only a fixed diagnosis or the HTTP status, never provider bodies.
		switch err.Error() {
		case "HTTP 401", "HTTP 403":
			card.Error = "Claude session expired or lacks usage access; sign in again with Claude Code"
		case "HTTP 429":
			card.Error = "Claude usage rate limited; retry on the next refresh"
		default:
			card.Error = "Claude usage request failed"
		}
		return card
	}
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
	var gauges []Gauge
	appendWindow := func(label string, duration float64, raw json.RawMessage, percentKey string) bool {
		var w map[string]json.RawMessage
		if json.Unmarshal(raw, &w) != nil || w == nil {
			return false
		}
		g := Gauge{Label: label, WindowSeconds: duration}
		var used float64
		if data := w[percentKey]; len(data) > 0 && string(data) != "null" && json.Unmarshal(data, &used) == nil && !math.IsInf(used, 0) && !math.IsNaN(used) && used >= 0 && used <= 100 {
			g.UsedPercent = &used
		}
		var reset string
		if json.Unmarshal(w["resets_at"], &reset) == nil {
			g.ResetAt, _ = time.Parse(time.RFC3339Nano, reset)
		}
		if g.UsedPercent == nil && g.ResetAt.IsZero() {
			return false
		}
		if g.UsedPercent != nil && !g.ResetAt.IsZero() {
			if p, ok := projectEndPercent(label, used, g.ResetAt.Sub(now).Seconds(), duration, now, false); ok {
				g.ProjectedEnd = &p
			}
		}
		gauges = append(gauges, g)
		return true
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
	// Root windows come first. Unscoped limit entries stand in for the
	// session and weekly windows only when the root window is unusable, so
	// responses carrying both shapes never report the same quota twice.
	limitFallback := func(label string, duration float64, kind, group string) {
		for _, entry := range limits {
			if entry.Kind == kind && entry.Group == group && appendWindow(label, duration, entry.raw, "percent") {
				return
			}
		}
	}
	for _, spec := range []struct {
		keys          []string
		label         string
		seconds       float64
		fallbackKind  string
		fallbackGroup string
	}{
		{[]string{"five_hour"}, "5h", 5 * 3600, "session", "session"},
		{[]string{"seven_day"}, "Weekly", 7 * 86400, "weekly_all", "weekly"},
		{[]string{"seven_day_sonnet"}, "Sonnet · weekly", 7 * 86400, "", ""},
		{[]string{"seven_day_opus"}, "Opus · weekly", 7 * 86400, "", ""},
		{[]string{"seven_day_oauth_apps"}, "OAuth apps · weekly", 7 * 86400, "", ""},
		// Accounts report Routines under several names, including the Cowork alias.
		{[]string{"seven_day_routines", "seven_day_claude_routines", "claude_routines", "routines", "routine", "seven_day_cowork", "cowork"}, "Routines · weekly", 7 * 86400, "", ""},
	} {
		found := false
		for _, key := range spec.keys {
			if found = appendWindow(spec.label, spec.seconds, root[key], "utilization"); found {
				break
			}
		}
		if !found && spec.fallbackKind != "" {
			limitFallback(spec.label, spec.seconds, spec.fallbackKind, spec.fallbackGroup)
		}
	}
	seen := map[string]bool{}
	for _, entry := range limits {
		if entry.Kind != "weekly_scoped" || entry.Group != "weekly" {
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
		if !seen[id] {
			seen[id] = appendWindow(name+" · weekly", 7*86400, entry.raw, "percent")
		}
	}
	if len(gauges) == 0 {
		return nil, fmt.Errorf("no Claude usage windows reported")
	}
	return gauges, nil
}
