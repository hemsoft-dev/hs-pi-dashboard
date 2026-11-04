package usage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestClaudeLocalProvenanceSeparatesAttemptFromSuccess(t *testing.T) {
	home := claudeHome(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "synthetic-session")
	status, calls := 200, 0
	f := &Fetcher{Home: home, museRun: noCLI, agyRun: noCLI, Client: &http.Client{Transport: claudeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != claudeUsageURL {
			return nil, errors.New("unused provider")
		}
		calls++
		h := make(http.Header)
		h.Set("Retry-After", "300")
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(`{"seven_day":{"utilization":35}}`)), Header: h}, nil
	})}}
	store := NewStore(f, "home", time.Minute)
	store.refresh(context.Background())
	good := claudeCard(t, store)
	if good.Claude == nil || good.Claude.SourceMachine != "home" || good.Claude.Status != claudeStatusOK ||
		!good.Claude.LastAttemptAt.Equal(good.FetchedAt) || !good.Claude.LastSuccessAt.Equal(good.FetchedAt) {
		t.Fatalf("success provenance=%+v", good.Claude)
	}

	// Real responses move the attempt, never the retained success.
	for _, tc := range []struct {
		code   int
		status string
		text   string
	}{
		{401, claudeStatusAuthRejected, "HTTP 401"},
		{403, claudeStatusAccessDenied, "HTTP 403"},
		{500, claudeStatusRequestFailed, "request failed"},
	} {
		status = tc.code
		time.Sleep(time.Millisecond)
		store.refresh(context.Background())
		c := claudeCard(t, store)
		if c.Claude.Status != tc.status || !strings.Contains(c.Error, tc.text) || !c.Claude.LastAttemptAt.After(good.Claude.LastAttemptAt) ||
			!c.Claude.LastSuccessAt.Equal(good.FetchedAt) || !c.FetchedAt.Equal(good.FetchedAt) || len(c.Gauges) != 1 {
			t.Fatalf("%d: card=%+v provenance=%+v", tc.code, c, c.Claude)
		}
	}

	// A real 429 is an attempt. The cooldown after it sends nothing and says so.
	status = 429
	store.refresh(context.Background())
	limited := claudeCard(t, store)
	if limited.Claude.Status != claudeStatusRateLimited || limited.Claude.NextAttemptAt.IsZero() || !limited.Claude.LastAttemptAt.After(good.Claude.LastAttemptAt) {
		t.Fatalf("429 provenance=%+v", limited.Claude)
	}
	before := calls
	time.Sleep(time.Millisecond)
	store.refresh(context.Background())
	cooling := claudeCard(t, store)
	if calls != before || cooling.Claude.Status != claudeStatusCooldown || !cooling.Claude.LastAttemptAt.Equal(limited.Claude.LastAttemptAt) ||
		!cooling.Claude.NextAttemptAt.Equal(limited.Claude.NextAttemptAt) || !cooling.Claude.LastSuccessAt.Equal(good.FetchedAt) {
		t.Fatalf("cooldown provenance=%+v calls=%d/%d", cooling.Claude, calls, before)
	}

	// Without a session nothing is sent; the last real attempt stays on record.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	store.refresh(context.Background())
	gone := claudeCard(t, store)
	if calls != before || gone.Claude.Status != claudeStatusNoSession || !gone.Claude.LastAttemptAt.Equal(limited.Claude.LastAttemptAt) || !gone.Claude.NextAttemptAt.IsZero() {
		t.Fatalf("no session provenance=%+v", gone.Claude)
	}

	// An unusable 200 counts as an attempt without a success.
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "synthetic-other-session")
	f.Client.Transport = claudeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != claudeUsageURL {
			return nil, errors.New("unused provider")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("synthetic-secret")), Header: make(http.Header)}, nil
	})
	store.refresh(context.Background())
	bad := claudeCard(t, store)
	if bad.Claude.Status != claudeStatusInvalidResponse || !bad.Claude.LastSuccessAt.Equal(good.FetchedAt) || !bad.FetchedAt.Equal(good.FetchedAt) {
		t.Fatalf("invalid provenance=%+v", bad.Claude)
	}
	// Publishing the provenance never carries a credential or provider text.
	wire, err := json.Marshal(bad)
	if err != nil || strings.Contains(string(wire), "synthetic") {
		t.Fatalf("wire=%s err=%v", wire, err)
	}
}

// A peer on the new binary hands over its provider-level result unchanged.
func TestClaudePeerCarriesSourceAttemptMetadata(t *testing.T) {
	attempt := time.Now().Add(-30 * time.Second)
	next := time.Now().Add(4 * time.Minute)
	success := time.Now().Add(-time.Hour)
	card := `{"key":"claude","kind":"quota","gauges":[{"label":"5h","usedPercent":6,"windowSeconds":18000}],"error":"Claude usage rate limited; next attempt in 4m","fetchedAt":"` + peerStamp(success) +
		`","claude":{"sourceMachine":"home","status":"rate_limited","lastAttemptAt":"` + peerStamp(attempt) + `","lastSuccessAt":"` + peerStamp(success) + `","nextAttemptAt":"` + peerStamp(next) + `"}}`
	server := peerServer(t, &peerFixture{machine: "home", card: card})
	f, _ := peerFetcher(t, server.URL+"/usage")
	c := f.claude(context.Background())
	if c.Claude == nil || c.Claude.Status != claudeStatusRateLimited || !c.Claude.LastAttemptAt.Equal(attempt.UTC()) || !c.Claude.NextAttemptAt.Equal(next.UTC()) ||
		!c.Claude.LastSuccessAt.Equal(success.UTC()) || c.Claude.PeerStatus != claudePeerOK || !c.FetchedAt.Equal(success.UTC()) || len(c.Gauges) != 1 {
		t.Fatalf("card=%+v provenance=%+v", c, c.Claude)
	}
}
