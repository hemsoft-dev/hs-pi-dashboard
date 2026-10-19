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

// V2 includes complete active and archived shells with potentially large PR
// link bodies. Bound decompressed data while allowing measured 17 MiB fleets.
const (
	maxLegacyShellBytes int64 = 8 << 20
	maxV2ShellBytes     int64 = 64 << 20
)

// Runtime credentials never leave this collector's memory. The local T3 CLI
// creates a one-time grant, exchanged for orchestration:read only.
type runtimeSource struct {
	path, binary string
	v2           bool
	client       *http.Client
	run          func(context.Context, string, ...string) ([]byte, error)
	now          func() time.Time
	verify       func(context.Context, int, time.Time, *url.URL) error
	scopedCLI    *bool
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
	LatestRunStartedAt     string `json:"latestRunStartedAt"`
	LatestRunCompletedAt   string `json:"latestRunCompletedAt"`
	ActivityRunStartedAt   string `json:"activityRunStartedAt"`
	V2                     bool   `json:"-"`
	Status                 string `json:"status"`
	ActivityRunStatus      string `json:"activityRunStatus"`
	completionPresent      bool
	activityStatusPresent  bool
	LatestRunID            *string `json:"latestRunId"`
	ActiveProviderThreadID *string `json:"activeProviderThreadId"`
	LastErrorClass         string  `json:"lastErrorClass"`
	LastVisitedAt          *string `json:"lastVisitedAt"`
	SnoozedAt              *string `json:"snoozedAt"`
	SnoozedUntil           *string `json:"snoozedUntil"`
	SettledOverride        *string `json:"settledOverride"`
	CreationSource         string  `json:"creationSource"`
	Goal                   *struct {
		Status string `json:"status"`
	} `json:"goal"`
	Lineage struct {
		ParentThreadID *string `json:"parentThreadId"`
		Relationship   string  `json:"relationshipToParent"`
	} `json:"lineage"`
	DeletedAt      *string `json:"deletedAt"`
	PendingRequest *struct {
		Kind string `json:"kind"`
	} `json:"pendingRuntimeRequest"`
	PendingBackground []struct {
		Kind string `json:"kind"`
	} `json:"pendingBackgroundTasks"`
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
		now:    time.Now,
		verify: verifyRuntimeProcess,
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
	// A stale record must not send a fresh grant to a replacement listener.
	if err := r.verify(ctx, record.PID, parseTime(record.StartedAt), origin); err != nil {
		r.token = ""
		return nil, errors.New("T3 runtime process identity unavailable")
	}
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
	if r.v2 {
		req.Header.Set("x-t3-orchestration-protocol", "2")
	}
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
	limit := maxLegacyShellBytes
	if r.v2 {
		limit = maxV2ShellBytes
	}
	if resp.ContentLength > limit {
		return nil, errors.New("T3 live snapshot exceeds size limit")
	}
	var shell struct {
		Threads         *[]liveThread `json:"threads"`
		ArchivedThreads *[]liveThread `json:"archivedThreads"`
		Projects        []struct {
			ID            string `json:"id"`
			Title         string `json:"title"`
			WorkspaceRoot string `json:"workspaceRoot"`
		} `json:"projects"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, limit)).Decode(&shell); err != nil || shell.Threads == nil {
		return nil, errors.New("T3 live snapshot incompatible")
	}
	if r.v2 {
		if shell.ArchivedThreads == nil {
			return nil, errors.New("T3 V2 live snapshot incompatible")
		}
		*shell.Threads = append(*shell.Threads, (*shell.ArchivedThreads)...)
	}
	seen := map[string]bool{}
	threads := make([]liveThread, 0, len(*shell.Threads))
	for _, thread := range *shell.Threads {
		thread.V2 = r.v2
		if thread.DeletedAt != nil {
			continue
		}
		if thread.ID == "" || seen[thread.ID] || !thread.valid() {
			return nil, errors.New("T3 live snapshot incompatible")
		}
		seen[thread.ID] = true
		for _, project := range shell.Projects {
			if project.ID == thread.ProjectID {
				thread.Project, thread.Cwd = project.Title, project.WorkspaceRoot
				break
			}
		}
		if thread.WorktreePath != "" {
			thread.Cwd = thread.WorktreePath
		}
		threads = append(threads, thread)
	}
	return threads, nil
}

func (r *runtimeSource) authorize(ctx context.Context, base string) error {
	if r.scopedCLI == nil {
		help, err := r.run(ctx, r.binary, "auth", "pairing", "create", "--help")
		if err != nil {
			return errors.New("T3 local pairing capabilities unavailable")
		}
		supported := strings.Contains(string(help), "--scope ")
		r.scopedCLI = &supported
	}
	args := []string{"auth", "pairing", "create", "--base-dir", filepath.Dir(filepath.Dir(r.path)),
		"--ttl", "1m", "--label", "hs-pi-dashboard read-only", "--json"}
	if *r.scopedCLI {
		args = append(args, "--scope", "orchestration:read")
	}
	// Older supported CLIs have no scope flag. Their one-minute, single-use
	// grant is sent only to the verified local process and immediately narrowed.
	data, err := r.run(ctx, r.binary, args...)
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
	if t.V2 {
		if !v2StatusValid(t.Status) {
			return false
		}
		return t.ActivityRunStatus == "" || t.ActivityRunStatus == "preparing" || t.ActivityRunStatus == "starting" || t.ActivityRunStatus == "running" || t.ActivityRunStatus == "waiting"
	}
	// A V2 response must never validate a frozen explicitly selected V1 file.
	if t.Status != "" {
		return false
	}
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

// Count live work, including a ready model monitoring in the background,
// while excluding terminal sessions and archived threads.
func (t liveThread) state() (string, bool) {
	if t.V2 {
		return t.v2State()
	}
	if t.ArchivedAt != nil {
		return "stopped", false
	}
	if t.Session != nil {
		switch t.Session.Status {
		case "error":
			return "error", false
		case "stopped", "interrupted":
			return "stopped", false
		}
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
		}
	}
	if t.BackgroundLiveness != "" {
		return t.BackgroundLiveness, true
	}
	return "idle", false
}

func (t liveThread) runningTurn() bool {
	if t.V2 {
		state := t.ActivityRunStatus
		if state == "" {
			state = t.Status
		}
		return state == "running" || state == "starting" || state == "preparing"
	}
	return t.Session != nil && (t.Session.Status == "running" || t.Session.Status == "starting")
}

func v2StatusValid(status string) bool {
	switch status {
	case "idle", "preparing", "queued", "starting", "running", "waiting", "completed", "interrupted", "failed", "cancelled", "rolled_back":
		return true
	}
	return false
}

func (t liveThread) v2State() (string, bool) {
	if t.ArchivedAt != nil {
		return "stopped", false
	}
	status := t.ActivityRunStatus
	if status == "" {
		status = t.Status
	}
	switch status {
	case "failed":
		return "error", false
	case "cancelled", "interrupted", "rolled_back":
		return "stopped", false
	}
	if t.PendingRequest != nil {
		return "waiting", true
	}
	switch status {
	case "preparing", "starting":
		return "connecting", true
	case "running":
		return "working", true
	}
	for _, task := range t.PendingBackground {
		if task.Kind == "monitor" {
			return "monitoring", true
		}
	}
	if len(t.PendingBackground) > 0 {
		return "working", true
	}
	// V2 persists a successful provider turn as waiting until checkpoint
	// capture finishes. Only PendingRequest above needs an operator response.
	if status == "waiting" {
		return "working", true
	}
	return "idle", false
}
