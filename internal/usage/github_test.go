package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var billingNow = time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)

func billingFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, e := os.ReadFile(filepath.Join("testdata", "github", name+".json"))
	if e != nil {
		t.Fatal(e)
	}
	return b
}
func billingFake(t *testing.T) billingRunner {
	t.Helper()
	credits := billingFixture(t, "ai-credit")
	summary := billingFixture(t, "summary")
	budgets := billingFixture(t, "budgets")
	return func(_ context.Context, path string) (billingResponse, error) {
		data := summary
		if strings.Contains(path, "ai_credit") {
			data = credits
		} else if strings.Contains(path, "budgets") {
			data = budgets
		}
		return billingResponse{Status: 200, Data: data}, nil
	}
}
func TestGitHubRecordedFixturesAndZeroMonth(t *testing.T) {
	f := &Fetcher{githubRun: billingFake(t)}
	card := f.githubBilling(context.Background(), "example-org", billingNow)
	if card.Error != "" {
		t.Fatal(card.Error)
	}
	b := card.Billing
	if b.Credits != 0 || b.NetUSD != 0 || b.GrossUSD != 3 || len(b.Budgets) != 4 || len(b.Products) != 2 || b.Attention || !b.Products[0].HasStopBudget {
		t.Fatalf("wrong fixture result %+v", b)
	}
	empty := []byte(`{"organization":"example-org","timePeriod":{"year":2026,"month":10},"usageItems":[]}`)
	b, e := parseBilling(empty, empty, []BillingBudget{}, "example-org", billingNow)
	if e != nil || b.Credits != 0 || b.ProjectedCredits != 0 || b.NetUSD != 0 || b.Attention {
		t.Fatal("empty month not zero", e, b)
	}
}
func report(product string, gross, net, quantity float64) []byte {
	return []byte(fmt.Sprintf(`{"organization":"example-org","timePeriod":{"year":2026,"month":10},"usageItems":[{"product":%q,"grossAmount":%g,"netAmount":%g,"grossQuantity":%g}]}`, product, gross, net, quantity))
}
func TestGitHubProjectionAndAttention(t *testing.T) {
	for _, tc := range []struct {
		product     string
		stop        bool
		scope, kind string
		attention   bool
	}{{"Actions", true, "organization", "ProductPricing", false}, {"Actions", false, "organization", "ProductPricing", true}, {"Actions", true, "repository", "ProductPricing", true}, {"Actions", true, "organization", "SkuPricing", true}, {"Copilot", true, "organization", "ProductPricing", true}} {
		budgets := []BillingBudget{{Products: []string{productKey(tc.product)}, Type: tc.kind, Scope: tc.scope, Amount: 0, StopsUsage: tc.stop}}
		data := report(tc.product, 10, 2, 100)
		b, e := parseBilling(report("Copilot", 0, 0, 0), data, budgets, "example-org", billingNow)
		if e != nil || b.Attention != tc.attention || b.ProjectedNetUSD != 4 {
			t.Fatalf("%+v: %+v %v", tc, b, e)
		}
	}
	b, e := parseBilling(report("Copilot", 10, 1, 100), report("Actions", 0, 0, 0), nil, "example-org", billingNow)
	if e != nil || !b.Attention || b.Credits != 100 || b.ProjectedCredits != 200 {
		t.Fatal("credit-only spend not flagged", b, e)
	}
}
func TestGitHubErrorIsolationAndCredentialSafety(t *testing.T) {
	for _, tc := range []struct {
		status  int
		rate    bool
		message string
	}{{401, false, "admin:org"}, {403, false, "admin:org"}, {404, false, "admin:org"}, {429, false, "rate limited"}, {403, true, "rate limited"}, {500, false, "unavailable"}, {0, false, "unavailable"}} {
		f := &Fetcher{githubRun: func(context.Context, string) (billingResponse, error) {
			return billingResponse{Status: tc.status, RateLimited: tc.rate, Data: []byte("token=SECRET")}, errors.New("token=SECRET")
		}}
		c := f.githubBilling(context.Background(), "example-org", billingNow)
		if !strings.Contains(c.Error, tc.message) || strings.Contains(c.Error, "SECRET") || c.Billing != nil {
			t.Fatal(tc, c)
		}
	}
	var calls atomic.Int32
	f := &Fetcher{githubRun: func(context.Context, string) (billingResponse, error) { calls.Add(1); return billingResponse{}, nil }}
	c := f.githubBilling(context.Background(), "bad/org", billingNow)
	if c.Error == "" || calls.Load() != 0 {
		t.Fatal("invalid org reached transport")
	}
}
func TestGitHubBudgetPaginationAndSingularPluralShapes(t *testing.T) {
	run := billingFake(t)
	var pages int
	wrapper := func(ctx context.Context, path string) (billingResponse, error) {
		pages++
		resp, e := run(ctx, path)
		if pages == 1 {
			resp.Data = []byte(`{"budgets":[{"budget_product_skus":["actions","packages"],"budget_type":"ProductPricing","budget_scope":"organization","budget_amount":0,"prevent_further_usage":true}],"has_next_page":true}`)
		}
		return resp, e
	}
	budgets, e := readBudgets(context.Background(), wrapper, "/organizations/example-org/settings/billing/")
	if e != nil || pages != 2 || len(budgets) != 5 || len(budgets[0].Products) != 2 {
		t.Fatal(budgets, pages, e)
	}
	neverEnd := func(context.Context, string) (billingResponse, error) {
		return billingResponse{Status: 200, Data: []byte(`{"budgets":[],"has_next_page":true}`)}, nil
	}
	if _, e := readBudgets(context.Background(), neverEnd, "/"); e == nil {
		t.Fatal("unbounded budgets accepted")
	}
}
func TestGitHubRejectsMalformedAndWrongMonth(t *testing.T) {
	for _, bad := range []string{"null", "false", "0", "{}", `{"usageItems":null}`, strings.Replace(string(billingFixture(t, "summary")), `"month": 10`, `"month": 9`, 1), strings.Replace(string(billingFixture(t, "summary")), `"netAmount": 0`, `"netAmount": null`, 1)} {
		if _, e := parseUsageReport([]byte(bad), "example-org", billingNow); e == nil {
			t.Fatalf("accepted bad report %s", bad)
		}
	}
	for _, bad := range []string{"null", "false", "0", "{}", `{"budgets":null,"has_next_page":false}`, `{"budgets":[{"budget_product_sku":"actions","budget_type":"ProductPricing","budget_scope":"organization","budget_amount":0}],"has_next_page":false}`} {
		run := func(context.Context, string) (billingResponse, error) {
			return billingResponse{Status: 200, Data: []byte(bad)}, nil
		}
		if _, e := readBudgets(context.Background(), run, "/"); e == nil {
			t.Fatalf("accepted bad budgets %s", bad)
		}
	}
}
func TestGitHubRejectsBlankBudgetProducts(t *testing.T) {
	for _, products := range []string{`[""]`, `[" "]`, `["actions", "\t"]`} {
		data := []byte(`{"budgets":[{"budget_product_skus":` + products + `,"budget_type":"ProductPricing","budget_scope":"organization","budget_amount":0,"prevent_further_usage":true}],"has_next_page":false}`)
		run := func(context.Context, string) (billingResponse, error) {
			return billingResponse{Status: 200, Data: data}, nil
		}
		if _, err := readBudgets(context.Background(), run, "/"); err == nil {
			t.Fatalf("accepted blank product %s", products)
		}
	}
}

func TestGitHubReportMonthUTC(t *testing.T) {
	// Eastern is still September; the billing period must already be October UTC.
	eastern := time.FixedZone("EDT", -4*3600)
	now := time.Date(2026, 9, 30, 21, 0, 0, 0, eastern)
	data := report("Copilot", 0, 0, 0)
	if _, e := parseUsageReport(data, "example-org", now); e != nil {
		t.Fatal(e)
	}
}
func TestGitHubJSONNeverIncludesCredentials(t *testing.T) {
	c := (&Fetcher{githubRun: billingFake(t)}).githubBilling(context.Background(), "example-org", billingNow)
	b, e := json.Marshal(c)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), "token") || strings.Contains(string(b), "Authorization") {
		t.Fatal(string(b))
	}
}

func TestGitHubStoreKeepsSameMonthReadingAndRecovers(t *testing.T) {
	t.Setenv("GITHUB_BILLING_ORG", "example-org")
	var failing atomic.Bool
	fake := billingFake(t)
	run := func(ctx context.Context, path string) (billingResponse, error) {
		if failing.Load() {
			return billingResponse{Status: 503}, errors.New("down")
		}
		response, err := fake(ctx, path)
		if !strings.Contains(path, "budgets") {
			var wire map[string]any
			_ = json.Unmarshal(response.Data, &wire)
			now := time.Now().UTC()
			wire["timePeriod"] = map[string]int{"year": now.Year(), "month": int(now.Month())}
			response.Data, _ = json.Marshal(wire)
		}
		return response, err
	}
	f := &Fetcher{Home: t.TempDir(), githubRun: run, Client: &http.Client{Transport: usageRoundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New("other provider unavailable") })}}
	s := NewStore(f, "fixture", time.Minute)
	find := func() Card {
		for _, c := range s.Snapshot().Cards {
			if c.Kind == "billing" {
				return c
			}
		}
		t.Fatal("billing missing")
		return Card{}
	}
	s.refresh(context.Background())
	initial := find()
	if initial.Error != "" || initial.Billing == nil {
		t.Fatal(initial)
	}
	snapshot := s.Snapshot()
	before, _ := json.Marshal(snapshot)
	failing.Store(true)
	for range 2 {
		s.refresh(context.Background())
		cached := find()
		if cached.Error == "" || cached.Billing == nil || cached.Billing.GrossUSD != initial.Billing.GrossUSD || !cached.FetchedAt.Equal(initial.FetchedAt) {
			t.Fatal("cached values or age lost", cached)
		}
	}
	after, _ := json.Marshal(snapshot)
	if string(before) != string(after) {
		t.Fatal("published snapshot changed")
	}
	failing.Store(false)
	s.refresh(context.Background())
	if recovered := find(); recovered.Error != "" || !recovered.FetchedAt.After(initial.FetchedAt) {
		t.Fatal("did not recover", recovered)
	}
	// A failed new-month read must never be labeled with the previous month's totals.
	old := initial
	old.Billing = &Billing{Month: time.Now().UTC().AddDate(0, 0, -40).Format("2006-01"), NetUSD: 99}
	s.snapshot = Snapshot{Cards: []Card{old}}
	failing.Store(true)
	s.refresh(context.Background())
	if failed := find(); failed.Error == "" || failed.Billing != nil {
		t.Fatal("previous month retained", failed)
	}
}

func TestGitHubKeepsReportsSeparateWithoutDoubleCounting(t *testing.T) {
	credits := report("Copilot", 10, 1, 100)
	b, e := parseBilling(credits, report("Actions", 2, 0, 100), nil, "example-org", billingNow)
	if e != nil || !b.Attention || b.CreditNetUSD != 1 || b.NetUSD != 0 || b.ProjectedNetUSD != 0 || b.GrossUSD != 2 || len(b.Products) != 1 {
		t.Fatal("report provenance lost", b, e)
	}
	b, e = parseBilling(credits, report("Copilot", 10, 1, 100), nil, "example-org", billingNow)
	if e != nil || b.NetUSD != 1 || b.GrossUSD != 10 || b.ProjectedNetUSD != 2 {
		t.Fatal("overlapping reports double-counted", b, e)
	}
}
func TestGitHubEnterpriseProductBudgetCoverage(t *testing.T) {
	for _, tc := range []struct {
		scope, entity string
		covered       bool
	}{{"enterprise", "parent-enterprise", true}, {"organization", "example-org", true}, {"organization", "different-org", false}, {"cost_center", "division", false}, {"repository", "example-org/repo", false}} {
		budget := BillingBudget{Products: []string{"actions"}, Type: "ProductPricing", Scope: tc.scope, Entity: tc.entity, Amount: 0, StopsUsage: true}
		b, e := parseBilling(report("Copilot", 0, 0, 0), report("Actions", 2, 1, 100), []BillingBudget{budget}, "example-org", billingNow)
		if e != nil || b.Products[0].HasStopBudget != tc.covered || b.Attention == tc.covered {
			t.Fatal(tc, b, e)
		}
	}
}
func TestGitHubPrimaryAndSecondaryRateLimitSignals(t *testing.T) {
	for _, tc := range []struct {
		status                    int
		remaining, retry, message string
		limited                   bool
		delay                     time.Duration
	}{{403, "0", "", "", true, time.Minute}, {403, "42", "600", "", true, 10 * time.Minute}, {403, "42", "", "You have exceeded a secondary rate limit", true, time.Minute}, {403, "42", "", "Resource not accessible", false, 0}, {404, "42", "600", "not found", false, 0}, {429, "42", "", "", true, time.Minute}} {
		h := http.Header{}
		h.Set("X-RateLimit-Remaining", tc.remaining)
		h.Set("Retry-After", tc.retry)
		body, _ := json.Marshal(map[string]string{"message": tc.message})
		limited, until := billingRateLimit(tc.status, h, body, billingNow)
		if limited != tc.limited || limited && until.Sub(billingNow) != tc.delay {
			t.Fatal(tc, limited, until)
		}
	}
	h := http.Header{}
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Reset", fmt.Sprint(billingNow.Add(time.Hour).Unix()))
	if limited, until := billingRateLimit(403, h, nil, billingNow); !limited || until.Sub(billingNow) != time.Hour {
		t.Fatal("reset deadline ignored", until)
	}
}
func TestGitHubCooldownSuppressesRequestsUntilRetryDeadline(t *testing.T) {
	var count atomic.Int32
	var limited atomic.Bool
	limited.Store(true)
	good := billingFake(t)
	f := &Fetcher{githubRun: func(ctx context.Context, path string) (billingResponse, error) {
		count.Add(1)
		if limited.Load() {
			return billingResponse{Status: 403, RateLimited: true, RetryAt: billingNow.Add(10 * time.Minute)}, errors.New("limited")
		}
		return good(ctx, path)
	}}
	first := f.githubBilling(context.Background(), "example-org", billingNow)
	if !strings.Contains(first.Error, "rate limited") || count.Load() != 3 {
		t.Fatal(first, count.Load())
	}
	limited.Store(false)
	blocked := f.githubBilling(context.Background(), "example-org", billingNow.Add(time.Minute))
	if !strings.Contains(blocked.Error, "rate limited") || count.Load() != 3 {
		t.Fatal("request during cooldown", blocked, count.Load())
	}
	recovered := f.githubBilling(context.Background(), "example-org", billingNow.Add(11*time.Minute))
	if recovered.Error != "" || recovered.Billing == nil || count.Load() != 6 {
		t.Fatal("cooldown did not recover", recovered, count.Load())
	}
}

func TestGitHubIncludesSubscriptionSKUsAndFlagsAnyCopilotNet(t *testing.T) {
	b, err := parseBilling(report("Copilot", 0, 0, 0), billingFixture(t, "summary-subscription"), nil, "example-org", billingNow)
	if err != nil || b.NetUSD != .5 || b.GrossUSD != .5 || b.CreditNetUSD != 0 || b.CreditGrossUSD != 0 || b.Credits != 0 || !b.Attention || len(b.Products) != 1 || b.Products[0].Product != "Copilot" || b.Products[0].NetUSD != .5 || b.Products[0].GrossUSD != .5 || b.Products[0].HasStopBudget || !b.Products[0].Attention {
		t.Fatal("subscription summary row lost or conflated with AI credits", b, err)
	}
}
