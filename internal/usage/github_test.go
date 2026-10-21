package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
