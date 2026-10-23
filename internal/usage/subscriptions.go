package usage

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// Explicit transition dates avoid guessing the account's next billing period.
// Empty dates keep today's price; invalid configuration makes the price unknown.
func subscriptionPrice(provider string, now time.Time) *float64 {
	defaults := map[string]float64{"claude": 200, "codex": 200, "antigravity": 19.99}
	price, ok := defaults[provider]
	if !ok {
		return nil
	}
	prefix := "HS_PI_DASHBOARD_" + strings.ToUpper(provider) + "_"
	if value := envValue(prefix + "PRICE_USD"); value != "" {
		var err error
		price, err = strconv.ParseFloat(value, 64)
		if err != nil || price <= 0 || math.IsNaN(price) || math.IsInf(price, 0) {
			return nil
		}
	}
	if value := envValue(prefix + "NEXT_PRICE_AT"); value != "" {
		at, err := time.Parse(time.RFC3339, value)
		if err != nil {
			return nil
		}
		next := 100.0
		if provider == "antigravity" {
			next = price
		}
		if value := envValue(prefix + "NEXT_PRICE_USD"); value != "" {
			next, err = strconv.ParseFloat(value, 64)
			if err != nil || next <= 0 || math.IsNaN(next) || math.IsInf(next, 0) {
				return nil
			}
		}
		if !now.Before(at) {
			price = next
		}
	}
	return &price
}
