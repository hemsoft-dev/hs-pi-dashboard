package usage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// peerFixture serves an agent snapshot the way a real agent's GET /usage does.
type peerFixture struct {
	mu      sync.Mutex
	machine string
	card    string
	status  int
	hits    atomic.Int64
	headers atomic.Value
}

func (p *peerFixture) set(status int, card string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status, p.card = status, card
}

func (p *peerFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hits.Add(1)
	p.headers.Store(r.Header.Clone())
	if r.URL.Path != "/usage" || r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	if p.status != 0 {
		w.WriteHeader(p.status)
		return
	}
	fmt.Fprintf(w, `{"machine":%q,"generatedAt":%q,"cards":[{"key":"codex","kind":"quota"},%s]}`, p.machine, time.Now().Format(time.RFC3339Nano), p.card)
}

func peerStamp(t time.Time) string { return t.UTC().Format(time.RFC3339Nano) }

func legacyPeerCard(fetched time.Time, used int) string {
	reset := time.Now().Add(time.Hour)
	return fmt.Sprintf(`{"subscriptionPriceUsd":200,"key":"claude","label":"Claude","kind":"quota","gauges":[{"label":"5h","usedPercent":%d,"resetAt":%q,"windowSeconds":18000,"projectedEnd":7.5},{"label":"Weekly","usedPercent":14,"resetAt":%q,"windowSeconds":604800}],"fetchedAt":%q}`,
		used, peerStamp(reset), peerStamp(reset.Add(72*time.Hour)), peerStamp(fetched))
}

func noCLI(context.Context) ([]byte, error) { return nil, errors.New("not installed") }

// A Fetcher whose Claude credentials and provider endpoint must never be used.
func peerFetcher(t *testing.T, url string) (*Fetcher, *atomic.Int64) {
	t.Helper()
	home := claudeHome(t)
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "synthetic-local-secret")
	writeClaudeJSON(t, home+"/.claude/.credentials.json", `{"claudeAiOauth":{"accessToken":"synthetic-file-secret"}}`)
	writeClaudeJSON(t, home+"/.pi/agent/auth.json", `{"anthropic":{"access":"synthetic-pi-secret"}}`)
	var provider atomic.Int64
	return &Fetcher{Home: home, ClaudePeer: &ClaudePeer{Machine: "home", URL: url},
		museRun: noCLI, agyRun: noCLI,
		Client: &http.Client{Transport: claudeTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.Host == "api.anthropic.com" {
				provider.Add(1)
			}
			return nil, errors.New("no provider in this test")
		})}}, &provider
}

func claudeCard(t *testing.T, s *Store) Card {
	t.Helper()
	for _, c := range s.Snapshot().Cards {
		if c.Key == "claude" {
			return c
		}
	}
	t.Fatal("Claude missing from snapshot")
	return Card{}
}

func TestClaudePeerReplacesLocalWithoutTouchingCredentials(t *testing.T) {
	fetched := time.Now().Add(-20 * time.Second)
	fixture := &peerFixture{machine: "home", card: legacyPeerCard(fetched, 5)}
	server := httptest.NewServer(fixture)
	defer server.Close()
	f, provider := peerFetcher(t, server.URL+"/usage")
	c := f.claude(context.Background())
	if c.Error != "" || len(c.Gauges) != 2 || *c.Gauges[0].UsedPercent != 5 || c.Gauges[0].WindowSeconds != 18000 || c.Gauges[1].Label != "Weekly" {
		t.Fatalf("card=%+v", c)
	}
	if !c.FetchedAt.Equal(fetched.UTC().Truncate(time.Nanosecond)) {
		t.Fatalf("reading age rewritten: got %v want %v", c.FetchedAt, fetched)
	}
	if c.Claude == nil || c.Claude.SourceMachine != "home" || c.Claude.PeerStatus != claudePeerOK || !c.Claude.LastSuccessAt.Equal(c.FetchedAt) {
		t.Fatalf("provenance=%+v", c.Claude)
	}
	if provider.Load() != 0 {
		t.Fatalf("provider called %d times", provider.Load())
	}
	headers := fixture.headers.Load().(http.Header)
	for _, name := range []string{"Authorization", "Cookie", "Anthropic-Beta"} {
		if headers.Get(name) != "" {
			t.Fatalf("peer received %s", name)
		}
	}
	wire, _ := json.Marshal(c)
	if strings.Contains(string(wire), "synthetic") {
		t.Fatalf("credential in card: %s", wire)
	}
}

func TestClaudePeerRefusesUnsafeOrUntruthfulReadings(t *testing.T) {
	now := time.Now()
	good := peerStamp(now.Add(-time.Minute))
	reset := peerStamp(now.Add(time.Hour))
	gauge := fmt.Sprintf(`{"label":"5h","usedPercent":5,"resetAt":%q,"windowSeconds":18000}`, reset)
	card := func(body string) string { return `{"key":"claude","kind":"quota",` + body + `}` }
	for _, tc := range []struct {
		name, machine, card, status string
	}{
		{"wrong machine", "laptop", card(`"gauges":[` + gauge + `],"fetchedAt":"` + good + `"`), claudePeerIdentity},
		{"no claude card", "home", `{"key":"muse-code","kind":"quota"}`, claudePeerNoCard},
		{"duplicate claude card", "home", card(`"gauges":[`+gauge+`],"fetchedAt":"`+good+`"`) + "," + card(`"gauges":[`+gauge+`],"fetchedAt":"`+good+`"`), claudePeerInvalid},
		{"missing time", "home", card(`"gauges":[` + gauge + `]`), claudePeerInvalid},
		{"invalid time", "home", card(`"gauges":[` + gauge + `],"fetchedAt":"yesterday"`), claudePeerInvalid},
		{"future time", "home", card(`"gauges":[` + gauge + `],"fetchedAt":"` + peerStamp(now.Add(10*time.Minute)) + `"`), claudePeerInvalid},
		{"future attempt", "home", card(`"gauges":[` + gauge + `],"fetchedAt":"` + good + `","claude":{"lastAttemptAt":"` + peerStamp(now.Add(10*time.Minute)) + `"}`), claudePeerInvalid},
		{"other source", "home", card(`"gauges":[` + gauge + `],"fetchedAt":"` + good + `","claude":{"sourceMachine":"air"}`), claudePeerIdentity},
		{"unknown status", "home", card(`"gauges":[` + gauge + `],"fetchedAt":"` + good + `","claude":{"status":"Bearer abc"}`), claudePeerInvalid},
		{"chained import", "home", card(`"gauges":[` + gauge + `],"fetchedAt":"` + good + `","claude":{"sourceMachine":"home","peerStatus":"ok"}`), claudePeerInvalid},
		{"percent above range", "home", card(`"gauges":[{"label":"5h","usedPercent":101}],"fetchedAt":"` + good + `"`), claudePeerInvalid},
		{"negative percent", "home", card(`"gauges":[{"label":"5h","usedPercent":-1}],"fetchedAt":"` + good + `"`), claudePeerInvalid},
		{"string percent", "home", card(`"gauges":[{"label":"5h","usedPercent":"5"}],"fetchedAt":"` + good + `"`), claudePeerInvalid},
		{"invalid reset", "home", card(`"gauges":[{"label":"5h","usedPercent":5,"resetAt":"soon"}],"fetchedAt":"` + good + `"`), claudePeerInvalid},
		{"empty label", "home", card(`"gauges":[{"label":" ","usedPercent":5}],"fetchedAt":"` + good + `"`), claudePeerInvalid},
		{"control label", "home", card(`"gauges":[{"label":"a\nb","usedPercent":5}],"fetchedAt":"` + good + `"`), claudePeerInvalid},
		{"negative window", "home", card(`"gauges":[{"label":"5h","usedPercent":5,"windowSeconds":-1}],"fetchedAt":"` + good + `"`), claudePeerInvalid},
		{"nothing reported", "home", card(`"fetchedAt":"` + good + `"`), claudePeerInvalid},
		{"free-form error", "home", card(`"error":"upstream said Bearer synthetic-leak","fetchedAt":"` + good + `"`), claudePeerInvalid},
		{"wrong kind", "home", `{"key":"claude","kind":"balance","gauges":[` + gauge + `],"fetchedAt":"` + good + `"}`, claudePeerInvalid},
		{"not an object", "home", `"claude"`, claudePeerNoCard},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(&peerFixture{machine: tc.machine, card: tc.card})
			defer server.Close()
			f, provider := peerFetcher(t, server.URL+"/usage")
			c := f.claude(context.Background())
			if len(c.Gauges) != 0 || c.Error == "" || c.Claude == nil || c.Claude.PeerStatus != tc.status || provider.Load() != 0 {
				t.Fatalf("card=%+v provenance=%+v provider=%d", c, c.Claude, provider.Load())
			}
			if !c.Claude.LastSuccessAt.IsZero() || !c.Claude.PeerLastSuccessAt.IsZero() || c.Claude.PeerLastAttemptAt.IsZero() {
				t.Fatalf("failed read recorded as success: %+v", c.Claude)
			}
			if strings.Contains(c.Error, "synthetic") || strings.Contains(c.Error, "Bearer") {
				t.Fatalf("peer text leaked: %q", c.Error)
			}
		})
	}
}

func TestClaudePeerTransportFailuresAreClassifiedAndBounded(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500} {
		fixture := &peerFixture{machine: "home", status: status}
		server := httptest.NewServer(fixture)
		f, provider := peerFetcher(t, server.URL+"/usage")
		c := f.claude(context.Background())
		server.Close()
		if c.Claude == nil || c.Claude.PeerStatus != claudePeerHTTP || c.Error == "" || len(c.Gauges) != 0 || fixture.hits.Load() != 1 || provider.Load() != 0 {
			t.Fatalf("status %d: card=%+v hits=%d", status, c, fixture.hits.Load())
		}
	}
	closed := httptest.NewServer(http.NotFoundHandler())
	url := closed.URL + "/usage"
	closed.Close()
	f, _ := peerFetcher(t, url)
	if c := f.claude(context.Background()); c.Claude == nil || c.Claude.PeerStatus != claudePeerUnreachable || c.Error == "" || strings.Contains(c.Error, url) {
		t.Fatalf("unreachable=%+v", c)
	}
	// A redirect could send the agent anywhere: it is reported, never followed.
	var followed atomic.Int64
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { followed.Add(1) }))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/usage", http.StatusFound)
	}))
	defer redirect.Close()
	f, _ = peerFetcher(t, redirect.URL+"/usage")
	if c := f.claude(context.Background()); c.Claude == nil || c.Claude.PeerStatus != claudePeerHTTP || followed.Load() != 0 {
		t.Fatalf("redirect=%+v followed=%d", c, followed.Load())
	}
	transport, ok := claudePeerClient.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil || claudePeerClient.CheckRedirect == nil {
		t.Fatalf("peer client may use a proxy or follow redirects: %+v", claudePeerClient)
	}
	// A body over the bound is refused, not truncated into a reading.
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"machine":"home","pad":"%s"}`, strings.Repeat("x", claudePeerMaxBody))
	}))
	defer huge.Close()
	f, _ = peerFetcher(t, huge.URL+"/usage")
	if c := f.claude(context.Background()); c.Claude == nil || c.Claude.PeerStatus != claudePeerInvalid {
		t.Fatalf("oversized=%+v", c)
	}
}

func TestClaudePeerRetentionKeepsAgeAttemptAndRecovery(t *testing.T) {
	first := time.Now().Add(-90 * time.Second)
	fixture := &peerFixture{machine: "home", card: legacyPeerCard(first, 5)}
	server := httptest.NewServer(fixture)
	defer server.Close()
	f, _ := peerFetcher(t, server.URL+"/usage")
	store := NewStore(f, "mini", time.Minute)
	store.refresh(context.Background())
	initial := claudeCard(t, store)
	if initial.Error != "" || initial.Claude.SourceMachine != "home" || !initial.Claude.PeerLastSuccessAt.Equal(initial.Claude.PeerLastAttemptAt) {
		t.Fatalf("initial=%+v %+v", initial, initial.Claude)
	}

	// Peer outage: the reading keeps its age, the attempt moves, success does not.
	fixture.set(http.StatusBadGateway, "")
	store.refresh(context.Background())
	failed := claudeCard(t, store)
	if failed.Error == "" || len(failed.Gauges) != 2 || !failed.FetchedAt.Equal(initial.FetchedAt) ||
		failed.Claude.PeerStatus != claudePeerHTTP || !failed.Claude.PeerLastAttemptAt.After(initial.Claude.PeerLastAttemptAt) ||
		!failed.Claude.PeerLastSuccessAt.Equal(initial.Claude.PeerLastSuccessAt) || !failed.Claude.LastSuccessAt.Equal(initial.Claude.LastSuccessAt) ||
		failed.Claude.SourceMachine != "home" {
		t.Fatalf("outage=%+v %+v", failed, failed.Claude)
	}

	// The peer's own retained failure passes through with its original age.
	fixture.set(0, fmt.Sprintf(`{"key":"claude","kind":"quota","gauges":[{"label":"5h","usedPercent":6,"windowSeconds":18000}],"error":"Claude usage rate limited; next attempt in 7m","fetchedAt":%q}`, peerStamp(first.Add(time.Second))))
	store.refresh(context.Background())
	limited := claudeCard(t, store)
	if limited.Error != "home: Claude usage rate limited; next attempt in 7m" || *limited.Gauges[0].UsedPercent != 6 || !limited.FetchedAt.Equal(first.Add(time.Second).UTC()) || limited.Claude.PeerStatus != claudePeerOK {
		t.Fatalf("peer cooldown=%+v %+v", limited, limited.Claude)
	}

	// An older reading never replaces a newer one, whatever the peer serves.
	fixture.set(0, legacyPeerCard(first.Add(-time.Hour), 1))
	store.refresh(context.Background())
	regressed := claudeCard(t, store)
	if *regressed.Gauges[0].UsedPercent != 6 || !regressed.FetchedAt.Equal(limited.FetchedAt) || regressed.Claude.PeerStatus != claudePeerOutOfOrder || regressed.Error == "" {
		t.Fatalf("regression accepted: %+v %+v", regressed, regressed.Claude)
	}

	// Recovery comes only from a newer observed reading.
	recovered := time.Now().Add(-time.Second)
	fixture.set(0, legacyPeerCard(recovered, 8))
	store.refresh(context.Background())
	good := claudeCard(t, store)
	if good.Error != "" || *good.Gauges[0].UsedPercent != 8 || !good.FetchedAt.Equal(recovered.UTC()) || good.Claude.PeerStatus != claudePeerOK || !good.Claude.LastSuccessAt.Equal(good.FetchedAt) {
		t.Fatalf("recovery=%+v %+v", good, good.Claude)
	}
}

func TestClaudePeerColdStartStaysUnavailableUntilRead(t *testing.T) {
	fixture := &peerFixture{machine: "home", status: http.StatusServiceUnavailable}
	server := httptest.NewServer(fixture)
	defer server.Close()
	f, provider := peerFetcher(t, server.URL+"/usage")
	store := NewStore(f, "mini", time.Minute)
	store.refresh(context.Background())
	cold := claudeCard(t, store)
	if len(cold.Gauges) != 0 || cold.Error == "" || provider.Load() != 0 || !cold.Claude.LastSuccessAt.IsZero() {
		t.Fatalf("cold=%+v %+v", cold, cold.Claude)
	}
	fixture.set(0, legacyPeerCard(time.Now(), 5))
	store.refresh(context.Background())
	if warm := claudeCard(t, store); warm.Error != "" || len(warm.Gauges) != 2 {
		t.Fatalf("warm=%+v", warm)
	}
}

// Other providers fetch exactly as before, and only the Claude card is imported.
func TestClaudePeerLeavesOtherProviderCardsLocal(t *testing.T) {
	fixture := &peerFixture{machine: "home", card: legacyPeerCard(time.Now(), 5)}
	server := httptest.NewServer(fixture)
	defer server.Close()
	f, _ := peerFetcher(t, server.URL+"/usage")
	t.Setenv("MOONSHOT_API_KEY", "fixture")
	f.Client = &http.Client{Transport: claudeTransport(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != moonshotBalance {
			return nil, errors.New("unused provider")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"data":{"available_balance":12.5}}`)), Header: make(http.Header)}, nil
	})}
	var moonshot, claude *Card
	cards := f.FetchAll(context.Background())
	for i := range cards {
		switch cards[i].Key {
		case "moonshot":
			moonshot = &cards[i]
		case "claude":
			claude = &cards[i]
		}
		if cards[i].Key != "claude" && cards[i].Claude != nil {
			t.Fatalf("provenance on %s", cards[i].Key)
		}
	}
	if moonshot == nil || moonshot.BalanceUSD == nil || *moonshot.BalanceUSD != 12.5 {
		t.Fatalf("moonshot=%+v", moonshot)
	}
	if claude == nil || claude.Claude == nil || claude.Claude.SourceMachine != "home" || claude.SubscriptionPriceUSD == nil {
		t.Fatalf("claude=%+v", claude)
	}
	if fixture.hits.Load() != 1 {
		t.Fatalf("peer read %d times", fixture.hits.Load())
	}
}

func TestParseClaudePeer(t *testing.T) {
	got, err := ParseClaudePeer("home=http://100.101.122.39:8787")
	if err != nil || got.Machine != "home" || got.URL != "http://100.101.122.39:8787/usage" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if got, err := ParseClaudePeer("local-1=http://127.0.0.1:8787/"); err != nil || got.URL != "http://127.0.0.1:8787/usage" {
		t.Fatalf("loopback=%+v err=%v", got, err)
	}
	for _, bad := range []string{
		"", "home", "=http://100.101.122.39:8787", "Home=http://100.101.122.39:8787", "home=",
		"home=http://203.0.113.5:8787",             // public address
		"home=http://example.com:8787",             // DNS can move a name
		"home=https://100.101.122.39:8787",         // agents serve plain HTTP on the tailnet
		"home=ftp://100.101.122.39:8787",           // not HTTP
		"home=http://100.101.122.39",               // no port
		"home=http://user:pw@100.101.122.39:8787",  // credentials
		"home=http://100.101.122.39:8787/other",    // another path
		"home=http://100.101.122.39:8787/usage?x=", // query
		"home=http://100.101.122.39:8787#frag",     // fragment
		"home=http://100.128.0.1:8787",             // outside 100.64.0.0/10
		"home=http://[::ffff:8.8.8.8]:8787",        // mapped public address
		"home=http://0.0.0.0:8787",                 // unspecified
	} {
		if got, err := ParseClaudePeer(bad); err == nil {
			t.Errorf("%q accepted: %+v", bad, got)
		}
	}
}

func peerServer(t *testing.T, fixture *peerFixture) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(fixture)
	t.Cleanup(server.Close)
	return server
}
