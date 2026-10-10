package t3

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	osexec "os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Runtime credentials never leave this collector's memory. The local T3 CLI
// creates a one-time grant, exchanged for orchestration:read only.
type runtimeSource struct {
	path, binary string
	client       *http.Client
	run          func(context.Context, string, ...string) ([]byte, error)
	now          func() time.Time
	identity     string
	token        string
	expires      time.Time
}

type liveThread struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	ProjectID      string `json:"projectId"`
	Project        string `json:"-"`
	Cwd            string `json:"-"`
	WorktreePath   string `json:"worktreePath"`
	CreatedAt      string `json:"createdAt"`
	UpdatedAt      string `json:"updatedAt"`
	ModelSelection struct {
		Instance string          `json:"instanceId"`
		Model    string          `json:"model"`
		Options  json.RawMessage `json:"options"`
	} `json:"modelSelection"`
	ArchivedAt          *string      `json:"archivedAt"`
	BackgroundLiveness  string       `json:"backgroundLiveness"`
	HasPendingApprovals bool         `json:"hasPendingApprovals"`
	HasPendingUserInput bool         `json:"hasPendingUserInput"`
	Session             *liveSession `json:"session"`
}

type liveSession struct {
	Status       string `json:"status"`
	ProviderName string `json:"providerName"`
}

func newRuntime(path, binary string) *runtimeSource {
	if binary == "" {
		binary = "t3"
		if runtime.GOOS == "windows" {
			// Go cannot execute npm's .cmd/.ps1 launchers directly.
			if native, err := osexec.LookPath("t3.exe"); err == nil {
				binary = native
			} else if wrapper, err := osexec.LookPath("t3"); err == nil {
				native := filepath.Join(filepath.Dir(wrapper), "node_modules", "t3", "node_modules", "@t3code", "t3-win32-x64", "t3.exe")
				if info, err := os.Stat(native); err == nil && !info.IsDir() {
					binary = native
				}
			}
		}
	}
	return &runtimeSource{
		path: filepath.Join(filepath.Dir(path), "server-runtime.json"), binary: binary,
		client: &http.Client{Transport: &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		now: time.Now,
		run: func(ctx context.Context, binary string, args ...string) ([]byte, error) {
			// Capture stdout only; CLI diagnostics can include pairing URLs.
			cmd := osexec.CommandContext(ctx, binary, args...)
			hideCommandWindow(cmd)
			return cmd.Output()
		},
	}
}

func (r *runtimeSource) snapshot(ctx context.Context) ([]liveThread, error) {
	var record struct {
		PID       int    `json:"pid"`
		Origin    string `json:"origin"`
		StartedAt string `json:"startedAt"`
	}
	data, err := os.ReadFile(r.path)
	if err != nil || json.Unmarshal(data, &record) != nil || record.PID <= 0 || parseTime(record.StartedAt).IsZero() {
		return nil, errors.New("T3 runtime discovery unavailable")
	}
	origin, err := url.Parse(record.Origin)
	if err != nil || (origin.Scheme != "http" && origin.Scheme != "https") || origin.User != nil ||
		(origin.Path != "" && origin.Path != "/") || origin.RawQuery != "" || origin.Fragment != "" ||
		(origin.Hostname() != "localhost" && !net.ParseIP(origin.Hostname()).IsLoopback()) {
		return nil, errors.New("T3 runtime must use a local loopback origin")
	}
	base := strings.TrimRight(origin.String(), "/")
	identity := string(data)
	if identity != r.identity {
		r.token, r.identity = "", identity
	}
	if r.token == "" || !r.now().Add(time.Minute).Before(r.expires) {
		if err := r.authorize(ctx, base); err != nil {
			return nil, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/orchestration/shell", nil)
	if err != nil {
		return nil, errors.New("T3 runtime request unavailable")
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, errors.New("T3 live runtime unavailable")
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		r.token = ""
		return nil, errors.New("T3 runtime authentication unavailable; renewal on next poll")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, errors.New("T3 live runtime unavailable")
	}
	var shell struct {
		Threads  *[]liveThread `json:"threads"`
		Projects []struct {
			ID            string `json:"id"`
			Title         string `json:"title"`
			WorkspaceRoot string `json:"workspaceRoot"`
		} `json:"projects"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&shell); err != nil || shell.Threads == nil {
		return nil, errors.New("T3 live snapshot incompatible")
	}
	seen := map[string]bool{}
	for i, thread := range *shell.Threads {
		if thread.ID == "" || seen[thread.ID] || !thread.valid() {
			return nil, errors.New("T3 live snapshot incompatible")
		}
		seen[thread.ID] = true
		for _, project := range shell.Projects {
			if project.ID == thread.ProjectID {
				(*shell.Threads)[i].Project, (*shell.Threads)[i].Cwd = project.Title, project.WorkspaceRoot
				break
			}
		}
		if thread.WorktreePath != "" {
			(*shell.Threads)[i].Cwd = thread.WorktreePath
		}
	}
	return *shell.Threads, nil
}

func (r *runtimeSource) authorize(ctx context.Context, base string) error {
	data, err := r.run(ctx, r.binary, "auth", "pairing", "create", "--base-dir", filepath.Dir(filepath.Dir(r.path)),
		"--ttl", "1m", "--label", "hs-pi-dashboard read-only", "--json")
	var grant struct {
		Credential string `json:"credential"`
	}
	if err != nil || json.Unmarshal(data, &grant) != nil || grant.Credential == "" {
		return errors.New("T3 local pairing unavailable; check -t3-bin and environment access")
	}
	form := url.Values{
		"grant_type":           {"urn:ietf:params:oauth:grant-type:token-exchange"},
		"subject_token":        {grant.Credential},
		"subject_token_type":   {"urn:t3:params:oauth:token-type:environment-bootstrap"},
		"requested_token_type": {"urn:ietf:params:oauth:token-type:access_token"},
		"scope":                {"orchestration:read"}, "client_label": {"hs-pi-dashboard read-only"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/oauth/token", strings.NewReader(form.Encode()))
	if err != nil {
		return errors.New("T3 read-only token exchange unavailable")
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := r.client.Do(req)
	if err != nil {
		return errors.New("T3 read-only token exchange unavailable")
	}
	defer resp.Body.Close()
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Scope       string `json:"scope"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&token) != nil ||
		token.AccessToken == "" || token.TokenType != "Bearer" || token.Scope != "orchestration:read" || token.ExpiresIn <= 60 || token.ExpiresIn > 366*24*60*60 {
		return errors.New("T3 read-only token exchange refused or incompatible")
	}
	r.token, r.expires = token.AccessToken, r.now().Add(time.Duration(token.ExpiresIn)*time.Second)
	return nil
}

func (t liveThread) valid() bool {
	if t.BackgroundLiveness != "" && t.BackgroundLiveness != "working" && t.BackgroundLiveness != "monitoring" {
		return false
	}
	if t.Session == nil {
		return true
	}
	switch t.Session.Status {
	case "idle", "starting", "running", "ready", "interrupted", "stopped", "error":
		return true
	default:
		return false
	}
}

// Match T3's Working shelf, including background work with a ready model.
func (t liveThread) state() (string, bool) {
	if t.ArchivedAt != nil {
		return "stopped", false
	}
	if t.HasPendingApprovals || t.HasPendingUserInput {
		return "waiting", true
	}
	if t.Session != nil {
		switch t.Session.Status {
		case "starting":
			return "connecting", true
		case "running":
			return "working", true
		case "error":
			return "error", false
		}
	}
	if t.BackgroundLiveness != "" {
		return t.BackgroundLiveness, true
	}
	if t.Session != nil && (t.Session.Status == "stopped" || t.Session.Status == "interrupted") {
		return "stopped", false
	}
	return "idle", false
}
