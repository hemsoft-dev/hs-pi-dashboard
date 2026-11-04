package usage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

// Last result of the source host's own provider attempt, published as
// ClaudeProvenance.Status. cooldown and no_session mean no request was sent.
const (
	claudeStatusOK              = "ok"
	claudeStatusNoSession       = "no_session"
	claudeStatusAuthRejected    = "auth_rejected"  // HTTP 401
	claudeStatusAccessDenied    = "access_denied"  // HTTP 403
	claudeStatusRateLimited     = "rate_limited"   // HTTP 429
	claudeStatusCooldown        = "cooldown"       // waiting out an earlier 429
	claudeStatusRequestFailed   = "request_failed" // transport or other HTTP status
	claudeStatusInvalidResponse = "invalid_response"
)

// Last result of reading the peer, published as ClaudeProvenance.PeerStatus.
const (
	claudePeerOK          = "ok"
	claudePeerUnreachable = "unreachable"
	claudePeerHTTP        = "http_error"
	claudePeerInvalid     = "invalid"
	claudePeerIdentity    = "identity_mismatch"
	claudePeerNoCard      = "no_card"
	claudePeerOutOfOrder  = "out_of_order"
)

const (
	claudePeerTimeout = 5 * time.Second
	claudePeerMaxBody = 8 << 20
	// Fleet clocks differ by seconds; a larger lead means the time is wrong.
	claudePeerFutureSkew = 2 * time.Minute
	claudePeerMaxGauges  = 32
	claudePeerMaxWindow  = 366 * 86400.0
)

// ClaudeProvenance says whose session produced a Claude card and how its last
// attempts went. Every value is allowlisted or a time; it never carries a
// provider body, credential or account identity. Card.FetchedAt stays the time
// of the reading, so a retained reading keeps its age.
type ClaudeProvenance struct {
	SourceMachine string    `json:"sourceMachine,omitempty"` // host whose own session produced the reading
	Status        string    `json:"status,omitempty"`        // that host's last provider result; empty for an older peer
	LastAttemptAt time.Time `json:"lastAttemptAt,omitzero"`  // last request actually sent to the provider
	LastSuccessAt time.Time `json:"lastSuccessAt,omitzero"`  // when the card's gauges were observed
	NextAttemptAt time.Time `json:"nextAttemptAt,omitzero"`  // end of a rate-limit cooldown
	// Only on a republished card: this agent's own reads of the peer.
	PeerStatus        string    `json:"peerStatus,omitempty"`
	PeerLastAttemptAt time.Time `json:"peerLastAttemptAt,omitzero"`
	PeerLastSuccessAt time.Time `json:"peerLastSuccessAt,omitzero"`
}

// ClaudePeer is another agent whose own Claude reading this agent republishes.
// No credential is read for it or sent to it: the session stays on its host.
type ClaudePeer struct {
	Machine string // must match the machine label the peer reports
	URL     string // the peer's GET /usage
}

var claudePeerName = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// Tailscale's address range. The peer is named by IP so DNS cannot move it.
var tailnetRange = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

// ParseClaudePeer reads name=http://ip:port, where ip is a loopback or Tailscale
// address. Agents serve plain HTTP on the tailnet and nowhere else.
func ParseClaudePeer(value string) (ClaudePeer, error) {
	name, raw, ok := strings.Cut(value, "=")
	if !ok || !claudePeerName.MatchString(name) {
		return ClaudePeer{}, fmt.Errorf("want name=http://ip:port with a name of up to 32 of a-z, 0-9 and -, got %q", value)
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Port() == "" || (u.Path != "" && u.Path != "/") {
		return ClaudePeer{}, fmt.Errorf("peer %q: want http://ip:port with nothing else", name)
	}
	ip := net.ParseIP(u.Hostname())
	if ip == nil || !(ip.IsLoopback() || tailnetRange.Contains(ip)) {
		return ClaudePeer{}, fmt.Errorf("peer %q: host must be a loopback or Tailscale IP address", name)
	}
	return ClaudePeer{Machine: name, URL: "http://" + u.Host + "/usage"}, nil
}

// The peer client never uses a proxy and never follows a redirect, so a reading
// can only come from the configured address.
var claudePeerClient = &http.Client{
	Timeout:       claudePeerTimeout,
	Transport:     &http.Transport{DisableKeepAlives: true},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// The peer's error text is republished only when it is one of the collector's
// own fixed messages, older and current.
var claudePeerErrors = regexp.MustCompile(`^(` +
	`Claude usage rate limited; (retrying on the next refresh|next attempt in [0-9]+m)` +
	`|` + regexp.QuoteMeta(claudeErrSessionRejected) +
	`|` + regexp.QuoteMeta(claudeErrAccessDenied) +
	`|Claude session expired or lacks usage access; sign in again with Claude Code` +
	`|` + regexp.QuoteMeta(claudeErrNoSession) +
	`|` + regexp.QuoteMeta(claudeErrRequestFailed) +
	`|could not prepare Claude usage request` +
	`|invalid Claude usage response` +
	`|no Claude usage windows reported` +
	`)$`)

var claudeProviderStatuses = map[string]bool{
	"": true, claudeStatusOK: true, claudeStatusNoSession: true, claudeStatusAuthRejected: true, claudeStatusAccessDenied: true,
	claudeStatusRateLimited: true, claudeStatusCooldown: true, claudeStatusRequestFailed: true, claudeStatusInvalidResponse: true,
}

// claudeFromPeer makes one bounded read of the peer's cached card. That read
// sends nothing to the provider, so the peer's own cooldown is never disturbed.
func (f *Fetcher) claudeFromPeer(ctx context.Context) Card {
	now := time.Now()
	peer := f.ClaudePeer
	card := Card{Key: "claude", Label: "Claude", Kind: KindQuota, FetchedAt: now, Claude: &ClaudeProvenance{SourceMachine: peer.Machine, PeerLastAttemptAt: now}}
	fail := func(status, problem string) Card {
		card.Claude.PeerStatus = status
		card.Error = fmt.Sprintf("Claude usage from %s: %s", peer.Machine, problem)
		return card
	}
	ctx, cancel := context.WithTimeout(ctx, claudePeerTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, peer.URL, nil)
	if err != nil {
		return fail(claudePeerUnreachable, "unreachable")
	}
	req.Header.Set("Accept", "application/json")
	// Transport errors name addresses, so only a fixed diagnosis is published.
	resp, err := claudePeerClient.Do(req)
	if err != nil {
		return fail(claudePeerUnreachable, "unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail(claudePeerHTTP, fmt.Sprintf("agent returned HTTP %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, claudePeerMaxBody+1))
	if err != nil || len(body) > claudePeerMaxBody {
		return fail(claudePeerInvalid, "response unreadable")
	}
	reading, status, problem := decodeClaudePeer(body, peer.Machine, now)
	if status != claudePeerOK {
		return fail(status, problem)
	}
	card.Gauges, card.FetchedAt, card.Error = reading.Gauges, reading.FetchedAt, ""
	if reading.Error != "" {
		card.Error = peer.Machine + ": " + reading.Error
	}
	card.Claude = &ClaudeProvenance{SourceMachine: peer.Machine, PeerStatus: claudePeerOK, PeerLastAttemptAt: now, PeerLastSuccessAt: now}
	if p := reading.Claude; p != nil {
		card.Claude.Status, card.Claude.LastAttemptAt, card.Claude.NextAttemptAt = p.Status, p.LastAttemptAt, p.NextAttemptAt
	}
	// Gauges and their time travel together, whatever else the peer reported.
	if len(card.Gauges) > 0 {
		card.Claude.LastSuccessAt = card.FetchedAt
	}
	return card
}

// decodeClaudePeer accepts only the peer's own, fully valid Claude card and
// copies allowlisted fields. Anything else refuses the whole reading: a partial
// import could pass as a current quota.
func decodeClaudePeer(body []byte, machine string, now time.Time) (Card, string, string) {
	var snapshot struct {
		Machine string            `json:"machine"`
		Cards   []json.RawMessage `json:"cards"`
	}
	if json.Unmarshal(body, &snapshot) != nil {
		return Card{}, claudePeerInvalid, "response is not a usage snapshot"
	}
	if snapshot.Machine != machine {
		return Card{}, claudePeerIdentity, "agent reports a different machine"
	}
	var raw json.RawMessage
	for _, entry := range snapshot.Cards {
		var head struct {
			Key string `json:"key"`
		}
		if json.Unmarshal(entry, &head) != nil || head.Key != "claude" {
			continue
		}
		if raw != nil {
			return Card{}, claudePeerInvalid, "agent reports two Claude cards"
		}
		raw = entry
	}
	if raw == nil {
		return Card{}, claudePeerNoCard, "agent reports no Claude card"
	}
	var got struct {
		Kind      string            `json:"kind"`
		Gauges    []Gauge           `json:"gauges"`
		Error     string            `json:"error"`
		FetchedAt time.Time         `json:"fetchedAt"`
		Claude    *ClaudeProvenance `json:"claude"`
	}
	if json.Unmarshal(raw, &got) != nil || got.Kind != KindQuota {
		return Card{}, claudePeerInvalid, "malformed Claude card"
	}
	if got.FetchedAt.IsZero() || got.FetchedAt.After(now.Add(claudePeerFutureSkew)) {
		return Card{}, claudePeerInvalid, "reading time missing or in the future"
	}
	if p := got.Claude; p != nil {
		if p.SourceMachine != "" && p.SourceMachine != machine {
			return Card{}, claudePeerIdentity, "reading belongs to another machine"
		}
		// One hop only: a republished card says nothing about the peer's session.
		if p.PeerStatus != "" || !p.PeerLastAttemptAt.IsZero() || !p.PeerLastSuccessAt.IsZero() {
			return Card{}, claudePeerInvalid, "agent republishes another machine's reading"
		}
		if !claudeProviderStatuses[p.Status] || p.LastAttemptAt.After(now.Add(claudePeerFutureSkew)) || p.LastSuccessAt.After(now.Add(claudePeerFutureSkew)) {
			return Card{}, claudePeerInvalid, "attempt metadata invalid"
		}
	}
	if got.Error != "" && !claudePeerErrors.MatchString(got.Error) {
		return Card{}, claudePeerInvalid, "agent reports an unrecognized error"
	}
	if len(got.Gauges) == 0 && got.Error == "" {
		return Card{}, claudePeerInvalid, "agent reports no windows and no error"
	}
	if len(got.Gauges) > claudePeerMaxGauges {
		return Card{}, claudePeerInvalid, "too many windows"
	}
	gauges := make([]Gauge, 0, len(got.Gauges))
	for _, g := range got.Gauges {
		if !validClaudePeerGauge(g) {
			return Card{}, claudePeerInvalid, "malformed window"
		}
		gauges = append(gauges, Gauge{Label: g.Label, UsedPercent: g.UsedPercent, ResetAt: g.ResetAt, WindowSeconds: g.WindowSeconds, ProjectedEnd: g.ProjectedEnd})
	}
	return Card{Gauges: gauges, Error: got.Error, FetchedAt: got.FetchedAt, Claude: got.Claude}, claudePeerOK, ""
}

func validClaudePeerGauge(g Gauge) bool {
	// Match the provider parser: a label or projection alone is no observation.
	if g.UsedPercent == nil && g.ResetAt.IsZero() {
		return false
	}
	if strings.TrimSpace(g.Label) == "" || len(g.Label) > 128 || strings.ContainsFunc(g.Label, unicode.IsControl) {
		return false
	}
	if g.UsedPercent != nil && (*g.UsedPercent < 0 || *g.UsedPercent > 100) {
		return false
	}
	if g.ProjectedEnd != nil && *g.ProjectedEnd < 0 {
		return false
	}
	return g.WindowSeconds >= 0 && g.WindowSeconds <= claudePeerMaxWindow
}

// mergeClaudeProvenance runs before the shared retain rule. It fills in what the
// current attempt cannot know, without ever advancing a success or an attempt
// that did not happen. Callers hold s.mu; the old card stays immutable.
func mergeClaudeProvenance(card *Card, old Card, retained bool, machine string) {
	p := *card.Claude
	card.Claude = &p
	if p.SourceMachine == "" {
		p.SourceMachine = machine
	}
	if retained && old.Claude != nil {
		prev := old.Claude
		// An older reading from the peer never replaces a newer one.
		if p.PeerStatus == claudePeerOK && len(card.Gauges) > 0 && len(old.Gauges) > 0 && card.FetchedAt.Before(old.FetchedAt) {
			p.PeerStatus, p.PeerLastSuccessAt, p.LastSuccessAt = claudePeerOutOfOrder, time.Time{}, time.Time{}
			card.Gauges = nil
			card.Error = "Claude usage from " + p.SourceMachine + ": reading older than the one retained"
		}
		// A failed peer read says nothing new about the source host's attempts.
		if p.PeerStatus != "" && p.PeerStatus != claudePeerOK {
			p.Status, p.LastAttemptAt, p.NextAttemptAt = prev.Status, prev.LastAttemptAt, prev.NextAttemptAt
		}
		if p.LastAttemptAt.IsZero() {
			p.LastAttemptAt = prev.LastAttemptAt
		}
		if p.LastSuccessAt.IsZero() {
			p.LastSuccessAt = prev.LastSuccessAt
		}
		if p.PeerLastSuccessAt.IsZero() {
			p.PeerLastSuccessAt = prev.PeerLastSuccessAt
		}
	}
}
