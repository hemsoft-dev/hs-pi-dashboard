package fleet

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/herdr"
)

func (s *Server) fetchHerdr(ctx context.Context, t Target) *herdr.Snapshot {
	unavailable := func(message string) *herdr.Snapshot {
		return &herdr.Snapshot{Sessions: []herdr.Session{}, Error: message}
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(t.URL, "/")+"/herdr", nil)
	if err != nil {
		return unavailable("Invalid collector address")
	}
	resp, err := agentClient.Do(req)
	if err != nil {
		return unavailable("Live agent collector is unreachable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return unavailable("Collector upgrade required for live Herdr states")
	}
	if resp.StatusCode != http.StatusOK {
		return unavailable("Live agent collector returned an error")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return unavailable("Cannot read live agent telemetry")
	}
	var snap herdr.Snapshot
	if json.Unmarshal(body, &snap) != nil || snap.Sessions == nil {
		return unavailable("Invalid live agent telemetry")
	}
	if snap.Available && (snap.GeneratedAt.IsZero() || time.Since(snap.GeneratedAt) > 20*time.Second || time.Until(snap.GeneratedAt) > time.Minute) {
		return unavailable("Live agent telemetry is stale")
	}
	return &snap
}
