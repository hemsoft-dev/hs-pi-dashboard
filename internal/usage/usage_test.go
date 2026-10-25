package usage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The live /go page carries a units counter (`monthlyUsage:455539320` followed
// by timeMonthlyUsageUpdated) BEFORE the hydration block; each window must
// scan past non-hydration occurrences to its real data.
func TestParseGoWindowsSkipsUnitsCounter(t *testing.T) {
	page := `<html>other stuff monthlyUsage:455539320,timeMonthlyUsageUpdated:$R[30]=new Date("2026-09-08T00:18:23.000Z"),` +
		`reloadError:null,rollingUsage:$R[34]={status:"ok",resetInSec:5181,usagePercent:47.8,usage:573776370,limit:1200000000},` +
		`weeklyUsage:$R[35]={status:"ok",resetInSec:512018,usagePercent:97.6,usage:2929247763,limit:3000000000},` +
		`monthlyUsage:$R[36]={status:"ok",resetInSec:1555200,usagePercent:73.2,usage:100,limit:200}</html>`
	gauges := parseGoWindows(page)
	if gauges == nil {
		t.Fatal("parse failed")
	}
	want := []struct {
		label string
		pct   float64
	}{
		{"5h", 47.8}, {"Weekly", 97.6}, {"Monthly", 73.2},
	}
	for i, w := range want {
		if gauges[i].Label != w.label || gauges[i].UsedPercent == nil || *gauges[i].UsedPercent != w.pct {
			t.Fatalf("gauge %d = %+v, want %s %.1f", i, gauges[i], w.label, w.pct)
		}
	}
}

func TestParseGoWindowsEscapedAndPlain(t *testing.T) {
	if gauges := parseGoWindows(`{\"rollingUsage\":{\"usagePercent\":5,\"resetInSec\":1000},` +
		`\"weeklyUsage\":{\"usagePercent\":6,\"resetInSec\":100000},` +
		`\"monthlyUsage\":{\"usagePercent\":7,\"resetInSec\":1000000}}`); gauges == nil || len(gauges) != 3 {
		t.Fatalf("escaped = %+v", gauges)
	}
	if gauges := parseGoWindows(`"rollingUsage":{"usagePercent":5,"resetInSec":1000},` +
		`"weeklyUsage":{"usagePercent":6,"resetInSec":100000},` +
		`"monthlyUsage":{"usagePercent":7,"resetInSec":1000000}}`); gauges == nil || len(gauges) != 3 {
		t.Fatalf("plain = %+v", gauges)
	}
}

func TestParseGoWindowsIgnoresCaseTrap(t *testing.T) {
	// timeMonthlyUsageUpdated must not satisfy the monthly window even when
	// no hydration block exists.
	if gauges := parseGoWindows(`<p>timeMonthlyUsageUpdated:never</p>`); gauges != nil {
		t.Fatalf("expected nil, got %+v", gauges)
	}
}

func TestParseCodexWindowsNullSecondary(t *testing.T) {
	body := []byte(`{"rate_limit":{"primary_window":{"used_percent":0,"limit_window_seconds":604800,"reset_after_seconds":603701,"reset_at":1789435382},"secondary_window":null}}`)
	gauges, err := parseCodexWindows(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(gauges) != 1 || gauges[0].Label != "Weekly" || gauges[0].UsedPercent == nil || *gauges[0].UsedPercent != 0 {
		t.Fatalf("gauges = %+v", gauges)
	}
}

func TestParseAntigravityWindows(t *testing.T) {
	future := time.Now().Add(2 * time.Hour).UTC().Format(time.RFC3339)
	body := []byte(`{"groups":[{"buckets":[
		{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.9,"resetTime":"` + future + `"},
		{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.72,"resetTime":"` + future + `"},
		{"bucketId":"3p-5h","window":"5h","remainingFraction":0.5,"disabled":true,"resetTime":"` + future + `"},
		{"bucketId":"3p-weekly","window":"weekly","remainingFraction":0.1,"resetTime":"` + future + `"}]}]}`)
	gauges, err := parseAntigravityWindows(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(gauges) != 2 {
		t.Fatalf("gauges = %d, want 2 (only Gemini buckets)", len(gauges))
	}
	if gauges[0].Label != "Gemini 5h" || gauges[0].UsedPercent == nil || *gauges[0].UsedPercent != 10 {
		t.Fatalf("gemini 5h = %+v", gauges[0])
	}
	if gauges[1].Label != "Gemini W" || gauges[1].UsedPercent == nil || *gauges[1].UsedPercent != 28 {
		t.Fatalf("gemini W = %+v", gauges[1])
	}
}

func TestParseAntigravityWindowsFailsClosed(t *testing.T) {
	if _, err := parseAntigravityWindows([]byte(`{"groups":[]}`)); err == nil {
		t.Fatal("expected error when no buckets qualify")
	}
}

func TestParseZenBalanceFromPage(t *testing.T) {
	// balance is carried in 1e-8 dollar units: 13151171185 = $131.51171185.
	card := zenCardFromPage([]byte(`html "balance":13151171185 more html`), time.Now())
	if card.BalanceUSD == nil || *card.BalanceUSD < 131.5 || *card.BalanceUSD > 131.52 {
		t.Fatalf("balance = %+v", card.BalanceUSD)
	}
	broken := zenCardFromPage([]byte(`no balance on this page`), time.Now())
	if broken.Error == "" || broken.BalanceUSD != nil {
		t.Fatalf("broken page = %+v", broken)
	}
}

func TestParseMoonshotBalance(t *testing.T) {
	if v, ok := parseMoonshotBalance(any(float64(12.5))); !ok || v != 12.5 {
		t.Fatalf("number balance = %v %v", v, ok)
	}
	if v, ok := parseMoonshotBalance(any("13.75")); !ok || v != 13.75 {
		t.Fatalf("string balance = %v %v", v, ok)
	}
	if _, ok := parseMoonshotBalance(any(nil)); ok {
		t.Fatal("nil should not parse")
	}
}

func TestNormalizeOpenCodeConfig(t *testing.T) {
	if got := normalizeWorkspaceID("https://opencode.ai/workspace/ws-123/billing"); got != "ws-123" {
		t.Fatalf("workspace id = %q", got)
	}
	if got := normalizeCookie("Cookie: auth=abc123; other=1"); got != "abc123" {
		t.Fatalf("cookie = %q", got)
	}
	if got := normalizeCookie("bearer tok"); got != "tok" {
		t.Fatalf("cookie = %q", got)
	}
}

func TestCodexAccountIDFromJWT(t *testing.T) {
	header := base64URLEncode(`{}`)
	payload := base64URLEncode(`{"https://api.openai.com/auth":{"chatgpt_account_id":"acc-777"}}`)
	token := header + "." + payload + ".sig"
	if got := codexAccountIDFromJWT(token); got != "acc-777" {
		t.Fatalf("account id = %q", got)
	}
	if got := codexAccountIDFromJWT("not.a.jwt"); got != "" {
		t.Fatalf("garbage token should yield empty id, got %q", got)
	}
}

func TestAgentUsageEndpointServesCachedSnapshot(t *testing.T) {
	// The agent's /usage handler must serve the cache, not refetch per hit.
	store := NewStore(New(), "home", time.Minute)
	store.snapshot = Snapshot{Machine: "home", Cards: []Card{{Key: "moonshot", Label: "Moonshot AI", Kind: KindBalance}}}
	store.readyOnce.Do(func() { close(store.ready) })
	got := store.Snapshot()
	if got.Machine != "home" || len(got.Cards) != 1 || got.Cards[0].Kind != KindBalance {
		t.Fatalf("usage snapshot = %+v", got)
	}
}

func TestContextCancellationPropagates(t *testing.T) {
	f := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, card := range f.FetchAll(ctx) {
		if card.Error == "" {
			t.Fatalf("card %s unexpectedly succeeded with cancelled context", card.Key)
		}
	}
}

func base64URLEncode(s string) string {
	return strings.TrimRight(base64.StdEncoding.EncodeToString([]byte(s)), "=")
}

func TestCodexMissingUsageAndResetSerializeAsUnavailable(t *testing.T) {
	future := time.Now().Add(time.Hour).Unix()
	body := []byte(fmt.Sprintf(`{"rate_limit":{"primary_window":{"reset_at":%d,"limit_window_seconds":604800},"secondary_window":{"used_percent":0,"limit_window_seconds":18000}}}`, future))
	gauges, err := parseCodexWindows(body)
	if err != nil || len(gauges) != 2 {
		t.Fatalf("gauges=%+v err=%v", gauges, err)
	}
	if gauges[0].UsedPercent != nil || gauges[0].ProjectedEnd != nil {
		t.Fatalf("missing usage became numeric: %+v", gauges[0])
	}
	if gauges[1].UsedPercent == nil || *gauges[1].UsedPercent != 0 || !gauges[1].ResetAt.IsZero() {
		t.Fatalf("valid zero or absent reset lost: %+v", gauges[1])
	}
	encoded, err := json.Marshal(gauges)
	if err != nil {
		t.Fatal(err)
	}
	var wire []map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if string(wire[0]["usedPercent"]) != "null" || string(wire[1]["usedPercent"]) != "0" || wire[1]["resetAt"] != nil {
		t.Fatalf("wire=%s", encoded)
	}
}

type usageRoundTripper func(*http.Request) (*http.Response, error)

func (f usageRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestRefreshRetainsOriginalReadingOnFailuresAndRecovers(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".pi", "agent"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".pi", "agent", "auth.json"), []byte(`{"openai-codex":{"access":"fixture"},"moonshotai":{"key":"fixture"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MOONSHOT_API_KEY", "fixture")
	t.Setenv("KIMI_K3_API_KEY", "")
	var phase, codexRequests atomic.Int32
	client := &http.Client{Transport: usageRoundTripper(func(r *http.Request) (*http.Response, error) {
		status, body := 503, `{}`
		if r.URL.String() == codexUsageURL {
			codexRequests.Add(1)
			if phase.Load() != 1 {
				status = 200
				body = fmt.Sprintf(`{"rate_limit":{"primary_window":{"used_percent":%d,"limit_window_seconds":604800}}}`, phase.Load()*25)
			}
		} else if r.URL.String() == moonshotBalance && phase.Load() != 1 {
			status = 200
			body = fmt.Sprintf(`{"data":{"available_balance":%d}}`, 40+phase.Load())
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})}
	store := NewStore(&Fetcher{Client: client, Home: home}, "demo", time.Minute)
	store.refresh(context.Background())
	original := store.Snapshot()
	before, _ := json.Marshal(original)
	find := func(s Snapshot, key string) Card {
		for _, c := range s.Cards {
			if c.Key == key {
				return c
			}
		}
		t.Fatalf("missing %s", key)
		return Card{}
	}
	initial := find(original, "codex")
	if initial.Error != "" || initial.Gauges[0].UsedPercent == nil || *initial.Gauges[0].UsedPercent != 0 {
		t.Fatalf("initial=%+v", initial)
	}
	phase.Store(1)
	for i := 0; i < 2; i++ {
		store.refresh(context.Background())
		failed := store.Snapshot()
		quota := find(failed, "codex")
		balance := find(failed, "moonshot")
		if quota.Error == "" || len(quota.Gauges) != 1 || *quota.Gauges[0].UsedPercent != 0 || !quota.FetchedAt.Equal(initial.FetchedAt) {
			t.Fatalf("failed=%+v", quota)
		}
		if balance.Error == "" || balance.BalanceUSD == nil || *balance.BalanceUSD != 40 || !balance.FetchedAt.Equal(find(original, "moonshot").FetchedAt) {
			t.Fatalf("balance=%+v", balance)
		}
	}
	after, _ := json.Marshal(original)
	if string(before) != string(after) {
		t.Fatal("published snapshot mutated")
	}
	phase.Store(2)
	store.refresh(context.Background())
	recovered := store.Snapshot()
	quota := find(recovered, "codex")
	balance := find(recovered, "moonshot")
	if quota.Error != "" || *quota.Gauges[0].UsedPercent != 50 || !quota.FetchedAt.After(initial.FetchedAt) || balance.Error != "" || *balance.BalanceUSD != 42 {
		t.Fatalf("recovery=%+v", recovered)
	}
	if codexRequests.Load() != 4 {
		t.Fatalf("requests=%d", codexRequests.Load())
	}
}

func TestAntigravityArgonCannotSupplyGeminiMetrics(t *testing.T) {
	reset := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	body := []byte(`{"groups":[{"buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.8,"resetTime":"` + reset + `"},{"bucketId":"gemini-argon-wtu-weekly","window":"weekly","remainingFraction":0.3,"resetTime":"` + reset + `"}]}]}`)
	gauges, err := parseAntigravityWindows(body)
	if err != nil || len(gauges) != 1 || gauges[0].Label != "Gemini W" || *gauges[0].UsedPercent != 20 {
		t.Fatal(gauges, err)
	}
}
