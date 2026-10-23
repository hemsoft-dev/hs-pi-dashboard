package usage

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

func TestAntigravityCommandRequiresReadOnlyUsageAndKeepsArgonSeparate(t *testing.T) {
	future := time.Now().Add(72 * time.Hour).UTC().Format(time.RFC3339)
	payload := `{"num_turns":0,"command":{"name":"usage","data":{"groups":[{"buckets":[{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.8,"resetTime":"` + future + `"},{"bucketId":"gemini-argon-wtu-weekly","window":"weekly","remainingFraction":0.3,"resetTime":"` + future + `"}]}]}}}`
	f := &Fetcher{Home: t.TempDir(), agyRun: func(context.Context) ([]byte, error) { return []byte(payload), nil }}
	card := f.antigravity(context.Background())
	if card.Error != "" || len(card.Gauges) != 2 || card.Gauges[1].Label != "Argon W" {
		t.Fatal(card)
	}
	for _, invalid := range []string{strings.Replace(payload, `"num_turns":0`, `"num_turns":1`, 1), strings.Replace(payload, `"name":"usage"`, `"name":"chat"`, 1), `{"command":{"name":"usage"}}`} {
		if _, err := parseAntigravityCommand([]byte(invalid)); err == nil {
			t.Fatal("accepted non-usage response")
		}
	}
}
func TestAntigravityMissingSessionDoesNotLaunchCLI(t *testing.T) {
	f := &Fetcher{Home: t.TempDir()}
	t.Setenv("HS_PI_DASHBOARD_AGY_BIN", "must-not-run")
	if _, err := f.runAntigravityUsage(context.Background()); err == nil {
		t.Fatal("no session accepted")
	}
}
func TestUsageCommandBufferCapAppliesThroughCopy(t *testing.T) {
	var b usageCommandBuffer
	if _, err := io.Copy(&b, struct{ io.Reader }{strings.NewReader(strings.Repeat("x", 5<<20))}); err == nil || b.buffer.Len() > 4<<20 {
		t.Fatal("unbounded output", err)
	}
}
