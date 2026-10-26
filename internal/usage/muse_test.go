package usage

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Package tests must never run a real provider CLI. Tests that need the
// command path set their own binary.
func TestMain(m *testing.M) {
	os.Setenv("HS_PI_DASHBOARD_MUSE_BIN", filepath.Join(os.TempDir(), "hs-pi-dashboard-no-muse-in-tests"))
	os.Exit(m.Run())
}

// measured.json was observed at asOf 2026-10-26 13:54:05 ET.
var museAsOf = time.UnixMilli(1793037245064).UTC()

func museFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "muse", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func parseMuseFixture(t *testing.T, name string, now time.Time) (Card, error) {
	t.Helper()
	card := Card{Key: "muse-code"}
	err := parseMuseUsage(museFixture(t, name), &card, now)
	return card, err
}

func TestMuseMeasuredAllowanceKeepsWindowsTierAndObservationTime(t *testing.T) {
	card, err := parseMuseFixture(t, "measured.json", museAsOf.Add(time.Minute))
	if err != nil || len(card.Gauges) != 2 {
		t.Fatal(card, err)
	}
	short, weekly := card.Gauges[0], card.Gauges[1]
	if short.BucketID != "window" || short.Label != "5h" || short.WindowSeconds != 18000 || *short.UsedPercent != 15 ||
		!short.ResetAt.Equal(time.UnixMilli(1793053104000)) {
		t.Fatalf("short=%+v", short)
	}
	if weekly.BucketID != "weekly" || weekly.Label != "Weekly" || weekly.WindowSeconds != 604800 || *weekly.UsedPercent != 5 ||
		!weekly.ResetAt.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("weekly=%+v", weekly)
	}
	// Pace is anchored at asOf: 15% after ~35.7m of 5h, 5% after ~17.9h of 7d.
	if short.ProjectedEnd == nil || *short.ProjectedEnd < 125.5 || *short.ProjectedEnd > 126.5 {
		t.Fatalf("short projection=%v", short.ProjectedEnd)
	}
	if weekly.ProjectedEnd == nil || *weekly.ProjectedEnd < 46 || *weekly.ProjectedEnd > 48 {
		t.Fatalf("weekly projection=%v", weekly.ProjectedEnd)
	}
	if card.Plan != "Muse Code Power Usage" || card.AllowanceSource != "mint" || !card.ObservedAt.Equal(museAsOf) {
		t.Fatalf("metadata=%+v", card)
	}
	wire, _ := json.Marshal(card)
	for _, field := range []string{`"plan":"Muse Code Power Usage"`, `"allowanceSource":"mint"`, `"observedAt":"2026-10-26T17:54:05.064Z"`, `"bucketId":"window"`} {
		if !strings.Contains(string(wire), field) {
			t.Fatalf("missing %s in %s", field, wire)
		}
	}
}

func TestMuseNullAllowanceIsUnavailableAndNeverSurfacesReasonOrHint(t *testing.T) {
	card, err := parseMuseFixture(t, "null-allowance.json", museAsOf)
	if err == nil || len(card.Gauges) != 0 || card.Plan != "" {
		t.Fatal(card, err)
	}
	for _, leaked := range []string{"example.com", "Fixture User", "synthetic", "login"} {
		if strings.Contains(err.Error(), leaked) {
			t.Fatalf("diagnostic leaked: %v", err)
		}
	}
}

func TestMuseGenuineZeroAndExhaustedStayNumeric(t *testing.T) {
	zero, err := parseMuseFixture(t, "zero.json", museAsOf)
	if err != nil || len(zero.Gauges) != 2 || *zero.Gauges[0].UsedPercent != 0 || *zero.Gauges[1].UsedPercent != 0 {
		t.Fatal(zero, err)
	}
	out, err := parseMuseFixture(t, "exhausted.json", museAsOf)
	if err != nil || *out.Gauges[0].UsedPercent != 100 || *out.Gauges[1].UsedPercent != 100 {
		t.Fatal(out, err)
	}
	// The bar clamps; the forecast keeps the raw over-limit reading.
	if p := out.Gauges[0].ProjectedEnd; p == nil || *p <= 1000 {
		t.Fatalf("projection=%v", p)
	}
}

func TestMusePartialAndMalformedWindowsAreDroppedIndependently(t *testing.T) {
	for _, name := range []string{"partial.json", "reset-beyond-window.json"} {
		card, err := parseMuseFixture(t, name, museAsOf)
		if err != nil || len(card.Gauges) != 1 {
			t.Fatal(name, card, err)
		}
		if name == "partial.json" && card.Gauges[0].Label != "Weekly" || name != "partial.json" && card.Gauges[0].Label != "5h" {
			t.Fatal(name, card.Gauges)
		}
	}
	if card, err := parseMuseFixture(t, "malformed-values.json", museAsOf); err == nil || len(card.Gauges) != 0 {
		t.Fatal(card, err)
	}
}

func TestMuseResetRolloverDropsTheExpiredWindow(t *testing.T) {
	// After the short window resets, the cached 15% no longer describes it.
	card, err := parseMuseFixture(t, "measured.json", time.UnixMilli(1793053104000).Add(time.Second))
	if err != nil || len(card.Gauges) != 1 || card.Gauges[0].Label != "Weekly" {
		t.Fatal(card, err)
	}
	if card, err := parseMuseFixture(t, "measured.json", time.Date(2026, 11, 2, 0, 0, 1, 0, time.UTC)); err == nil || len(card.Gauges) != 0 {
		t.Fatal(card, err)
	}
}

func TestMuseRejectsUnsupportedSchemaAndMalformedJSON(t *testing.T) {
	for name, want := range map[string]string{
		"unsupported-schema.json": "schema unsupported",
		"truncated.json":          "malformed",
	} {
		card, err := parseMuseFixture(t, name, museAsOf)
		if err == nil || !strings.Contains(err.Error(), want) || len(card.Gauges) != 0 {
			t.Fatal(name, card, err)
		}
	}
	for _, body := range []string{`{"allowance":null}`, `{"schemaVersion":"1","allowance":null}`, `[]`, `{"schemaVersion":1,"allowance":[]}`,
		`{"schemaVersion":1,"allowance":{"weekly":{"resetsAtMs":1793577600000,"usedPercent":5}}}`} {
		if err := parseMuseUsage([]byte(body), &Card{}, museAsOf); err == nil {
			t.Fatal("accepted", body)
		}
	}
}

func TestMuseExtraWindowsStayUnscopedAndTokensNeverBecomeQuota(t *testing.T) {
	card, err := parseMuseFixture(t, "extra-window.json", museAsOf)
	if err != nil || len(card.Gauges) != 2 {
		t.Fatal(card, err)
	}
	extra := card.Gauges[1]
	if extra.BucketID != "wonder-full-weekly" || extra.Label != "wonder-full-weekly" || extra.WindowSeconds != 0 || extra.ProjectedEnd != nil {
		t.Fatalf("extra=%+v", extra)
	}
	// Markup in the tier fails the allowlist instead of being echoed.
	if card.Plan != "" {
		t.Fatalf("plan=%q", card.Plan)
	}
}

func TestMuseCardCachesReadingsAndBacksOffFailures(t *testing.T) {
	calls := 0
	result := func(context.Context) ([]byte, error) { return nil, errors.New("muse usage command failed") }
	f := &Fetcher{museRun: func(ctx context.Context) ([]byte, error) { calls++; return result(ctx) }}
	start := time.Now()
	card := f.museCode(context.Background())
	if calls != 1 || card.Error != "muse usage command failed" || card.Key != "muse-code" || card.Label != "Muse Code" || card.Kind != KindQuota {
		t.Fatal(calls, card)
	}
	if again := f.museCode(context.Background()); calls != 1 || !again.FetchedAt.Equal(card.FetchedAt) {
		t.Fatal("refetched inside backoff", calls)
	}
	if got := f.muse.next.Sub(start); got < 5*time.Minute || got > 6*time.Minute {
		t.Fatalf("first backoff=%s", got)
	}
	for i, want := range []time.Duration{10, 20, 30, 30} {
		f.muse.next = time.Time{}
		f.museCode(context.Background())
		if got := f.muse.next.Sub(start); got < want*time.Minute || got > want*time.Minute+time.Minute {
			t.Fatalf("failure %d backoff=%s", i+2, got)
		}
	}
	// A reading resets the backoff and is reused for the minimum interval.
	future := time.Now().Add(time.Hour)
	result = func(context.Context) ([]byte, error) {
		return []byte(`{"schemaVersion":1,"allowance":{"asOfMs":` + jsonInt(time.Now().UnixMilli()) + `,"weekly":{"resetsAtMs":` + jsonInt(future.UnixMilli()) + `,"usedPercent":7}}}`), nil
	}
	f.muse.next = time.Time{}
	ok := f.museCode(context.Background())
	if ok.Error != "" || len(ok.Gauges) != 1 || f.muse.failures != 0 || ok.FreshForSeconds != museFreshFor.Seconds() {
		t.Fatal(ok)
	}
	if got := time.Until(f.muse.next); got < 4*time.Minute || got > museMinInterval {
		t.Fatal("cache interval", got)
	}
	before := calls
	if f.museCode(context.Background()); calls != before {
		t.Fatal("cached reading refetched")
	}
}

func jsonInt(v int64) string { b, _ := json.Marshal(v); return string(b) }

func TestMuseMissingCLIIsActionableAndLaunchesNothing(t *testing.T) {
	t.Setenv("HS_PI_DASHBOARD_MUSE_BIN", "")
	t.Setenv("PATH", t.TempDir())
	f := &Fetcher{Home: t.TempDir()}
	if _, err := f.runMuseUsage(context.Background()); err == nil || err.Error() != "muse CLI not installed" {
		t.Fatal(err)
	}
	t.Setenv("HS_PI_DASHBOARD_MUSE_BIN", filepath.Join(t.TempDir(), "muse"))
	if _, err := f.runMuseUsage(context.Background()); err == nil || err.Error() != "muse CLI not installed" {
		t.Fatal(err)
	}
}

func TestMuseCommandRunsUsageWithoutPAYGKeyLoginOrUpdate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	script := filepath.Join(dir, "muse")
	body := "#!/bin/sh\n" +
		`printf '%s|%s|%s|%s\n' "$*" "${META_API_KEY-unset}" "${MUSE_LOGIN-unset}" "${MUSE_NO_AUTO_UPDATE-unset}" > "` + record + "\"\n" +
		`echo '{"schemaVersion":1,"allowance":null}'` + "\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HS_PI_DASHBOARD_MUSE_BIN", script)
	t.Setenv("META_API_KEY", "fixture-not-a-key")
	t.Setenv("MUSE_LOGIN", "1")
	out, err := (&Fetcher{Home: dir}).runMuseUsage(context.Background())
	if err != nil || strings.TrimSpace(string(out)) != `{"schemaVersion":1,"allowance":null}` {
		t.Fatal(string(out), err)
	}
	got, _ := os.ReadFile(record)
	if strings.TrimSpace(string(got)) != "usage --json|unset|0|1" {
		t.Fatalf("invocation=%q", got)
	}
	failing := filepath.Join(dir, "failing")
	if err := os.WriteFile(failing, []byte("#!/bin/sh\necho secret-looking-stderr >&2\nexit 2\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HS_PI_DASHBOARD_MUSE_BIN", failing)
	if _, err := (&Fetcher{Home: dir}).runMuseUsage(context.Background()); err == nil || err.Error() != "muse usage command failed" {
		t.Fatal(err)
	}
}

func TestMuseCardsRetainPlanAndObservationWithCachedWindows(t *testing.T) {
	observed := time.Now().Add(-time.Minute).UTC()
	used := 15.0
	store := NewStore(&Fetcher{}, "demo", time.Minute)
	store.snapshot = Snapshot{Cards: []Card{{Key: "muse-code", Kind: KindQuota, Plan: "Muse Code Power Usage", AllowanceSource: "mint",
		ObservedAt: observed, FetchedAt: observed, Gauges: []Gauge{{Label: "5h", UsedPercent: &used}}}}}
	failed := Card{Key: "muse-code", Kind: KindQuota, Error: "muse usage timed out", FetchedAt: time.Now()}
	merged := store.retain([]Card{failed})
	if got := merged[0]; got.Error == "" || len(got.Gauges) != 1 || got.Plan != "Muse Code Power Usage" || got.AllowanceSource != "mint" ||
		!got.ObservedAt.Equal(observed) || !got.FetchedAt.Equal(observed) {
		t.Fatalf("retained=%+v", got)
	}
}
