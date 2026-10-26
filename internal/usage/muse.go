package usage

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Muse Code allowance comes from `muse usage --json` (1.4.13+), a
// non-inference command. Schema version 1:
//
//	{"schemaVersion":1,"allowance":{"asOfMs":…,"source":"…","tier":"…",
//	 "window":{"resetsAtMs":…,"usedPercent":…},"weekly":{…}}}
//
// allowance may be null (no subscription session) with free-form reason/hint
// strings; those are never surfaced. See testdata/muse/README.md.
const (
	museMinInterval = 5 * time.Minute
	museMaxBackoff  = 30 * time.Minute
	museFreshFor    = 7 * time.Minute // cache interval plus one agent poll and slack
	museResetSlack  = time.Minute
)

var (
	musePlanText   = regexp.MustCompile(`^[A-Za-z0-9 ._()+-]{1,64}$`)
	museSourceText = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	museBucketText = regexp.MustCompile(`^[A-Za-z0-9_-]{1,40}$`)
)

// museWindows are the vendor-documented windows. Undocumented keys still
// become gauges, but without a duration they never get a forecast.
var museWindows = map[string]struct {
	label    string
	duration time.Duration
}{
	"window": {"5h", 5 * time.Hour},
	"weekly": {"Weekly", 7 * 24 * time.Hour},
}

type museCache struct {
	mu       sync.Mutex
	card     *Card
	next     time.Time
	failures int
}

func (f *Fetcher) museCode(ctx context.Context) Card {
	f.muse.mu.Lock()
	defer f.muse.mu.Unlock()
	now := time.Now()
	if f.muse.card != nil && now.Before(f.muse.next) {
		return *f.muse.card
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	run := f.museRun
	if run == nil {
		run = f.runMuseUsage
	}
	card := Card{Key: "muse-code", Label: "Muse Code", Kind: KindQuota, FetchedAt: now, FreshForSeconds: museFreshFor.Seconds()}
	data, err := run(ctx)
	if err == nil {
		err = parseMuseUsage(data, &card, now)
	}
	wait := museMinInterval
	if err != nil {
		card.Gauges, card.Plan, card.ObservedAt, card.AllowanceSource = nil, "", time.Time{}, ""
		card.Error = err.Error()
		// Failures back off so a signed-out or broken CLI is not hammered.
		f.muse.failures++
		for i := 1; i < f.muse.failures && wait < museMaxBackoff; i++ {
			wait *= 2
		}
		wait = min(wait, museMaxBackoff)
	} else {
		f.muse.failures = 0
	}
	f.muse.card, f.muse.next = &card, now.Add(wait)
	return card
}

// parseMuseUsage fills card from the CLI's JSON. A nil error means at least
// one valid window; every other outcome is an error with no gauges.
func parseMuseUsage(data []byte, card *Card, now time.Time) error {
	var root struct {
		SchemaVersion *int            `json:"schemaVersion"`
		Allowance     json.RawMessage `json:"allowance"`
	}
	if json.Unmarshal(data, &root) != nil {
		return errors.New("muse usage output malformed")
	}
	if root.SchemaVersion == nil || *root.SchemaVersion != 1 {
		return errors.New("muse usage schema unsupported")
	}
	if len(root.Allowance) == 0 || string(root.Allowance) == "null" {
		// Unavailable, not zero: the CLI ran but reported no allowance.
		return errors.New("allowance unavailable (CLI reported none)")
	}
	var allowance map[string]json.RawMessage
	if json.Unmarshal(root.Allowance, &allowance) != nil {
		return errors.New("muse usage output malformed")
	}
	var asOfMs float64
	if json.Unmarshal(allowance["asOfMs"], &asOfMs) != nil || !museEpochMs(asOfMs) {
		return errors.New("allowance observation time missing")
	}
	asOf := time.UnixMilli(int64(asOfMs)).UTC()
	keys := make([]string, 0, len(allowance))
	for key := range allowance {
		keys = append(keys, key)
	}
	// Documented windows first, then any others in a stable order.
	sort.Slice(keys, func(i, j int) bool {
		_, a := museWindows[keys[i]]
		_, b := museWindows[keys[j]]
		if a != b {
			return a
		}
		if a && b {
			return museWindows[keys[i]].duration < museWindows[keys[j]].duration
		}
		return keys[i] < keys[j]
	})
	var gauges []Gauge
	for _, key := range keys {
		if gauge, ok := museGauge(key, allowance[key], asOf, now); ok {
			gauges = append(gauges, gauge)
		}
	}
	if len(gauges) == 0 {
		return errors.New("allowance has no valid windows")
	}
	card.Gauges, card.ObservedAt = gauges, asOf
	var tier, source string
	if json.Unmarshal(allowance["tier"], &tier) == nil && musePlanText.MatchString(tier) {
		card.Plan = tier
	}
	if json.Unmarshal(allowance["source"], &source) == nil && museSourceText.MatchString(source) {
		card.AllowanceSource = source
	}
	return nil
}

func museGauge(key string, raw json.RawMessage, asOf, now time.Time) (Gauge, bool) {
	var w struct {
		ResetsAtMs  *float64 `json:"resetsAtMs"`
		UsedPercent *float64 `json:"usedPercent"`
	}
	if !museBucketText.MatchString(key) || len(raw) == 0 || raw[0] != '{' || json.Unmarshal(raw, &w) != nil ||
		w.ResetsAtMs == nil || !museEpochMs(*w.ResetsAtMs) || w.UsedPercent == nil || *w.UsedPercent < 0 {
		return Gauge{}, false
	}
	reset := time.UnixMilli(int64(*w.ResetsAtMs)).UTC()
	// A reset that already passed means the reading belongs to a rolled-over
	// window; its percentage no longer describes the current one.
	if !reset.After(asOf) || !reset.After(now) {
		return Gauge{}, false
	}
	used := clamp(*w.UsedPercent, 0, 100)
	gauge := Gauge{BucketID: key, Label: key, UsedPercent: &used, ResetAt: reset}
	spec, known := museWindows[key]
	if !known {
		return gauge, true
	}
	if reset.Sub(asOf) > spec.duration+museResetSlack {
		// A reset beyond the documented window contradicts the contract.
		return Gauge{}, false
	}
	gauge.Label, gauge.WindowSeconds = spec.label, spec.duration.Seconds()
	// Anchor the pace at the source observation, not at fetch time.
	resetIn := min(reset.Sub(asOf).Seconds(), spec.duration.Seconds())
	if p, ok := projectEndPercent(spec.label, *w.UsedPercent, resetIn, spec.duration.Seconds(), asOf, false); ok {
		gauge.ProjectedEnd = &p
	}
	return gauge, true
}

// museEpochMs accepts epoch milliseconds between 2001 and 2286.
func museEpochMs(v float64) bool { return v >= 1e12 && v < 1e13 }

func (f *Fetcher) runMuseUsage(ctx context.Context) ([]byte, error) {
	bin := envValue("HS_PI_DASHBOARD_MUSE_BIN")
	if bin == "" {
		if path, err := exec.LookPath("muse"); err == nil {
			bin = path
		} else if path := filepath.Join(f.Home, ".local", "bin", "muse"); fileExists(path) {
			bin = path
		} else {
			return nil, errors.New("muse CLI not installed")
		}
	}
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, bin, "usage", "--json")
	cmd.Env = museEnv(os.Environ())
	cmd.Stdin = nil
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	var out usageCommandBuffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		if c.Err() != nil {
			return nil, errors.New("muse usage timed out")
		}
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return nil, errors.New("muse CLI not installed")
		}
		return nil, errors.New("muse usage command failed")
	}
	return out.buffer.Bytes(), nil
}

// museEnv measures the subscription session only: an exported META_API_KEY
// would switch the CLI to pay-as-you-go. A background collector never signs
// in or updates the CLI.
func museEnv(parent []string) []string {
	env := make([]string, 0, len(parent)+2)
	for _, kv := range parent {
		name, _, _ := strings.Cut(kv, "=")
		switch strings.ToUpper(name) {
		case "META_API_KEY", "MUSE_LOGIN", "MUSE_NO_AUTO_UPDATE", "MUSE_SYNC_UPDATE", "MUSE_LAUNCHER_INSTALL":
			continue
		}
		env = append(env, kv)
	}
	return append(env, "MUSE_LOGIN=0", "MUSE_NO_AUTO_UPDATE=1")
}
