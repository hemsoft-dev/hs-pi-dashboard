// Package pmd pushes this machine's T3 snapshots to project-manager's pmd
// (POST /v1/ingest/t3/{machine}), so pmd's assignments stay current without
// ssh scans. It runs beside the dashboard agent and never blocks it.
package pmd

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/t3"
)

const (
	// DefaultURL is pmd on mini.
	DefaultURL = "http://100.97.164.73:8790"
	// Interval is the steady push cadence; MinGap limits pushes on database
	// changes (a busy thread writes the WAL many times a second).
	Interval = 30 * time.Second
	MinGap   = 5 * time.Second
	// holdFor bounds how long a refused snapshot waits before trying again
	// unchanged, so a fix on pmd's side (a new token or machine) heals.
	holdFor  = 15 * time.Minute
	tick     = time.Second
	maxReply = 64 << 10
)

var validEnvironment = regexp.MustCompile(`^[a-z0-9-]{0,32}$`)

// Source is one T3 install: its environment name ("" for a machine's only
// install) and its V2 database.
type Source struct {
	Environment string
	Path        string
}

// ParseSource reads an env=path flag value. The environment may be empty.
func ParseSource(value string) (Source, error) {
	env, path, ok := strings.Cut(value, "=")
	if !ok || strings.TrimSpace(path) == "" {
		return Source{}, fmt.Errorf("want env=path, got %q", value)
	}
	if !validEnvironment.MatchString(env) {
		return Source{}, fmt.Errorf("environment %q must be up to 32 of a-z, 0-9 and -", env)
	}
	return Source{Environment: env, Path: expandHome(path)}, nil
}

// DefaultTokenFile is where the agent looks for its pmd token.
func DefaultTokenFile() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "hs-pi-dashboard", "pmd-token")
}

// Options configures the pusher. Zero durations use the package defaults.
type Options struct {
	URL       string
	Machine   string
	TokenFile string
	Sources   []Source
	Client    *http.Client
	Now       func() time.Time
	Logf      func(format string, args ...any)
	Interval  time.Duration
	MinGap    time.Duration
	Tick      time.Duration
	// Build reads one snapshot; tests replace it.
	Build func(ctx context.Context, src Source, now func() time.Time) (any, error)
}

// Run pushes every source until ctx ends. Each source keeps its own schedule.
func Run(ctx context.Context, opts Options) {
	opts.defaults()
	token := &tokenSource{path: opts.TokenFile, logf: opts.Logf}
	var wg sync.WaitGroup
	for _, src := range opts.Sources {
		p := newPusher(opts, src, token)
		wg.Add(1)
		go func() { defer wg.Done(); p.loop(ctx) }()
	}
	wg.Wait()
}

func (opts *Options) defaults() {
	if opts.Client == nil {
		opts.Client = &http.Client{Timeout: 15 * time.Second}
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logf == nil {
		opts.Logf = log.Printf
	}
	if opts.Interval <= 0 {
		opts.Interval = Interval
	}
	if opts.MinGap <= 0 {
		opts.MinGap = MinGap
	}
	if opts.Tick <= 0 {
		opts.Tick = tick
	}
	if opts.Build == nil {
		opts.Build = func(ctx context.Context, src Source, now func() time.Time) (any, error) {
			return t3.BuildIngestSnapshot(ctx, src.Path, src.Environment, now)
		}
	}
}

func newPusher(opts Options, src Source, token *tokenSource) *pusher {
	return &pusher{opts: opts, src: src, token: token, logged: map[string]bool{}}
}

type pusher struct {
	opts   Options
	src    Source
	token  *tokenSource
	last   time.Time // last attempt
	stamp  fileStamp // database + WAL at the last attempt
	logged map[string]bool
	ok     bool // the last push was accepted
	hold   *hold
}

// hold keeps a refused snapshot from being resent unchanged: a 400 or 413
// waits for different content, any other 4xx for a different token file.
type hold struct {
	until   time.Time
	byToken bool
	token   fileStamp
	content string
}

func (p *pusher) loop(ctx context.Context) {
	t := time.NewTicker(p.opts.Tick)
	defer t.Stop()
	for {
		p.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (p *pusher) step(ctx context.Context) {
	now := p.opts.Now()
	stamp := statDB(p.src.Path)
	since := now.Sub(p.last)
	due := p.last.IsZero() || since >= p.opts.Interval || (stamp != p.stamp && since >= p.opts.MinGap)
	if !due {
		return
	}
	p.last, p.stamp = now, stamp
	token, tokenStamp, ok := p.token.read()
	if !ok {
		return
	}
	if h := p.hold; h != nil && h.byToken {
		if tokenStamp == h.token && now.Before(h.until) {
			return
		}
		p.hold = nil
	}
	buildCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	snap, err := p.opts.Build(buildCtx, p.src, p.opts.Now)
	cancel()
	if err != nil {
		p.fail("read", fmt.Sprintf("T3 snapshot unavailable: %v", err))
		return
	}
	body, err := json.Marshal(snap)
	if err != nil {
		p.fail("encode", fmt.Sprintf("T3 snapshot could not be encoded: %v", err))
		return
	}
	content := contentKey(body)
	if h := p.hold; h != nil {
		if content == h.content && now.Before(h.until) {
			return
		}
		p.hold = nil
	}
	status, code, message, err := p.post(ctx, token, body)
	switch {
	case err != nil:
		p.fail("network", fmt.Sprintf("pmd unreachable, retrying: %v", err))
	case status == http.StatusOK:
		if !p.ok {
			p.opts.Logf("pmd push%s ok: %s", p.label(), message)
		}
		p.ok, p.hold = true, nil
		clear(p.logged)
	case status == http.StatusConflict:
		// The next snapshot heals a stale one.
		p.fail(fmt.Sprint(status), fmt.Sprintf("pmd refused a snapshot: %d %s", status, code))
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge:
		p.fail(fmt.Sprint(status), fmt.Sprintf("pmd refused the snapshot, holding until it changes: %d %s: %s", status, code, message))
		p.hold = &hold{until: now.Add(holdFor), content: content}
	case status >= 400 && status < 500:
		p.fail(fmt.Sprint(status), fmt.Sprintf("pmd refused the push, holding until the token file changes: %d %s: %s", status, code, message))
		p.hold = &hold{until: now.Add(holdFor), byToken: true, token: tokenStamp}
	default:
		p.fail(fmt.Sprint(status), fmt.Sprintf("pmd failed, retrying: %d %s", status, code))
	}
}

// fail logs each distinct failure once until a push is accepted again.
func (p *pusher) fail(kind, message string) {
	p.ok = false
	if p.logged[kind] {
		return
	}
	p.logged[kind] = true
	p.opts.Logf("pmd push%s: %s", p.label(), message)
}

func (p *pusher) label() string {
	if p.src.Environment == "" {
		return ""
	}
	return " (" + p.src.Environment + ")"
}

// post sends one snapshot. The token goes only into the Authorization header;
// replies are summarized as counts or pmd's error code and message.
func (p *pusher) post(ctx context.Context, token string, body []byte) (int, string, string, error) {
	target := strings.TrimRight(p.opts.URL, "/") + "/v1/ingest/t3/" + url.PathEscape(p.opts.Machine)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.opts.Client.Do(req)
	if err != nil {
		var urlErr *url.Error
		if errors.As(err, &urlErr) {
			err = urlErr.Err // the URL holds no secret, but keep the line short
		}
		return 0, "", "", err
	}
	defer resp.Body.Close()
	reply, _ := io.ReadAll(io.LimitReader(resp.Body, maxReply))
	if resp.StatusCode == http.StatusOK {
		var ok struct {
			Threads int `json:"threads"`
			InView  int `json:"inView"`
			Changed int `json:"changed"`
		}
		_ = json.Unmarshal(reply, &ok)
		return resp.StatusCode, "", fmt.Sprintf("%d threads, %d in view", ok.Threads, ok.InView), nil
	}
	var failure struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(reply, &failure)
	code := failure.Error.Code
	if code == "" {
		code = http.StatusText(resp.StatusCode)
	}
	return resp.StatusCode, code, cut(failure.Error.Message, 300), nil
}

// contentKey hashes a snapshot without its generatedAt.
func contentKey(body []byte) string {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return ""
	}
	delete(fields, "generatedAt")
	canonical, _ := json.Marshal(fields)
	sum := sha256.Sum256(canonical)
	return string(sum[:])
}

type fileStamp struct {
	size, walSize   int64
	modTime, walMod time.Time
}

func statDB(path string) fileStamp {
	var s fileStamp
	if info, err := os.Stat(path); err == nil {
		s.size, s.modTime = info.Size(), info.ModTime()
	}
	if info, err := os.Stat(path + "-wal"); err == nil {
		s.walSize, s.walMod = info.Size(), info.ModTime()
	}
	return s
}

// tokenSource reads the token file before every push, so a token added or
// rotated later takes effect without a restart. A missing or unsafe file turns
// the push off with one log line.
type tokenSource struct {
	path  string
	logf  func(string, ...any)
	mu    sync.Mutex
	state string
}

func (t *tokenSource) read() (string, fileStamp, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	info, err := os.Stat(t.path)
	if err != nil {
		t.report("missing", fmt.Sprintf("pmd push off: no token file at %s", t.path))
		return "", fileStamp{}, false
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		t.report("mode", fmt.Sprintf("pmd push off: %s must be mode 600", t.path))
		return "", fileStamp{}, false
	}
	raw, err := os.ReadFile(t.path)
	token := strings.TrimSpace(string(raw))
	if err != nil || token == "" {
		t.report("unreadable", fmt.Sprintf("pmd push off: token file %s is unreadable or empty", t.path))
		return "", fileStamp{}, false
	}
	t.report("on", fmt.Sprintf("pmd push on: token file %s", t.path))
	return token, fileStamp{size: info.Size(), modTime: info.ModTime()}, true
}

func (t *tokenSource) report(state, message string) {
	if t.state != state {
		t.state = state
		t.logf("%s", message)
	}
}

func expandHome(path string) string {
	if strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, _ := os.UserHomeDir()
		path = filepath.Join(home, path[2:])
	}
	return path
}

func cut(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// Sources picks what to push. Explicit -pmd-t3 sources win. Otherwise a
// machine's only T3 database (the default install, or one -t3-db) is pushed as
// environment "". Several databases need names, so none is pushed and the
// reason says why.
func Sources(explicit []Source, t3DBs []string) ([]Source, string) {
	if len(explicit) > 0 {
		return explicit, ""
	}
	if len(t3DBs) == 0 {
		return []Source{{Path: t3.DefaultIngestDB()}}, ""
	}
	var paths []string
	seen := map[string]bool{}
	for _, path := range t3DBs {
		if path != "off" && !seen[path] {
			seen[path] = true
			paths = append(paths, path)
		}
	}
	switch len(paths) {
	case 0:
		return nil, "pmd push off: T3 is off"
	case 1:
		return []Source{{Path: expandHome(paths[0])}}, ""
	}
	return nil, "pmd push off: several T3 databases; name each with -pmd-t3 env=path"
}
