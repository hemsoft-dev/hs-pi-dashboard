package usage

import (
	"math"
	"testing"
	"time"
)

func TestProjectionHonorsWeeklyResetAndElapsedBoundaries(t *testing.T) {
	monday := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	now := monday.Add(-84 * time.Hour)
	week := (7 * 24 * time.Hour).Seconds()
	for _, label := range []string{"Weekly", "W"} {
		for _, tc := range []struct {
			name   string
			offset time.Duration
			want   bool
		}{
			{"exact Monday", 0, true},
			{"ten minutes before", -10 * time.Minute, true},
			{"ten minutes after", 10 * time.Minute, true},
			{"too early", -10*time.Minute - time.Second, false},
			{"too late", 10*time.Minute + time.Second, false},
			{"wrong weekday", 24 * time.Hour, false},
		} {
			t.Run(label+"/"+tc.name, func(t *testing.T) {
				reset := monday.Add(tc.offset).Sub(now).Seconds()
				got, ok := projectEndPercent(label, 25, reset, week, now, true)
				if ok != tc.want {
					t.Fatalf("accepted=%v, want %v", ok, tc.want)
				}
				if tc.offset == 0 && got != 50 {
					t.Fatalf("half a week at 25%% projects to %v, want 50", got)
				}
			})
		}
	}
	local := now.In(time.FixedZone("fixture", -7*3600))
	if got, ok := projectEndPercent("W", 25, 84*3600, week, local, true); !ok || got != 50 {
		t.Fatalf("UTC boundary from local time: %v %v", got, ok)
	}
	for _, tc := range []struct {
		label string
		check bool
	}{{"Daily", true}, {"Weekly", false}} {
		if _, ok := projectEndPercent(tc.label, 25, 48*3600, week, now, tc.check); !ok {
			t.Fatalf("non-fixed weekly window rejected: %+v", tc)
		}
	}
	for _, tc := range []struct {
		name            string
		reset, duration float64
		want            bool
	}{
		{"just before two minutes", 880.001, 1000, false},
		{"exactly two minutes", 880, 1000, true},
		{"expired", 0, 1000, false},
		{"negative reset", -1, 1000, false},
		{"reset past window", 1001, 1000, false},
		{"invalid duration", 1, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := projectEndPercent("Daily", 12, tc.reset, tc.duration, now, false)
			if ok != tc.want || (tc.want && math.Abs(got-100) > 1e-9) {
				t.Fatalf("got %v, %v", got, ok)
			}
		})
	}
}

func TestConfiguredSubscriptionPricesMustBeFiniteAndPositive(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	for _, next := range []bool{false, true} {
		for _, tc := range []struct {
			raw   string
			want  float64
			valid bool
		}{
			{"37.25", 37.25, true}, {"0.01", 0.01, true},
			{"0", 0, false}, {"-1", 0, false}, {"NaN", 0, false},
			{"+Inf", 0, false}, {"-Inf", 0, false}, {"not a number", 0, false},
		} {
			t.Run(tc.raw+map[bool]string{false: "/current", true: "/next"}[next], func(t *testing.T) {
				t.Setenv("HS_PI_DASHBOARD_CODEX_PRICE_USD", "25")
				t.Setenv("HS_PI_DASHBOARD_CODEX_NEXT_PRICE_AT", "")
				t.Setenv("HS_PI_DASHBOARD_CODEX_NEXT_PRICE_USD", "")
				key := "HS_PI_DASHBOARD_CODEX_PRICE_USD"
				if next {
					t.Setenv("HS_PI_DASHBOARD_CODEX_NEXT_PRICE_AT", now.Format(time.RFC3339))
					key = "HS_PI_DASHBOARD_CODEX_NEXT_PRICE_USD"
				}
				t.Setenv(key, tc.raw)
				got := subscriptionPrice("codex", now)
				if !tc.valid {
					if got != nil {
						t.Fatalf("invalid price %q accepted: %v", tc.raw, *got)
					}
				} else if got == nil || *got != tc.want {
					t.Fatalf("valid price %q: got %v", tc.raw, got)
				}
			})
		}
	}
}
