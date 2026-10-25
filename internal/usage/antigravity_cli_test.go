package usage

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAntigravityCommandRequiresReadOnlyUsageAndExcludesArgon(t *testing.T) {
	future := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	payload := `{"num_turns":0,"command":{"name":"usage","data":{"groups":[{"buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.8,"resetTime":"` + future + `"},{"bucketId":"gemini-argon-wtu-weekly","window":"weekly","remainingFraction":0.3,"resetTime":"` + future + `"}]}]}}}`
	f := &Fetcher{Home: t.TempDir(), agyRun: func(context.Context) ([]byte, error) { return []byte(payload), nil }}
	card := f.antigravity(context.Background())
	if card.Error != "" || len(card.Gauges) != 1 || card.Gauges[0].Label != "Gemini W" {
		t.Fatal(card)
	}
	for _, invalid := range []string{strings.Replace(payload, `"num_turns":0`, `"num_turns":1`, 1), strings.Replace(payload, `"name":"usage"`, `"name":"chat"`, 1), `{"command":{"name":"usage"}}`} {
		if _, err := parseAntigravityCommand([]byte(invalid)); err == nil {
			t.Fatal("accepted non-usage response")
		}
	}
}
func TestAntigravityMissingSessionDoesNotLaunchCLI(t *testing.T) {
	f := &Fetcher{Home: t.TempDir()}
	t.Setenv("HS_PI_DASHBOARD_AGY_BIN", "must-not-run")
	if _, err := f.runAntigravityUsage(context.Background()); err == nil {
		t.Fatal("no session accepted")
	}
}
func TestUsageCommandBufferCapAppliesThroughCopy(t *testing.T) {
	var b usageCommandBuffer
	if _, err := io.Copy(&b, struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 5<<20))}); err == nil || b.buffer.Len() > 4<<20 {
		t.Fatal("unbounded output", err)
	}
}

func TestAntigravityLiveFallbackHasWarningWithoutRefreshError(t *testing.T) {
	home := t.TempDir()
	auth := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(auth, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(auth, "auth.json"), []byte(`{"antigravity":{"type":"oauth","access":"fixture"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	payload := `{"groups":[{"buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.8,"resetTime":"` + future + `"}]}]}`
	f := &Fetcher{Home: home, agyRun: func(context.Context) ([]byte, error) { return nil, errors.New("CLI unavailable") },
		Client: &http.Client{Transport: usageRoundTripper(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(payload)), Header: make(http.Header)}, nil
		})}}
	card := f.antigravity(context.Background())
	if len(card.Gauges) != 1 || card.Error != "" || !strings.Contains(card.Warning, "Gemini Models from quota endpoint") {
		t.Fatal(card)
	}
}

func TestAntigravityCLIAndFallbackShareRefreshDeadline(t *testing.T) {
	home := t.TempDir()
	auth := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(auth, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(auth, "auth.json"), []byte(`{"antigravity":{"type":"oauth","access":"fixture"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	var deadline time.Time
	called := false
	f := &Fetcher{Home: home, agyRun: func(ctx context.Context) ([]byte, error) {
		var ok bool
		deadline, ok = ctx.Deadline()
		if !ok || time.Until(deadline) > fetchTimeout || time.Until(deadline) <= 0 {
			t.Fatal("missing refresh budget", deadline)
		}
		return nil, errors.New("CLI failed")
	}, Client: &http.Client{Transport: usageRoundTripper(func(req *http.Request) (*http.Response, error) {
		called = true
		fallbackDeadline, ok := req.Context().Deadline()
		if !ok || !fallbackDeadline.Equal(deadline) {
			t.Fatal("fallback received a new budget", fallbackDeadline, deadline)
		}
		return nil, errors.New("fixture failure")
	})}}
	card := f.antigravity(context.Background())
	if !called || card.Error == "" {
		t.Fatal(called, card)
	}
	// An exhausted caller budget must not start a fallback request.
	called = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f.agyRun = func(ctx context.Context) ([]byte, error) { return nil, ctx.Err() }
	card = f.antigravity(ctx)
	if called || card.Error != "antigravity refresh deadline exceeded" {
		t.Fatal(called, card)
	}
}

func TestAntigravityCLIPrintBucketsMatchChiefContract(t *testing.T) {
	reset := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	// The printmode producer uses id/remaining_fraction/reset_time. The raw
	// quota API uses camelCase; Chief's pinned ag_gauges accepts both.
	payload := `{"num_turns":0,"command":{"name":"usage","data":{"groups":[{"buckets":[{"id":"gemini-weekly","window":"weekly","remaining_fraction":0.8,"reset_time":"` + reset + `"},{"id":"gemini-argon-wtu-weekly","window":"weekly","remaining_fraction":0.3,"reset_time":"` + reset + `"}]}]}}}`
	gauges, err := parseAntigravityCommand([]byte(payload))
	if err != nil || len(gauges) != 1 || gauges[0].Label != "Gemini W" || *gauges[0].UsedPercent != 20 {
		t.Fatal(gauges, err)
	}
	for _, fraction := range []string{`null`, `-0.1`, `1.1`} {
		bad := strings.Replace(payload, `"remaining_fraction":0.8`, `"remaining_fraction":`+fraction, 1)
		// Healthy Argon cannot substitute for missing Gemini.
		got, err := parseAntigravityCommand([]byte(bad))
		if err == nil || len(got) != 0 {
			t.Fatal(got, err)
		}
	}
}

func TestAntigravityFallbackCannotSubstituteUnrelatedQuota(t *testing.T) {
	// Synthetic command and endpoint responses. The only credentials are this
	// test-owned fake pi-auth fixture, and the transport cannot reach a provider.
	home := t.TempDir()
	auth := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(auth, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(auth, "auth.json"), []byte(`{"antigravity":{"type":"oauth","access":"fixture"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	unrelated := `{"groups":[{"name":"Gemini Models","buckets":[{"id":"3p-weekly","window":"weekly","remaining_fraction":0.9,"reset_time":"` + reset + `"},{"id":"gemini-argon-wtu-weekly","window":"weekly","remaining_fraction":0.8,"reset_time":"` + reset + `"}]}]}`
	called := false
	f := &Fetcher{Home: home, agyRun: func(context.Context) ([]byte, error) {
		return []byte(`{"num_turns":0,"command":{"name":"usage","data":` + unrelated + `}}`), nil
	}, Client: &http.Client{Transport: usageRoundTripper(func(*http.Request) (*http.Response, error) {
		called = true
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(unrelated)), Header: make(http.Header)}, nil
	})}}
	got := f.antigravity(context.Background())
	if !called || len(got.Gauges) != 0 || !strings.Contains(got.Error, "Gemini Models quota unavailable") || got.Warning != "" {
		t.Fatal(got, called)
	}
}
