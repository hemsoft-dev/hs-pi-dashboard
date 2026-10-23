package usage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

func (f *Fetcher) antigravity(ctx context.Context) Card {
	card := Card{Key: "antigravity", Label: "Antigravity", Kind: KindQuota, FetchedAt: time.Now()}
	run := f.agyRun
	if run == nil {
		run = f.runAntigravityUsage
	}
	data, err := run(ctx)
	if err == nil {
		card.Gauges, err = parseAntigravityCommand(data)
	}
	if err == nil {
		return card
	}
	legacy := f.antigravityLegacy(ctx)
	if len(legacy.Gauges) > 0 {
		legacy.Warning = "Antigravity CLI weekly buckets unavailable; showing live legacy pools (SUS n/a)"
	}
	return legacy
}

func parseAntigravityCommand(data []byte) ([]Gauge, error) {
	var response struct {
		Turns   *int `json:"num_turns"`
		Command struct {
			Name string          `json:"name"`
			Data json.RawMessage `json:"data"`
		} `json:"command"`
	}
	if json.Unmarshal(data, &response) != nil || response.Turns == nil || *response.Turns != 0 || response.Command.Name != "usage" {
		return nil, errors.New("antigravity usage command unavailable")
	}
	return parseAntigravityWindows(response.Command.Data)
}

// Named field avoids io.Copy discovering a promoted bytes.Buffer.ReadFrom.
type usageCommandBuffer struct{ buffer bytes.Buffer }

func (b *usageCommandBuffer) Write(data []byte) (int, error) {
	if b.buffer.Len()+len(data) > 4<<20 {
		return 0, errors.New("usage command output too large")
	}
	return b.buffer.Write(data)
}

func (f *Fetcher) runAntigravityUsage(ctx context.Context) ([]byte, error) {
	// Never initiate a CLI sign-in from a background collector.
	if _, err := os.Stat(filepath.Join(f.Home, ".gemini", "antigravity-cli", "antigravity-oauth-token")); err != nil {
		return nil, errors.New("antigravity CLI session unavailable")
	}
	bin := envValue("HS_PI_DASHBOARD_AGY_BIN")
	if bin == "" {
		bin = "agy"
		if path, err := exec.LookPath(bin); err == nil {
			bin = path
		} else if path := filepath.Join(f.Home, ".local", "bin", "agy"); fileExists(path) {
			bin = path
		}
	}
	c, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, bin, "-p", "/usage", "--output-format", "json")
	cmd.Stderr = io.Discard
	cmd.WaitDelay = time.Second
	var out usageCommandBuffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, errors.New("antigravity usage command failed")
	}
	return out.buffer.Bytes(), nil
}

func fileExists(path string) bool { info, err := os.Stat(path); return err == nil && !info.IsDir() }
