package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const claudePrompt = `{"type":"user","sessionId":"shared","uuid":"u1","cwd":"/repo/dashboard","timestamp":"2026-09-30T12:00:00Z","message":{"role":"user","content":"Make it work"}}`

func claudeAssistant(uuid, id, content string, output int) string {
	return fmt.Sprintf(`{"type":"assistant","sessionId":"shared","uuid":%q,"cwd":"/repo/dashboard","timestamp":"2026-09-30T12:01:00Z","effort":"high","message":{"id":%q,"role":"assistant","model":"claude-opus-4-6","content":%s,"usage":{"input_tokens":10,"cache_read_input_tokens":100,"cache_creation_input_tokens":20,"output_tokens":%d}}}`, uuid, id, content, output)
}

func claudeScanner(t *testing.T, lines ...string) (*Scanner, string) {
	t.Helper()
	dir := t.TempDir()
	writeSession(t, dir, "session.jsonl", lines...)
	s := NewClaudeScanner(dir, 2*time.Minute)
	s.now = func() time.Time { return time.Date(2026, 9, 30, 12, 2, 0, 0, time.UTC) }
	return s, filepath.Join(dir, "repo", "session.jsonl")
}

func TestClaudeSummarizesContentBlocksOnce(t *testing.T) {
	text := claudeAssistant("a2", "msg1", `[{"type":"text","text":"Done.\nTests passed."}]`, 30)
	scanner, _ := claudeScanner(t, claudePrompt,
		claudeAssistant("a1", "msg1", `[{"type":"thinking","thinking":"private"}]`, 20),
		text, text,
		claudeAssistant("a3", "msg1", `[{"type":"tool_use","name":"Bash","input":{"command":"private"}}]`, 30),
		claudeAssistant("a4", "msg1", `[{"type":"text","text":"Ready."}]`, 40),
		`{"type":"user","sessionId":"shared","uuid":"u2","timestamp":"2026-09-30T12:01:10Z","message":{"role":"user","content":[{"type":"tool_result","content":"private"}]}}`,
	)
	snap := scanner.Poll()
	if len(snap.Sessions) != 1 || snap.ActiveSessions != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	s := snap.Sessions[0]
	if s.Source != "claude-code" || s.ID != "claude-code:shared" || s.Provider != "claude-code" || s.Project != "dashboard" || s.Model != "claude-opus-4-6" || s.ThinkingLevel != "high" {
		t.Fatalf("identity = %+v", s)
	}
	if s.MessageCount != 3 || s.InputTokens != 130 || s.OutputTokens != 40 || s.TotalTokens != 170 {
		t.Fatalf("message/usage totals = %+v", s)
	}
	if s.FirstPrompt != "Make it work" || s.CostUSD != 0 {
		t.Fatalf("prompt/cost = %+v", s)
	}
	if len(s.Outputs) != 1 || s.Outputs[0].Text != "Done.\nTests passed.\n\nReady." {
		t.Fatalf("outputs = %+v", s.Outputs)
	}
	if got := scanner.Poll().Sessions[0]; got.MessageCount != 3 || got.TotalTokens != 170 {
		t.Fatalf("idle poll double counted = %+v", got)
	}
}

func TestClaudeIncrementalPartialLineAndTruncation(t *testing.T) {
	scanner, path := claudeScanner(t, claudePrompt)
	initial := scanner.Poll()
	line := claudeAssistant("a1", "msg1", `[{"type":"text","text":"Hello"}]`, 12)
	appendData := func(data string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		defer f.Close()
		if _, err := f.WriteString(data); err != nil {
			t.Fatal(err)
		}
	}
	appendData(line[:len(line)/2])
	if got := scanner.Poll().Sessions[0]; got.MessageCount != 1 {
		t.Fatalf("partial line counted = %+v", got)
	}
	appendData(line[len(line)/2:] + "\n")
	first := scanner.Poll()
	appendData(claudeAssistant("a2", "msg1", `[{"type":"text","text":"World"}]`, 15) + "\n")
	second := scanner.Poll()
	if second.Sessions[0].MessageCount != 2 || second.Sessions[0].OutputTokens != 15 || second.Sessions[0].Outputs[0].Text != "Hello\n\nWorld" {
		t.Fatalf("incremental summary = %+v", second.Sessions[0])
	}
	if len(initial.Sessions[0].Outputs) != 0 || first.Sessions[0].Outputs[0].Text != "Hello" {
		t.Fatal("later polls mutated prior snapshots")
	}
	if err := os.WriteFile(path, []byte(claudePrompt+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := scanner.Poll().Sessions[0]; got.MessageCount != 1 || got.TotalTokens != 0 || len(got.Outputs) != 0 {
		t.Fatalf("truncation did not reset state = %+v", got)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if snap := scanner.Poll(); len(snap.Sessions) != 0 {
		t.Fatalf("empty truncated transcript retained = %+v", snap)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if snap := scanner.Poll(); len(snap.Sessions) != 0 || len(scanner.files) != 0 {
		t.Fatalf("deleted transcript retained = %+v", snap)
	}
}

func TestClaudeSkipsArtifactsAndSidechains(t *testing.T) {
	scanner, _ := claudeScanner(t, "not json", claudePrompt,
		strings.Replace(claudeAssistant("side", "msg-side", `[{"type":"text","text":"hidden"}]`, 100), `"type":"assistant"`, `"isSidechain":true,"type":"assistant"`, 1),
		`{"type":"file-history-snapshot","sessionId":"shared","timestamp":"2026-09-30T12:02:00Z"}`,
		`{"type":"assistant","sessionId":"other","uuid":"mixed","timestamp":"2026-09-30T12:02:00Z","message":{"role":"assistant","content":"wrong session"}}`,
		`{"type":"assistant","sessionId":"shared","uuid":"invalid","timestamp":"bad","message":{"role":"assistant","content":"bad time"}}`,
	)
	writeSession(t, scanner.dir, "parent/subagents/agent-child.jsonl", claudePrompt)
	writeSession(t, scanner.dir, "artifact.jsonl", `{"type":"assistant","timestamp":"2026-09-30T12:01:00Z","message":{"role":"assistant","content":"artifact"}}`)
	writeSession(t, scanner.dir, "invalid-role.jsonl", `{"type":"assistant","sessionId":"invalid","uuid":"invalid","timestamp":"2026-09-30T12:01:00Z","message":{"role":"user","content":"artifact"}}`)
	snap := scanner.Poll()
	if len(snap.Sessions) != 1 || snap.Sessions[0].MessageCount != 1 || len(snap.Sessions[0].Outputs) != 0 || snap.Sessions[0].TotalTokens != 0 {
		t.Fatalf("artifact included = %+v", snap)
	}
}

func TestClaudeOutputHistoryAndActivityExpiry(t *testing.T) {
	scanner, _ := claudeScanner(t, claudePrompt,
		claudeAssistant("a1", "msg1", `[{"type":"text","text":"One"}]`, 5),
		claudeAssistant("a2", "msg2", `[{"type":"text","text":"Two"}]`, 6),
		claudeAssistant("a3", "msg3", `[{"type":"text","text":"Three"}]`, 7),
	)
	s := scanner.Poll().Sessions[0]
	if len(s.Outputs) != 2 || s.Outputs[0].Text != "Three" || s.Outputs[1].Text != "Two" || s.TotalTokens != 408 {
		t.Fatalf("history = %+v", s)
	}
	scanner.now = func() time.Time { return time.Date(2026, 9, 30, 12, 4, 0, 0, time.UTC) }
	if snap := scanner.Poll(); snap.ActiveSessions != 0 || snap.Sessions[0].Active {
		t.Fatalf("quiet session stayed active = %+v", snap)
	}
}

func TestClaudeAcceptsObjectEffortAndCapsOutput(t *testing.T) {
	entry := claudeAssistant("a1", "msg1", `[{"type":"text","text":`+jsonString(strings.Repeat("界", 1000))+`}]`, 5)
	entry = strings.Replace(entry, `"effort":"high"`, `"effort":{"level":"max"}`, 1)
	scanner, _ := claudeScanner(t, claudePrompt, entry)
	s := scanner.Poll().Sessions[0]
	if s.ThinkingLevel != "max" || len(s.Outputs) != 1 || len([]rune(s.Outputs[0].Text)) != maxOutputChars+1 {
		t.Fatalf("effort/output = %+v", s)
	}
}

func TestClaudeDefaultDirectory(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	if got := NewClaudeScanner("", 0).dir; got != filepath.Join(os.Getenv("CLAUDE_CONFIG_DIR"), "projects") {
		t.Fatalf("config dir = %q", got)
	}
	custom := t.TempDir()
	if got := NewClaudeScanner(custom, 0).dir; got != custom {
		t.Fatalf("explicit dir = %q", got)
	}
	scanner := NewClaudeScanner(filepath.Join(t.TempDir(), "missing"), 0)
	if snap := scanner.Poll(); len(snap.Sessions) != 0 || snap.ActiveSessions != 0 {
		t.Fatalf("absent dir = %+v", snap)
	}
}

func TestMergeSnapshotsRetainsAllActiveAndCapsCombinedHistory(t *testing.T) {
	var pi, claude Snapshot
	for i := range 30 {
		pi.Sessions = append(pi.Sessions, Summary{ID: fmt.Sprintf("pi-%d", i), Active: i < 25})
		claude.Sessions = append(claude.Sessions, Summary{ID: fmt.Sprintf("claude-code:%d", i), Active: i < 25})
	}
	for i := range 30 {
		pi.Sessions = append(pi.Sessions, Summary{ID: fmt.Sprintf("old-%d", i)})
	}
	snap := MergeSnapshots(pi, claude)
	if snap.ActiveSessions != 50 || len(snap.Sessions) != 70 {
		t.Fatalf("combined snapshot = %d active, %d reported", snap.ActiveSessions, len(snap.Sessions))
	}
	for _, s := range snap.Sessions[:50] {
		if !s.Active {
			t.Fatal("active sessions not sorted first")
		}
	}
}

// Only real conversation records count. Each excluded record would move the
// first prompt, the session's start or last activity, the message count or the
// output if it were accepted; the kept records prove the filter is not too wide.
func TestClaudeEntryFiltersKeepOnlyConversationRecords(t *testing.T) {
	record := func(typ, uuid, ts, extra, message string) string {
		return fmt.Sprintf(`{"type":%q,"sessionId":"shared","uuid":%q,"cwd":"/repo/dashboard","timestamp":"2026-09-30T%sZ"%s,"message":%s}`, typ, uuid, ts, extra, message)
	}
	assistant := func(id, text string, output int) string {
		return fmt.Sprintf(`{"id":%q,"role":"assistant","model":"claude-opus-4-6","content":[{"type":"text","text":%q}],"usage":{"input_tokens":1,"output_tokens":%d}}`, id, text, output)
	}
	scanner, _ := claudeScanner(t,
		// Excluded: a meta user record, before the real prompt.
		record("user", "meta-user", "11:58:00", `,"isMeta":true`, `{"role":"user","content":"meta prompt"}`),
		// Excluded: a user record without a UUID, even with a message id.
		record("user", "", "11:59:00", "", `{"id":"user-msg","role":"user","content":"prompt without uuid"}`),
		claudePrompt,
		// Excluded: a user record whose message role is not user.
		record("user", "wrong-role", "12:00:20", "", `{"role":"assistant","content":"wrong role"}`),
		// Excluded: a meta assistant record.
		record("assistant", "meta-assistant", "12:00:40", `,"isMeta":true`, assistant("meta-msg", "meta output", 50)),
		// Kept: an assistant record without a UUID but with a message id.
		record("assistant", "", "12:01:00", "", assistant("kept-msg", "Kept", 7)),
		// Kept: a system record moves the activity time without a role.
		`{"type":"system","sessionId":"shared","uuid":"sys","timestamp":"2026-09-30T12:01:20Z"}`,
		// Excluded: an assistant record with neither a UUID nor a message id.
		record("assistant", "", "12:01:40", "", `{"role":"assistant","content":[{"type":"text","text":"anonymous"}],"usage":{"output_tokens":90}}`),
	)
	snap := scanner.Poll()
	if len(snap.Sessions) != 1 {
		t.Fatalf("sessions = %+v", snap.Sessions)
	}
	s := snap.Sessions[0]
	if s.FirstPrompt != "Make it work" || s.MessageCount != 2 || s.InputTokens != 1 || s.OutputTokens != 7 || s.TotalTokens != 8 {
		t.Fatalf("counted records = %+v", s)
	}
	if !s.StartedAt.Equal(time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)) || !s.LastActivity.Equal(time.Date(2026, 9, 30, 12, 1, 20, 0, time.UTC)) {
		t.Fatalf("activity span = %v to %v", s.StartedAt, s.LastActivity)
	}
	if len(s.Outputs) != 1 || s.Outputs[0].Text != "Kept" {
		t.Fatalf("outputs = %+v", s.Outputs)
	}
}
