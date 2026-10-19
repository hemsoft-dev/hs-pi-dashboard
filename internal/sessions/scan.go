// Package sessions discovers pi session files and distills them into
// lightweight summaries suitable for a fleet dashboard.
//
// Pi persists every conversation as an append-only JSONL tree under
// ~/.pi/agent/sessions/<munged-cwd>/<file>.jsonl. Each line is one entry:
//
//	{"type":"session","version":3,"id":"...","timestamp":"...","cwd":"D:\\repo"}
//	{"type":"model_change","provider":"openai-codex","modelId":"gpt-6-astra",...}
//	{"type":"thinking_level_change","thinkingLevel":"medium",...}
//	{"type":"message","message":{"role":"assistant","usage":{"input":...,"cost":{"total":...}}},...}
//
// The scanner only ever appends to its per-file state: on each poll it reads
// the bytes that appeared since the previous pass, so scanning a directory
// with hundreds of megabytes of history stays cheap.
package sessions

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultActiveWindow is how long after the last observed write a session is
// still considered active.
const DefaultActiveWindow = 2 * time.Minute

// Summary is the dashboard-facing projection of one coding-agent session file.
type Summary struct {
	ActivitySource string `json:"activitySource,omitempty"`
	Source         string `json:"source,omitempty"`
	Status         string `json:"status,omitempty"`
	// T3Status is the source-backed sidebar label, independent of activity.
	// Nil selects legacy presentation; a present empty string means no badge.
	T3Status          *string    `json:"t3Status,omitempty"`
	ProviderSessionID string     `json:"providerSessionId,omitempty"`
	ClaudeSessionIDs  []string   `json:"claudeSessionIds,omitempty"`
	ProviderThreadID  string     `json:"providerThreadId,omitempty"`
	ToolCallCount     int        `json:"toolCallCount,omitempty"`
	TurnStartedAt     *time.Time `json:"turnStartedAt,omitempty"`
	TurnCompletedAt   *time.Time `json:"turnCompletedAt,omitempty"`
	T3Usage           *T3Usage   `json:"t3Usage,omitempty"`
	ID                string     `json:"id"`
	Name              string     `json:"name,omitempty"`
	Cwd               string     `json:"cwd,omitempty"`
	Project           string     `json:"project,omitempty"`
	StartedAt         time.Time  `json:"startedAt"`
	LastActivity      time.Time  `json:"lastActivity"`
	Active            bool       `json:"active"`
	MessageCount      int        `json:"messageCount"`
	Provider          string     `json:"provider,omitempty"`
	Model             string     `json:"model,omitempty"`
	ThinkingLevel     string     `json:"thinkingLevel,omitempty"`
	InputTokens       int64      `json:"inputTokens"`
	OutputTokens      int64      `json:"outputTokens"`
	TotalTokens       int64      `json:"totalTokens"`
	CostUSD           float64    `json:"costUsd"`
	FirstPrompt       string     `json:"firstPrompt,omitempty"`

	// Outputs are the session's most recent visible agent texts, newest
	// first and capped at maxOutputs. Thinking and tool calls never count.
	Outputs []Output `json:"outputs,omitempty"`
}

// T3Usage holds the latest provider-reported counters. Nil means unreported,
// including when a provider reports only context size, not cumulative usage.
type T3Usage struct {
	InputTokens     *int64 `json:"inputTokens"`
	OutputTokens    *int64 `json:"outputTokens"`
	TotalTokens     *int64 `json:"totalTokens"`
	CacheReadTokens *int64 `json:"cacheReadTokens"`
	ReasoningTokens *int64 `json:"reasoningTokens"`
	ContextTokens   *int64 `json:"contextTokens"`
	ContextLimit    *int64 `json:"contextLimit"`
}

// SourceHealth exposes collection failures independently of other sources.
type SourceHealth struct {
	Source        string    `json:"source"`
	Path          string    `json:"path"`
	State         string    `json:"state"`
	Error         string    `json:"error,omitempty"`
	LastSuccessAt time.Time `json:"lastSuccessAt,omitempty"`
}

// Output is one assistant text turn, kept as it was emitted (newlines and
// indentation intact) so the dashboard can reproduce the original shape.
type Output struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

const maxOutputs = 2

// maxReportedSessions caps inactive history in the agent's /sessions payload.
// Active sessions always remain in the payload so every counted session has a
// dashboard row.
const maxReportedSessions = 20

// Snapshot is what the agent endpoint returns for one machine.
type Snapshot struct {
	Sources        []SourceHealth `json:"sources,omitempty"`
	Machine        string         `json:"machine"`
	GeneratedAt    time.Time      `json:"generatedAt"`
	SessionsDir    string         `json:"sessionsDir"` // Pi collector root, including in legacy mixed-source snapshots
	Sessions       []Summary      `json:"sessions"`
	ActiveSessions int            `json:"activeSessions"`
}

type fileState struct {
	size    int64
	summary *Summary
	partial []byte
	claude  *claudeState
}

// Scanner incrementally summarizes JSONL session files below its directory.
type Scanner struct {
	dir          string
	activeWindow time.Duration
	now          func() time.Time
	isClaude     bool

	mu    sync.Mutex
	files map[string]*fileState
}

// NewScanner returns a scanner for dir. dir may be empty, in which case the
// default ~/.pi/agent/sessions is used.
func NewScanner(dir string, activeWindow time.Duration) *Scanner {
	if strings.TrimSpace(dir) == "" {
		dir = defaultSessionsDir()
	}
	if activeWindow <= 0 {
		activeWindow = DefaultActiveWindow
	}
	return &Scanner{
		dir:          dir,
		activeWindow: activeWindow,
		now:          time.Now,
		files:        map[string]*fileState{},
	}
}

// Poll rescans the directory and returns the current snapshot.
func (s *Scanner) Poll() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := s.now()
	seen := map[string]bool{}
	var summaries []Summary

	_ = filepath.WalkDir(s.dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && s.isClaude && d.IsDir() && d.Name() == "subagents" {
			return filepath.SkipDir
		}
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return nil //nolint:nilerr // unreadable entries are simply skipped
		}
		seen[path] = true
		summary := s.pollFile(path, now)
		// Transcript artifacts can also use .jsonl. Both parsers require
		// a real session id before a file reaches the dashboard.
		if summary.ID != "" {
			summaries = append(summaries, summary)
		}
		return nil
	})

	// Forget files that disappeared so state does not grow forever.
	for path := range s.files {
		if !seen[path] {
			delete(s.files, path)
		}
	}

	return summarize(s.dir, now, summaries)
}

// MergeSnapshots combines agent sources under one shared inactive-history cap.
// IDs from non-Pi sources are namespaced by their parser to avoid collisions.
func MergeSnapshots(snapshots ...Snapshot) Snapshot {
	var summaries []Summary
	var sources []SourceHealth
	claudeIDs := map[string]bool{}
	nativeClaude := map[string]Summary{}
	var dir string
	var generatedAt time.Time
	for _, snap := range snapshots {
		if dir == "" {
			dir = snap.SessionsDir
		}
		if snap.GeneratedAt.After(generatedAt) {
			generatedAt = snap.GeneratedAt
		}
		summaries = append(summaries, snap.Sessions...)
		sources = append(sources, snap.Sources...)
		for _, row := range snap.Sessions {
			if row.Provider == "claude-code" {
				nativeClaude[row.ID] = row
			}
			if row.Source == "t3" {
				for _, id := range row.claudeSessionIDs() {
					if id != "" {
						claudeIDs["claude-code:"+id] = true
					}
				}
			}
		}
	}
	unique := summaries[:0]
	seenT3 := map[string]bool{}
	for _, row := range summaries {
		if row.Provider == "claude-code" && claudeIDs[row.ID] {
			continue
		}
		if row.Source == "t3" {
			var latestNative Summary
			foundNative := false
			for _, id := range row.claudeSessionIDs() {
				if id == "" {
					continue
				}
				native, ok := nativeClaude["claude-code:"+id]
				if !ok {
					continue
				}
				// A provider switch can leave several linked Claude sessions.
				// Keep all their activity on one app row, with the newest output.
				if !foundNative || native.LastActivity.After(latestNative.LastActivity) {
					latestNative, foundNative = native, true
				}
				if native.Active && !row.Active {
					row.Active = true
					row.ActivitySource = "claude-code"
					row.TurnStartedAt, row.TurnCompletedAt = nil, nil
				}
				if native.LastActivity.After(row.LastActivity) {
					row.LastActivity = native.LastActivity
				}
				if native.MessageCount > row.MessageCount {
					row.MessageCount = native.MessageCount
				}
			}
			if foundNative {
				row.Outputs = latestNative.Outputs
			}
			if seenT3[row.ID] {
				continue
			}
			seenT3[row.ID] = true
		}
		unique = append(unique, row)
	}
	out := summarize(dir, generatedAt, unique)
	out.Sources = sources
	return out
}

// Current provider fields retain V1 compatibility; V2 also supplies historical
// Claude identities even when the active provider has changed to another driver.
func (s Summary) claudeSessionIDs() []string {
	ids := append([]string(nil), s.ClaudeSessionIDs...)
	if s.Provider == "claudeAgent" {
		ids = append(ids, s.ProviderSessionID, s.ProviderThreadID)
	}
	return ids
}

func summarize(dir string, now time.Time, summaries []Summary) Snapshot {
	sort.Slice(summaries, func(i, j int) bool {
		if summaries[i].Active != summaries[j].Active {
			return summaries[i].Active
		}
		return summaries[i].LastActivity.After(summaries[j].LastActivity)
	})

	activeSessions := 0
	for _, summary := range summaries {
		if summary.Active {
			activeSessions++
		}
	}
	// Keep every T3 thread, including quiet sessions without assistant output.
	// The existing history cap applies only to inactive transcript rows.
	reported := summaries[:0]
	inactive := 0
	for _, row := range summaries {
		if row.Active || row.Source == "t3" || inactive < maxReportedSessions {
			reported = append(reported, row)
		}
		if !row.Active && row.Source != "t3" {
			inactive++
		}
	}

	return Snapshot{
		GeneratedAt:    now,
		SessionsDir:    dir,
		Sessions:       reported,
		ActiveSessions: activeSessions,
	}
}

func (s *Scanner) pollFile(path string, now time.Time) Summary {
	info, err := os.Stat(path)
	if err != nil {
		return Summary{}
	}

	state, ok := s.files[path]
	if !ok || info.Size() < state.size {
		state = &fileState{summary: &Summary{}}
		ok = false
	}

	if ok && state.size == info.Size() {
		// Nothing appended; only refresh the activity window.
		state.summary.Active = now.Sub(state.summary.LastActivity) <= s.activeWindow
		return *state.summary
	}

	f, err := os.Open(path)
	if err != nil {
		return *state.summary
	}
	defer f.Close()

	if _, err := f.Seek(state.size, io.SeekStart); err != nil {
		return *state.summary
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return *state.summary
	}

	state.size = info.Size()
	lines := append(state.partial, data...)
	state.partial = nil
	for len(lines) > 0 {
		idx := bytes.IndexByte(lines, '\n')
		if idx < 0 {
			state.partial = append(state.partial, append([]byte(nil), lines...)...)
			// Cap the partial buffer so a pathological single line cannot
			// grow without bound.
			if len(state.partial) > maxPartialLine {
				state.partial = nil
			}
			break
		}
		line := bytes.TrimSpace(lines[:idx])
		lines = lines[idx+1:]
		if len(line) > 0 {
			if s.isClaude {
				applyClaudeEntry(state, line)
			} else {
				applyEntry(state.summary, line)
			}
		}
	}

	if state.summary.StartedAt.IsZero() {
		state.summary.StartedAt = info.ModTime()
	}
	if state.summary.LastActivity.IsZero() {
		// No parseable entry timestamps; fall back to the file mtime.
		state.summary.LastActivity = info.ModTime()
	}
	state.summary.Active = now.Sub(state.summary.LastActivity) <= s.activeWindow
	s.files[path] = state
	return *state.summary
}

// maxPartialLine bounds the carry-over buffer for a line that has not been
// fully read yet. Pi entries are single-line JSON far below this; entries
// beyond it are treated as malformed and skipped.
const maxPartialLine = 8 << 20 // 8 MiB

// applyEntry folds one JSONL entry into the running summary. Unknown entry
// types and malformed lines are ignored: parsing must never fail a scan.
func applyEntry(s *Summary, line []byte) {
	var head struct {
		Type      string          `json:"type"`
		ID        json.RawMessage `json:"id"`
		Name      string          `json:"name"`
		Cwd       string          `json:"cwd"`
		Timestamp json.RawMessage `json:"timestamp"`
	}
	if err := json.Unmarshal(line, &head); err != nil {
		return
	}
	entryTS, entryHasTS := parseTimestamp(head.Timestamp)
	if entryHasTS && entryTS.After(s.LastActivity) {
		s.LastActivity = entryTS
	}

	switch head.Type {
	case "session":
		s.Source = "pi"
		s.ID = rawString(head.ID)
		s.Name = head.Name
		s.Cwd = head.Cwd
		s.Project = projectName(head.Cwd)
		if ts, ok := parseTimestamp(head.Timestamp); ok {
			s.StartedAt = ts
		}
	case "model_change":
		var mc struct {
			Provider string `json:"provider"`
			ModelID  string `json:"modelId"`
		}
		if json.Unmarshal(line, &mc) == nil {
			s.Provider = mc.Provider
			s.Model = mc.ModelID
		}
	case "thinking_level_change":
		var tc struct {
			ThinkingLevel string `json:"thinkingLevel"`
		}
		if json.Unmarshal(line, &tc) == nil {
			s.ThinkingLevel = tc.ThinkingLevel
		}
	case "message":
		s.MessageCount++
		var me struct {
			Message struct {
				Role     string          `json:"role"`
				Content  json.RawMessage `json:"content"`
				Provider string          `json:"provider"`
				Model    string          `json:"model"`
				Usage    *struct {
					Input       int64 `json:"input"`
					Output      int64 `json:"output"`
					TotalTokens int64 `json:"totalTokens"`
					Cost        *struct {
						Total float64 `json:"total"`
					} `json:"cost"`
				} `json:"usage"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &me); err != nil {
			return
		}
		msg := me.Message
		if msg.Provider != "" {
			s.Provider = msg.Provider
		}
		if msg.Model != "" {
			s.Model = msg.Model
		}
		if msg.Usage != nil {
			s.InputTokens += msg.Usage.Input
			s.OutputTokens += msg.Usage.Output
			s.TotalTokens += msg.Usage.TotalTokens
			if msg.Usage.Cost != nil {
				s.CostUSD += msg.Usage.Cost.Total
			}
		}
		if s.FirstPrompt == "" && msg.Role == "user" {
			s.FirstPrompt = firstText(msg.Content)
		}
		// The dashboard surfaces the agent's recent visible text as extra
		// rows under the session; thinking and toolCall blocks are not
		// output, so only text blocks count.
		if msg.Role == "assistant" && entryHasTS {
			if text := capRaw(allText(msg.Content)); text != "" {
				s.Outputs = append([]Output{{Text: text, At: entryTS}}, s.Outputs...)
				if len(s.Outputs) > maxOutputs {
					s.Outputs = s.Outputs[:maxOutputs]
				}
			}
		}
	}
}

// rawString coerces a JSON id (string or number) into its string form so
// synthetic or odd ids never break entry parsing.
func rawString(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return ""
	}
	if trimmed[0] == '"' {
		var s string
		if json.Unmarshal(trimmed, &s) == nil {
			return s
		}
		return ""
	}
	var num float64
	if json.Unmarshal(trimmed, &num) == nil {
		return fmt.Sprintf("%g", num)
	}
	return ""
}

// allText joins every text block of a message in order with blank lines,
// preserving the newlines the agent actually emitted; thinking and toolCall
// blocks are skipped.
func allText(content json.RawMessage) string {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var text string
		if json.Unmarshal(trimmed, &text) == nil {
			return text
		}
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(trimmed, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

// maxOutputChars bounds each stored output; the dashboard renders them with
// original formatting, so length is capped but newlines are kept.
const maxOutputChars = 800

// capRaw trims trailing whitespace and clamps length without flattening the
// newlines, unlike condense.
func capRaw(text string) string {
	text = strings.TrimRight(text, " \t\r\n")
	runes := []rune(text)
	if len(runes) > maxOutputChars {
		return string(runes[:maxOutputChars]) + "…"
	}
	return text
}

func firstText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) > 0 && trimmed[0] == '"' {
		var text string
		if json.Unmarshal(trimmed, &text) == nil {
			return condense(text)
		}
		return ""
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(trimmed, &blocks) != nil {
		return ""
	}
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			return condense(b.Text)
		}
	}
	return ""
}

func condense(text string) string {
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 160 {
		text = strings.TrimSpace(text[:160]) + "…"
	}
	return text
}

// projectName labels a session from its cwd. A session launched from the
// home directory itself would otherwise take the account name as its project
// (a Mac account named "home" produces sessions titled "home", which reads
// like another machine on the dashboard); "~" marks them plainly.
func projectName(cwd string) string {
	if home, err := os.UserHomeDir(); err == nil && cwd != "" &&
		filepath.Clean(cwd) == filepath.Clean(home) {
		return "~"
	}
	parts := strings.FieldsFunc(cwd, func(r rune) bool { return r == '/' || r == '\\' })
	if len(parts) == 0 {
		return filepath.Base(cwd)
	}
	return parts[len(parts)-1]
}

// parseTimestamp accepts the shapes pi writes: RFC3339 strings and
// millisecond epoch numbers.
func parseTimestamp(raw json.RawMessage) (time.Time, bool) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return time.Time{}, false
	}
	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return time.Time{}, false
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if ts, err := time.Parse(layout, s); err == nil {
				return ts, true
			}
		}
		return time.Time{}, false
	}
	var ms int64
	if err := json.Unmarshal(trimmed, &ms); err != nil || ms <= 0 {
		return time.Time{}, false
	}
	return time.UnixMilli(ms), true
}

func defaultSessionsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".pi/agent/sessions"
	}
	return filepath.Join(home, ".pi", "agent", "sessions")
}
