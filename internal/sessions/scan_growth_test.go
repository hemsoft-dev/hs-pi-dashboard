package sessions

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Regression tests for #107: bytes appended after pollFile's Stat but before
// its read must be applied exactly once.

const piGrowthHeader = `{"type":"session","version":3,"id":"grow","timestamp":"2026-09-30T12:00:00Z","cwd":"/repo/dashboard"}`

func piGrowthMessage(i int, text string) string {
	return fmt.Sprintf(`{"type":"message","id":"a%d","timestamp":"2026-09-30T12:01:%02dZ","message":{"role":"assistant","content":[{"type":"text","text":%q}],"usage":{"input":%d,"output":%d,"totalTokens":%d,"cost":{"total":0.25}}}}`,
		i, i, text, 10*i, i, 11*i)
}

func appendFile(t *testing.T, path, data string) {
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

// growOnOpen makes the scanner's next open append data first, so the bytes
// land after pollFile's Stat and before its read, as a live writer's can.
func growOnOpen(t *testing.T, s *Scanner, data string) {
	t.Helper()
	s.open = func(path string) (*os.File, error) {
		s.open = os.Open
		appendFile(t, path, data)
		return os.Open(path)
	}
}

// freshSummary is what one read of the finished file gives.
func freshSummary(t *testing.T, s *Scanner) Summary {
	t.Helper()
	fresh := NewScanner(s.dir, s.activeWindow)
	fresh.isClaude, fresh.now = s.isClaude, s.now
	snap := fresh.Poll()
	if len(snap.Sessions) != 1 {
		t.Fatalf("fresh sessions = %+v", snap.Sessions)
	}
	return snap.Sessions[0]
}

func TestPollGrowthAfterStatMatchesFreshScan(t *testing.T) {
	pi := func(i int) string { return piGrowthMessage(i, fmt.Sprintf("out %d", i)) }
	claude := func(i int) string {
		return claudeAssistant(fmt.Sprintf("a%d", i), fmt.Sprintf("msg%d", i), fmt.Sprintf(`[{"type":"text","text":"out %d"}]`, i), i)
	}
	half := func(line string) string { return line[:len(line)/2] }
	rest := func(line string) string { return line[len(line)/2:] }
	cases := []struct {
		name                  string
		claude                bool
		header                string
		before, during, after string
		messages              int
	}{
		// Old offset: the next poll applied 2 and 3 again.
		{"pi complete records", false, piGrowthHeader, pi(1) + "\n", pi(2) + "\n" + pi(3) + "\n", "", 3},
		// Old offset: the carried half of 2 was joined to a second copy of it,
		// and record 2 was dropped as malformed.
		{"pi partial record", false, piGrowthHeader, pi(1) + "\n", half(pi(2)), rest(pi(2)) + "\n", 2},
		{"claude complete records", true, claudePrompt, claude(1) + "\n", claude(2) + "\n", "", 3},
		{"claude partial record", true, claudePrompt, claude(1) + "\n", half(claude(2)), rest(claude(2)) + "\n", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeSession(t, dir, "s.jsonl", tc.header)
			path := filepath.Join(dir, "repo", "s.jsonl")
			s := NewScanner(dir, 2*time.Minute)
			s.isClaude = tc.claude
			s.now = func() time.Time { return time.Date(2026, 9, 30, 12, 2, 0, 0, time.UTC) }
			s.Poll()

			appendFile(t, path, tc.before)
			growOnOpen(t, s, tc.during)
			s.Poll()
			appendFile(t, path, tc.after)
			got := s.Poll().Sessions[0]

			if got.MessageCount != tc.messages {
				t.Fatalf("messageCount = %d, want %d: %+v", got.MessageCount, tc.messages, got)
			}
			if want := freshSummary(t, s); !reflect.DeepEqual(got, want) {
				t.Fatalf("incremental summary differs from a fresh scan\n got: %+v\nwant: %+v", got, want)
			}
			if got.Outputs[0].Text != "out 2" && got.Outputs[0].Text != "out 3" {
				t.Fatalf("newest output = %+v", got.Outputs)
			}
		})
	}
}

// The saved offset counts bytes read after Stat, so a file that shrinks
// below it is rescanned, even when it stays above the size Stat reported.
func TestPollRescansFileShrunkBelowReadOffset(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "s.jsonl", piGrowthHeader, piGrowthMessage(1, "one"))
	path := filepath.Join(dir, "repo", "s.jsonl")
	s := NewScanner(dir, 2*time.Minute)
	growOnOpen(t, s, piGrowthMessage(2, "two")+"\n")
	statSize := int64(len(piGrowthHeader) + len(piGrowthMessage(1, "one")) + 2)
	if got := s.Poll().Sessions[0]; got.MessageCount != 2 {
		t.Fatalf("growth poll = %+v", got)
	}
	read := s.files[path].size
	if read <= statSize {
		t.Fatalf("offset %d not past Stat size %d", read, statSize)
	}

	short := piGrowthHeader + "\n"
	pad := int((statSize+read)/2) - len(short) - len(piGrowthMessage(9, "")) - 1
	rewritten := short + piGrowthMessage(9, strings.Repeat("x", pad)) + "\n"
	if n := int64(len(rewritten)); n <= statSize || n >= read {
		t.Fatalf("rewritten size %d not between %d and %d", n, statSize, read)
	}
	if err := os.WriteFile(path, []byte(rewritten), 0o600); err != nil {
		t.Fatal(err)
	}
	got := s.Poll().Sessions[0]
	if got.MessageCount != 1 || got.InputTokens != 90 || !reflect.DeepEqual(got, freshSummary(t, s)) {
		t.Fatalf("shrunk file not rescanned = %+v", got)
	}

	if err := os.WriteFile(path, []byte(short), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := s.Poll().Sessions[0]; got.MessageCount != 0 || got.TotalTokens != 0 || len(got.Outputs) != 0 {
		t.Fatalf("truncated file not rescanned = %+v", got)
	}
}

// A partial line is carried up to maxPartialLine bytes; one byte more drops
// it, and the scanner resumes at the next complete line.
func TestPollPartialLineCap(t *testing.T) {
	dir := t.TempDir()
	writeSession(t, dir, "s.jsonl", piGrowthHeader)
	path := filepath.Join(dir, "repo", "s.jsonl")
	s := NewScanner(dir, 2*time.Minute)
	s.Poll()

	appendFile(t, path, strings.Repeat("x", maxPartialLine))
	s.Poll()
	if got := len(s.files[path].partial); got != maxPartialLine {
		t.Fatalf("partial = %d bytes, want %d", got, maxPartialLine)
	}
	appendFile(t, path, "x")
	s.Poll()
	if s.files[path].partial != nil {
		t.Fatalf("partial over the cap kept: %d bytes", len(s.files[path].partial))
	}
	appendFile(t, path, "\n"+piGrowthMessage(1, "one")+"\n")
	if got := s.Poll().Sessions[0]; got.MessageCount != 1 || got.InputTokens != 10 {
		t.Fatalf("line after dropped partial = %+v", got)
	}
}
