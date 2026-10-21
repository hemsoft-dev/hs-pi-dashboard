package usage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/HemSoft/hs-pi-dashboard/internal/githubcli"
	"io"
	"math"
	"net/http"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Billing preserves organization usage-summary and AI-credit reports separately.
type Billing struct {
	Organization     string           `json:"organization"`
	Month            string           `json:"month"`
	Credits          float64          `json:"credits"`
	ProjectedCredits float64          `json:"projectedCredits"`
	CreditGrossUSD   float64          `json:"creditGrossUsd"`
	CreditNetUSD     float64          `json:"creditNetUsd"`
	NetUSD           float64          `json:"netUsd"`
	GrossUSD         float64          `json:"grossUsd"`
	ProjectedNetUSD  float64          `json:"projectedNetUsd"`
	Products         []BillingProduct `json:"products"`
	Budgets          []BillingBudget  `json:"budgets"`
	Attention        bool             `json:"attention"`
	CopilotPaidUsage string           `json:"copilotPaidUsage,omitempty"`
}
type BillingProduct struct {
	Product       string  `json:"product"`
	NetUSD        float64 `json:"netUsd"`
	GrossUSD      float64 `json:"grossUsd"`
	HasStopBudget bool    `json:"hasStopBudget"`
	Attention     bool    `json:"attention"`
	SeatNetUSD    float64 `json:"seatNetUsd"`
	UsageNetUSD   float64 `json:"usageNetUsd"`
}
type BillingBudget struct {
	Products   []string `json:"products"`
	Type       string   `json:"type"`
	Scope      string   `json:"scope"`
	Entity     string   `json:"entity,omitempty"`
	Amount     float64  `json:"amount"`
	StopsUsage bool     `json:"stopsUsage"`
}
type billingResponse struct {
	Data        []byte
	Status      int
	RateLimited bool
	RetryAt     time.Time
}
type billingRunner func(context.Context, string) (billingResponse, error)

var orgPattern = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})$`)

// githubBilling uses gh's existing host configuration. Tokens are neither read
// into the dashboard nor placed in command arguments or browser payloads.
func (f *Fetcher) githubBilling(ctx context.Context, org string, now time.Time) Card {
	card := Card{Key: "github-billing:" + strings.ToLower(org), Label: "GitHub (" + org + ")", Kind: "billing", FetchedAt: now}
	if !orgPattern.MatchString(org) {
		card.Error = "invalid GitHub billing organization"
		return card
	}
	policy := envValue("GITHUB_COPILOT_PAID_USAGE")
	if policy != "" && policy != "disabled" && policy != "enabled" {
		card.Error = "invalid GITHUB_COPILOT_PAID_USAGE; use disabled or enabled, or leave unset"
		return card
	}
	c, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	f.githubRateMu.Lock()
	until := f.githubRateUntil[org]
	f.githubRateMu.Unlock()
	if until.After(now) {
		card.Error = "GitHub billing rate limited; retry after the limit resets"
		return card
	}
	rawRun := f.githubRun
	if rawRun == nil {
		rawRun = runGitHubBilling
	}
	run := func(ctx context.Context, path string) (billingResponse, error) {
		response, err := rawRun(ctx, path)
		if response.RateLimited || response.Status == 429 {
			retry := response.RetryAt
			if !retry.After(now) {
				retry = now.Add(time.Minute)
			}
			f.githubRateMu.Lock()
			if f.githubRateUntil == nil {
				f.githubRateUntil = map[string]time.Time{}
			}
			if retry.After(f.githubRateUntil[org]) {
				f.githubRateUntil[org] = retry
			}
			f.githubRateMu.Unlock()
		}
		return response, err
	}
	query := fmt.Sprintf("?year=%d&month=%d", now.UTC().Year(), now.UTC().Month())
	base := "/organizations/" + org + "/settings/billing/"
	var credit, summary []byte
	var budgets []BillingBudget
	var errs [3]error
	done := make(chan int, 3)
	for i, path := range []string{base + "ai_credit/usage" + query, base + "usage/summary" + query, base + "budgets?per_page=100&page=1"} {
		go func(i int, path string) {
			defer func() { done <- i }()
			if i == 2 {
				budgets, errs[i] = readBudgets(c, run, base)
				return
			}
			resp, e := run(c, path)
			if e = billingError(resp, e); e != nil {
				errs[i] = e
				return
			}
			if i == 0 {
				credit = resp.Data
			} else {
				summary = resp.Data
			}
		}(i, path)
	}
	for range 3 {
		<-done
	}
	for _, e := range errs {
		if e != nil {
			card.Error = e.Error()
			return card
		}
	}
	b, e := parseBilling(credit, summary, budgets, org, now)
	if e != nil {
		card.Error = e.Error()
		return card
	}
	card.Billing = b
	b.CopilotPaidUsage = policy
	return card
}
func billingError(resp billingResponse, err error) error {
	if resp.Status == 429 || resp.RateLimited {
		return errors.New("GitHub billing rate limited; retry after the limit resets")
	}
	if resp.Status == 401 || resp.Status == 403 || resp.Status == 404 {
		return errors.New("GitHub billing access unavailable; check org owner access and token admin:org scope on the agent")
	}
	if err != nil || resp.Status != 200 {
		return errors.New("GitHub billing API unavailable; check gh installation and host connectivity")
	}
	return nil
}

type billingBuffer struct{ bytes.Buffer }

func (b *billingBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4<<20 {
		return 0, errors.New("billing response too large")
	}
	return b.Buffer.Write(p)
}
func runGitHubBilling(ctx context.Context, path string) (billingResponse, error) {
	cmd := exec.CommandContext(ctx, githubcli.Binary(), "api", "--hostname", "github.com", "--method", "GET", "--include", path, "-H", "Accept: application/vnd.github+json", "-H", "X-GitHub-Api-Version: 2026-03-10")
	cmd.WaitDelay = time.Second
	var out billingBuffer
	cmd.Stdout = &out
	cmd.Stderr = io.Discard
	err := cmd.Run()
	response, e := http.ReadResponse(bufio.NewReader(bytes.NewReader(out.Bytes())), nil)
	if e != nil {
		return billingResponse{}, errors.New("gh billing transport failed")
	}
	defer response.Body.Close()
	data, e := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if e != nil {
		return billingResponse{}, e
	}
	limited, until := billingRateLimit(response.StatusCode, response.Header, data, time.Now())
	return billingResponse{Data: data, Status: response.StatusCode, RateLimited: limited, RetryAt: until}, err
}
func billingRateLimit(status int, headers http.Header, data []byte, now time.Time) (bool, time.Time) {
	var body struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(data, &body)
	message := strings.ToLower(body.Message)
	limited := status == 429 || status == 403 && (headers.Get("X-RateLimit-Remaining") == "0" || headers.Get("Retry-After") != "" || strings.Contains(message, "rate limit") || strings.Contains(message, "abuse detection"))
	if !limited {
		return false, time.Time{}
	}
	until := now.Add(time.Minute)
	if seconds, err := strconv.ParseInt(headers.Get("Retry-After"), 10, 64); err == nil && seconds > 0 && seconds <= int64((time.Duration(1<<63-1))/time.Second) {
		candidate := now.Add(time.Duration(seconds) * time.Second)
		if candidate.After(until) {
			until = candidate
		}
	} else if stamp, err := http.ParseTime(headers.Get("Retry-After")); err == nil && stamp.After(until) {
		until = stamp
	}
	if headers.Get("X-RateLimit-Remaining") == "0" {
		if seconds, err := strconv.ParseInt(headers.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			stamp := time.Unix(seconds, 0)
			if stamp.After(until) {
				until = stamp
			}
		}
	}
	return true, until
}

func readBudgets(ctx context.Context, run billingRunner, base string) ([]BillingBudget, error) {
	budgets := []BillingBudget{}
	for page := 1; page <= 10; page++ {
		resp, e := run(ctx, fmt.Sprintf("%sbudgets?per_page=100&page=%d", base, page))
		if e = billingError(resp, e); e != nil {
			return nil, e
		}
		var wire struct {
			Budgets *[]struct {
				Product  string   `json:"budget_product_sku"`
				Products []string `json:"budget_product_skus"`
				Type     string   `json:"budget_type"`
				Scope    string   `json:"budget_scope"`
				Entity   string   `json:"budget_entity_name"`
				Amount   *float64 `json:"budget_amount"`
				Stop     *bool    `json:"prevent_further_usage"`
			} `json:"budgets"`
			Next *bool `json:"has_next_page"`
		}
		if json.Unmarshal(resp.Data, &wire) != nil || wire.Budgets == nil || wire.Next == nil {
			return nil, errors.New("invalid GitHub billing budgets response")
		}
		for _, v := range *wire.Budgets {
			products := v.Products
			if len(products) == 0 && v.Product != "" {
				products = []string{v.Product}
			}
			for _, product := range products {
				if strings.TrimSpace(product) == "" {
					return nil, errors.New("invalid GitHub billing budget product")
				}
			}
			if len(products) == 0 || strings.TrimSpace(v.Type) == "" || strings.TrimSpace(v.Scope) == "" || v.Amount == nil || !validMoney(*v.Amount) || v.Stop == nil {
				return nil, errors.New("invalid GitHub billing budget row")
			}
			budgets = append(budgets, BillingBudget{Products: products, Type: v.Type, Scope: v.Scope, Entity: v.Entity, Amount: *v.Amount, StopsUsage: *v.Stop})
		}
		if !*wire.Next {
			return budgets, nil
		}
	}
	return nil, errors.New("GitHub billing budgets incomplete; pagination limit reached")
}

type billingItem struct {
	Product       string   `json:"product"`
	SKU           string   `json:"sku"`
	GrossQuantity *float64 `json:"grossQuantity"`
	GrossAmount   *float64 `json:"grossAmount"`
	NetAmount     *float64 `json:"netAmount"`
}

func parseUsageReport(data []byte, org string, now time.Time) ([]billingItem, error) {
	var report struct {
		Organization string `json:"organization"`
		TimePeriod   struct {
			Year  int `json:"year"`
			Month int `json:"month"`
		} `json:"timePeriod"`
		Items *[]billingItem `json:"usageItems"`
	}
	if json.Unmarshal(data, &report) != nil || report.Items == nil || !strings.EqualFold(report.Organization, org) || report.TimePeriod.Year != now.UTC().Year() || report.TimePeriod.Month != int(now.UTC().Month()) {
		return nil, errors.New("invalid GitHub billing usage response or reporting month")
	}
	for _, v := range *report.Items {
		if strings.TrimSpace(v.Product) == "" || v.GrossAmount == nil || v.NetAmount == nil || !validMoney(*v.GrossAmount) || !validMoney(*v.NetAmount) {
			return nil, errors.New("invalid GitHub billing usage row")
		}
	}
	return *report.Items, nil
}
func validMoney(n float64) bool { return !math.IsNaN(n) && !math.IsInf(n, 0) && n >= 0 }
func productKey(s string) string {
	return strings.ToLower(strings.ReplaceAll(strings.ReplaceAll(s, " ", "_"), "-", "_"))
}
func parseBilling(creditData, summaryData []byte, budgets []BillingBudget, org string, now time.Time) (*Billing, error) {
	credit, e := parseUsageReport(creditData, org, now)
	if e != nil {
		return nil, e
	}
	summary, e := parseUsageReport(summaryData, org, now)
	if e != nil {
		return nil, e
	}
	utc := now.UTC()
	start := time.Date(utc.Year(), utc.Month(), 1, 0, 0, 0, 0, time.UTC)
	end := start.AddDate(0, 1, 0)
	elapsed := utc.Sub(start).Seconds()
	factor := 1.0
	if elapsed > 0 {
		factor = end.Sub(start).Seconds() / elapsed
	}
	b := &Billing{Organization: org, Month: start.Format("2006-01"), Budgets: budgets, Products: []BillingProduct{}}
	for _, v := range credit {
		if strings.EqualFold(v.Product, "Copilot") {
			if v.GrossQuantity == nil || !validMoney(*v.GrossQuantity) {
				return nil, errors.New("invalid GitHub AI credit quantity")
			}
			b.Credits += *v.GrossQuantity
			b.CreditGrossUSD += *v.GrossAmount
			b.CreditNetUSD += *v.NetAmount
		}
	}
	byProduct := map[string]*BillingProduct{}
	for _, v := range summary {
		p := byProduct[v.Product]
		if p == nil {
			p = &BillingProduct{Product: v.Product}
			byProduct[v.Product] = p
		}
		p.NetUSD += *v.NetAmount
		p.GrossUSD += *v.GrossAmount
		// The Business seat is an expected subscription charge, not AI-credit
		// overage. Unknown SKUs remain usage so a new charge cannot hide here.
		if strings.EqualFold(v.Product, "Copilot") && strings.EqualFold(v.SKU, "copilot_for_business") {
			p.SeatNetUSD += *v.NetAmount
		} else {
			p.UsageNetUSD += *v.NetAmount
		}
		b.NetUSD += *v.NetAmount
		b.GrossUSD += *v.GrossAmount
	}
	for _, p := range byProduct {
		for _, budget := range budgets {
			if budget.Type != "ProductPricing" || (budget.Scope != "organization" && budget.Scope != "enterprise") || (budget.Scope == "organization" && budget.Entity != "" && !strings.EqualFold(budget.Entity, org)) || !budget.StopsUsage {
				continue
			}
			for _, product := range budget.Products {
				if productKey(product) == productKey(p.Product) {
					p.HasStopBudget = true
				}
			}
		}
		p.Attention = p.UsageNetUSD > 0 && (!p.HasStopBudget || strings.EqualFold(p.Product, "Copilot"))
		b.Attention = b.Attention || p.Attention
		b.Products = append(b.Products, *p)
	}
	// The AI-credit report can reveal spend before it reaches the summary.
	b.Attention = b.Attention || b.CreditNetUSD > 0
	b.ProjectedCredits = b.Credits * factor
	b.ProjectedNetUSD = b.NetUSD * factor
	if !validMoney(b.ProjectedCredits) || !validMoney(b.ProjectedNetUSD) || !validMoney(b.GrossUSD) || !validMoney(b.CreditGrossUSD) || !validMoney(b.CreditNetUSD) {
		return nil, errors.New("invalid GitHub billing totals")
	}
	sort.Slice(b.Products, func(i, j int) bool { return b.Products[i].Product < b.Products[j].Product })
	return b, nil
}
