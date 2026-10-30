// Package herdr reads live Herdr sessions through the installed CLI. It never
// starts a server, changes focus, sends input, or reads terminal transcripts.
package herdr

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"github.com/HemSoft/hs-pi-dashboard/internal/backgroundcmd"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Agent is one live agent recognized by Herdr, not a session-file estimate.
type Agent struct {
	PaneID    string `json:"paneId"`
	TabID     string `json:"tabId"`
	Workspace string `json:"workspace"`
	Agent     string `json:"agent"`
	Name      string `json:"name"`
	State     string `json:"state"`
	CWD       string `json:"cwd"`
}

// Session is one running Herdr server namespace.
type Session struct {
	Name      string  `json:"name"`
	Version   string  `json:"version,omitempty"`
	PaneCount int     `json:"paneCount"`
	Agents    []Agent `json:"agents"`
	Error     string  `json:"error,omitempty"`
}

// Snapshot distinguishes an empty, healthy session from unavailable telemetry.
type Snapshot struct {
	GeneratedAt time.Time `json:"generatedAt"`
	Available   bool      `json:"available"`
	Sessions    []Session `json:"sessions"`
	Error       string    `json:"error,omitempty"`
}

type command func(context.Context, ...string) ([]byte, error)

// Collector caches bounded, read-only CLI queries independently of HTTP requests.
type Collector struct {
	run    command
	mu     sync.RWMutex
	cached Snapshot
}

func New(binary string) *Collector {
	if binary == "" {
		binary = findBinary()
	}
	return &Collector{
		run: func(ctx context.Context, args ...string) ([]byte, error) {
			if binary == "" {
				return nil, errors.New("herdr is not installed")
			}
			cmd := backgroundcmd.CommandContext(ctx, binary, args...)
			cmd.Env = withoutCallerContext(os.Environ())
			cmd.WaitDelay = 250 * time.Millisecond
			var out limitedBuffer
			cmd.Stdout = &out
			if err := cmd.Run(); err != nil {
				return nil, err
			}
			return out.Bytes(), nil
		},
		cached: Snapshot{Sessions: []Session{}, Error: "Herdr telemetry has not been collected yet"},
	}
}

func findBinary() string {
	if p, err := exec.LookPath("herdr"); err == nil {
		return p
	}
	home, _ := os.UserHomeDir()
	for _, p := range []string{
		filepath.Join(home, ".local", "bin", "herdr"),
		"/opt/homebrew/bin/herdr",
		"/usr/local/bin/herdr",
		filepath.Join(os.Getenv("LOCALAPPDATA"), "Programs", "Herdr", "bin", "herdr.exe"),
	} {
		if stat, err := os.Stat(p); err == nil && !stat.IsDir() {
			return p
		}
	}
	return ""
}

func withoutCallerContext(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "HERDR_") {
			out = append(out, entry)
		}
	}
	return out
}

type limitedBuffer struct{ bytes.Buffer }

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 4<<20 {
		return 0, errors.New("herdr response exceeded 4 MiB")
	}
	return b.Buffer.Write(p)
}

// Start runs until ctx is canceled. Snapshot remains fast during CLI failures.
func (c *Collector) Start(ctx context.Context) {
	c.Refresh(ctx)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.Refresh(ctx)
		}
	}
}

// Snapshot returns an immutable cached value. Refresh never modifies old slices.
func (c *Collector) Snapshot() Snapshot {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cached
}

// Refresh has a total deadline, including machines with many named sessions.
func (c *Collector) Refresh(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 4*time.Second)
	defer cancel()
	snap := c.collect(ctx)
	c.mu.Lock()
	c.cached = snap
	c.mu.Unlock()
}

func (c *Collector) collect(ctx context.Context) Snapshot {
	snap := Snapshot{GeneratedAt: time.Now(), Sessions: []Session{}}
	body, err := c.run(ctx, "session", "list", "--json")
	if err != nil {
		snap.Error = "Cannot query Herdr sessions"
		return snap
	}
	var list struct {
		Sessions []struct {
			Name    string `json:"name"`
			Running bool   `json:"running"`
		} `json:"sessions"`
	}
	if json.Unmarshal(body, &list) != nil || list.Sessions == nil {
		snap.Error = "Unrecognized Herdr session list"
		return snap
	}
	snap.Available = true
	for _, item := range list.Sessions {
		if !item.Running {
			continue
		}
		if len(snap.Sessions) >= 32 {
			snap.Error = "Only the first 32 running sessions are shown"
			break
		}
		session := Session{Name: item.Name, Agents: []Agent{}}
		body, err := c.run(ctx, "--session", item.Name, "api", "snapshot")
		if err != nil {
			session.Error = "Cannot read this running session"
		} else if err := decodeSession(body, &session); err != nil {
			session.Error = "Unrecognized Herdr snapshot"
		}
		snap.Sessions = append(snap.Sessions, session)
	}
	return snap
}

func decodeSession(body []byte, session *Session) error {
	var envelope struct {
		Result struct {
			Snapshot *struct {
				Version    string            `json:"version"`
				Panes      []json.RawMessage `json:"panes"`
				Workspaces []struct {
					ID    string `json:"workspace_id"`
					Label string `json:"label"`
				} `json:"workspaces"`
				Agents []struct {
					PaneID      string `json:"pane_id"`
					TabID       string `json:"tab_id"`
					WorkspaceID string `json:"workspace_id"`
					Agent       string `json:"agent"`
					Name        string `json:"name"`
					State       string `json:"agent_status"`
					CWD         string `json:"cwd"`
				} `json:"agents"`
			} `json:"snapshot"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return err
	}
	s := envelope.Result.Snapshot
	if s == nil || s.Panes == nil || s.Agents == nil {
		return errors.New("snapshot fields missing")
	}
	session.Version = s.Version
	session.PaneCount = len(s.Panes)
	labels := make(map[string]string, len(s.Workspaces))
	for _, w := range s.Workspaces {
		labels[w.ID] = w.Label
	}
	for _, a := range s.Agents {
		state := a.State
		switch state {
		case "working", "blocked", "done", "idle", "unknown":
		default:
			state = "unknown"
		}
		session.Agents = append(session.Agents, Agent{
			PaneID: a.PaneID, TabID: a.TabID, Workspace: labels[a.WorkspaceID],
			Agent: a.Agent, Name: a.Name, State: state, CWD: a.CWD,
		})
	}
	return nil
}
