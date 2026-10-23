package usage

import (
	"testing"
	"time"
)

func TestSubscriptionPricesAndExplicitPeriodTransition(t *testing.T) {
	now := time.Date(2026, 10, 23, 5, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		key  string
		want float64
	}{{"claude", 200}, {"codex", 200}, {"antigravity", 19.99}} {
		if got := subscriptionPrice(tc.key, now); got == nil || *got != tc.want {
			t.Fatal(tc.key, got)
		}
	}
	t.Setenv("HS_PI_DASHBOARD_CLAUDE_NEXT_PRICE_AT", now.Add(time.Hour).Format(time.RFC3339))
	if p := subscriptionPrice("claude", now); p == nil || *p != 200 {
		t.Fatal(p)
	}
	if p := subscriptionPrice("claude", now.Add(time.Hour)); p == nil || *p != 100 {
		t.Fatal(p)
	}
	t.Setenv("HS_PI_DASHBOARD_CODEX_PRICE_USD", "unknown")
	if subscriptionPrice("codex", now) != nil {
		t.Fatal("invalid price invented")
	}
	t.Setenv("HS_PI_DASHBOARD_CLAUDE_NEXT_PRICE_AT", "unknown")
	if subscriptionPrice("claude", now) != nil {
		t.Fatal("invalid billing date accepted")
	}
	if subscriptionPrice("grok", now) != nil {
		t.Fatal("Grok price invented")
	}
}
