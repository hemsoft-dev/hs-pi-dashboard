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
	"strings"
	"time"
)

// CodexProvenance contains only allowlisted values. Plan is Card.Plan and comes
// from the authenticated usage response, never a locally decoded JWT claim.
// Windows describes reported shapes even when they cannot supply a quota.
type CodexProvenance struct {
	CredentialSource string   `json:"credentialSource"`
	PlanSource       string   `json:"planSource,omitempty"`
	Windows          []string `json:"windows,omitempty"`
}

func (f *Fetcher) codex(ctx context.Context) Card {
	card := Card{Key: "codex", Label: "Codex", Kind: KindQuota, FetchedAt: time.Now(), Codex: &CodexProvenance{CredentialSource: "none"}}
	// Read Pi once so the token and explicit account always come from one file.
	var pi struct {
		Codex struct {
			Access    string `json:"access"`
			AccountID string `json:"accountId"`
		} `json:"openai-codex"`
	}
	if !f.readJSON(filepath.Join(f.Home, ".pi", "agent", "auth.json"), &pi) {
		pi.Codex.Access, pi.Codex.AccountID = "", ""
	}
	token, accountID := strings.TrimSpace(pi.Codex.Access), strings.TrimSpace(pi.Codex.AccountID)
	if token != "" {
		card.Codex.CredentialSource = "pi"
	} else {
		var saved struct {
			Tokens struct {
				AccessToken string `json:"access_token"`
				AccountID   string `json:"account_id"`
			} `json:"tokens"`
		}
		if !f.readJSON(filepath.Join(f.Home, ".codex", "auth.json"), &saved) {
			saved.Tokens.AccessToken, saved.Tokens.AccountID = "", ""
		}
		token, accountID = strings.TrimSpace(saved.Tokens.AccessToken), strings.TrimSpace(saved.Tokens.AccountID)
		if token != "" {
			card.Codex.CredentialSource = "cli"
		}
	}
	if token == "" {
		card.Error = "no session"
		return card
	}
	if accountID == "" {
		accountID = codexAccountIDFromJWT(token)
	}
	identity := accountID
	if identity == "" {
		// Without an account identity even a token refresh invalidates the cache.
		identity = token
	}
	card.codexIdentity = sha256.Sum256([]byte(card.Codex.CredentialSource + "\x00" + identity))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, codexUsageURL, nil)
	if err != nil {
		card.Error = "Codex request unavailable"
		return card
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "hs-pi-dashboard/0.1")
	if accountID != "" {
		req.Header.Set("ChatGPT-Account-Id", accountID)
	}
	// Do not expose transport errors or provider bodies: either may echo secrets.
	resp, err := f.Client.Do(req)
	if err != nil {
		card.Error = "Codex request failed"
		return card
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		card.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return card
	}
	card.Codex.PlanSource = "usage-response"
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, (1<<20)+1))
	if readErr != nil || len(body) > 1<<20 {
		card.Plan, card.Error = "unknown", "Codex quota unavailable: malformed usage response"
		return card
	}
	card.Gauges, card.Plan, card.Codex.Windows, err = parseCodexWindows(body)
	if err != nil {
		card.Error = err.Error()
	}

	return card
}

// These are backend plan_type tokens, not marketing names or JWT claims.
// Unknown tokens are discarded. No plan currently proves a monthly quota.
func codexPlan(raw json.RawMessage) string {
	var plan string
	if json.Unmarshal(raw, &plan) != nil {
		return "unknown"
	}
	switch plan {
	case "free", "plus", "pro", "prolite", "promax", "team", "business", "enterprise", "edu":
		return plan
	default:
		return "unknown"
	}
}

type codexUsageResponse struct {
	Plan      json.RawMessage            `json:"plan_type"`
	RateLimit map[string]json.RawMessage `json:"rate_limit"`
}

// parseCodexWindows is also used by synthetic parser tests. Production attaches
// the same response's safe plan and rejected window shapes to the card.
func parseCodexWindows(body []byte) ([]Gauge, string, []string, error) {
	var root codexUsageResponse
	if json.Unmarshal(body, &root) != nil {
		return nil, "unknown", nil, fmt.Errorf("codex quota unavailable: malformed usage response")
	}
	plan := codexPlan(root.Plan)
	gauges, shapes, err := root.windows(plan)
	return gauges, plan, shapes, err
}

func (root codexUsageResponse) windows(plan string) ([]Gauge, []string, error) {
	var gauges []Gauge
	var shapes []string
	seen := make(map[string]bool)
	unsupported := false
	for _, name := range []string{"primary_window", "secondary_window"} {
		raw := root.RateLimit[name]
		if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
			continue
		}
		var w map[string]any
		if json.Unmarshal(raw, &w) != nil {
			unsupported = true
			continue
		}
		duration, ok := toNumber(w["limit_window_seconds"])
		shape, label := "unsupported", ""
		if ok {
			switch duration {
			case 18000:
				shape, label = "5h", "5h"
			case 604800:
				shape, label = "weekly", "Weekly"
			case 2592000:
				shape = "30d"
			}
		}
		if !seen[shape] {
			shapes = append(shapes, shape)
		}
		seen[shape] = true
		if label == "" {
			unsupported = true
			continue
		}
		g := Gauge{Label: label, WindowSeconds: duration}
		used, usedOK := toNumber(w["used_percent"])
		if usedOK && !math.IsNaN(used) && !math.IsInf(used, 0) && used >= 0 && used <= 100 {
			g.UsedPercent = &used
		}
		reset, resetOK := toNumber(w["reset_at"])
		// Bound before converting float64 to int64/time.Time. A bad reset cannot
		// create a forecast; a valid measured percentage can still be shown.
		if resetOK && !math.IsNaN(reset) && reset > 0 && reset < 253402300800 && math.Trunc(reset) == reset {
			g.ResetAt = time.Unix(int64(reset), 0).UTC()
			if p, valid := projectEndPercent(label, used, time.Until(g.ResetAt).Seconds(), duration, time.Now(), false); g.UsedPercent != nil && valid {
				g.ProjectedEnd = &p
			}
		}
		gauges = append(gauges, g)
	}
	if plan == "unknown" || plan == "free" {
		return nil, shapes, fmt.Errorf("codex quota unavailable: verified paid plan required")
	}
	if unsupported || len(gauges) == 0 {
		return nil, shapes, fmt.Errorf("codex quota unavailable: unsupported or missing quota window")
	}
	return gauges, shapes, nil
}
