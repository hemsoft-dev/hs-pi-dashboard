// Package probe checks the mutation sandbox from the inside.
//
// scripts/mutation_sandbox.py runs this test with `go test` in the same
// bubblewrap sandbox, with the same mounts and environment, as the step it is
// about to start, and refuses that step unless every check passes. The host
// side plants synthetic canaries first: a file outside every mount, a loopback
// TCP listener, a Unix socket and an abstract socket, a process with a marker
// in its arguments, a fake pairing executable on the launcher's PATH and a
// dummy credential in the launcher's environment. None of them hold real data.
// The probe never reads a real home folder, credential store or T3 endpoint: it
// only looks for the synthetic canaries and at the sandbox's own view of
// mounts, processes, descriptors and network.
package probe

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Mount is one expected mount, keyed by its mount point in the sandbox.
type Mount struct {
	Root     string `json:"root"`
	FSType   string `json:"fstype"`
	ReadOnly bool   `json:"read_only"`
}

// FileID names a host file by device and inode.
type FileID struct {
	Dev uint64 `json:"dev"`
	Ino uint64 `json:"ino"`
}

// Config is written by the launcher. It holds canary locations and hashes,
// never a secret.
type Config struct {
	ForbiddenPaths  []string         `json:"forbidden_paths"`
	Mounts          map[string]Mount `json:"mounts"`
	EnvCanaryName   string           `json:"env_canary_name"`
	EnvCanarySHA256 string           `json:"env_canary_sha256"`
	AllowedEnv      []string         `json:"allowed_env"`
	ProcessMarker   string           `json:"process_marker"`
	CanaryFile      FileID           `json:"canary_file"`
	CanarySockets   []uint64         `json:"canary_socket_inodes"`
	HostTCPAddr     string           `json:"host_tcp_addr"`
	HostUnixPath    string           `json:"host_unix_path"`
	HostAbstract    string           `json:"host_abstract"`
	PairingNames    []string         `json:"pairing_names"`
	MaskedTools     []string         `json:"masked_tools"`
	StubSHA256      string           `json:"stub_sha256"`
	Home            string           `json:"home"`
	HomeAllowed     []string         `json:"home_allowed"`
	TempDir         string           `json:"temp_dir"`
	ReadOnlyDirs    []string         `json:"read_only_dirs"`
	WritableDirs    []string         `json:"writable_dirs"`
	GoVersion       string           `json:"go_version"`
	Gremlins        string           `json:"gremlins"`
	GremlinsModule  string           `json:"gremlins_module"`
	GremlinsVersion string           `json:"gremlins_version"`
	GremlinsSum     string           `json:"gremlins_sum"`
	Result          string           `json:"result"`
}

// Check is one recorded result.
type Check struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

func TestSandbox(t *testing.T) {
	raw, err := os.ReadFile(os.Getenv("MUTATION_PROBE_CONFIG"))
	if err != nil {
		t.Fatalf("read probe config: %v", err)
	}
	var cfg Config
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse probe config: %v", err)
	}
	var results []Check
	for _, c := range checks {
		detail, err := c.run(&cfg)
		ok := err == nil
		if !ok {
			detail = err.Error()
			t.Errorf("%s: %v", c.name, err)
		}
		results = append(results, Check{Name: c.name, OK: ok, Detail: detail})
	}
	out, _ := json.MarshalIndent(results, "", "  ")
	if err := os.WriteFile(cfg.Result, append(out, '\n'), 0o644); err != nil {
		t.Fatalf("write probe result: %v", err)
	}
}

var checks = []struct {
	name string
	run  func(*Config) (string, error)
}{
	// Negative checks: nothing of the host's may be reachable.
	{"host-paths-absent", hostPathsAbsent},
	{"mounts-allowlisted", mountsAllowlisted},
	{"read-only-mounts", readOnlyMounts},
	{"environment-allowlisted", environmentAllowlisted},
	{"environment-canary-absent-everywhere", environCanaryAbsent},
	{"host-processes-invisible", hostProcessesInvisible},
	{"inherited-descriptors-absent", inheritedDescriptorsAbsent},
	{"network-loopback-only", networkLoopbackOnly},
	{"host-listeners-unreachable", hostListenersUnreachable},
	{"pairing-executable-absent", pairingExecutableAbsent},
	{"auth-tools-masked", authToolsMasked},
	{"home-fresh", homeFresh},
	{"no-privileges", noPrivileges},
	{"nested-user-namespace-refused", nestedUserNamespaceRefused},
	// The validators above must be able to fail.
	{"validators-detect-planted-leaks", validatorsDetectPlantedLeaks},
	// Positive controls: what the mutation run needs works.
	{"go-toolchain", goToolchain},
	{"go-build-and-run", goBuildAndRun},
	{"gremlins-version", gremlinsVersion},
	{"loopback-http-fixture", loopbackHTTP},
	{"fake-cli-fixture", fakeCLI},
	{"temp-and-writable-dirs", writableDirs},
}

func hostPathsAbsent(cfg *Config) (string, error) {
	if len(cfg.ForbiddenPaths) == 0 {
		return "", errors.New("no forbidden paths configured")
	}
	for _, p := range cfg.ForbiddenPaths {
		if _, err := os.Lstat(p); !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s is visible (lstat: %v)", p, err)
		}
	}
	return fmt.Sprintf("%d host paths absent", len(cfg.ForbiddenPaths)), nil
}

type mountLine struct {
	point, root, fstype string
	readOnly            bool
}

func unescapeMount(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func parseMountinfo(text string) ([]mountLine, error) {
	var mounts []mountLine
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		fields := strings.Fields(line)
		sep := -1
		for i, f := range fields {
			if f == "-" {
				sep = i
				break
			}
		}
		if len(fields) < 6 || sep < 6 || sep+1 >= len(fields) {
			return nil, fmt.Errorf("malformed mountinfo line %q", line)
		}
		ro := false
		for _, opt := range strings.Split(fields[5], ",") {
			ro = ro || opt == "ro"
		}
		mounts = append(mounts, mountLine{point: unescapeMount(fields[4]), root: unescapeMount(fields[3]), fstype: fields[sep+1], readOnly: ro})
	}
	return mounts, nil
}

// bwrap's own mounts: the root tmpfs, /proc and its read-only submounts,
// and the minimal /dev with its device nodes.
var bwrapDevices = map[string]bool{"/dev/null": true, "/dev/zero": true, "/dev/full": true, "/dev/random": true, "/dev/urandom": true, "/dev/tty": true}

func validateMounts(mounts []mountLine, want map[string]Mount) error {
	seen := map[string]bool{}
	var problems []string
	for _, m := range mounts {
		switch e, ok := want[m.point]; {
		case ok:
			if m.fstype != e.FSType || m.root != e.Root || (e.ReadOnly && !m.readOnly) {
				problems = append(problems, fmt.Sprintf("mount %s is %s %s ro=%v, want %s %s ro=%v", m.point, m.fstype, m.root, m.readOnly, e.FSType, e.Root, e.ReadOnly))
			}
			seen[m.point] = true
		case m.fstype == "proc" && (m.point == "/proc" || strings.HasPrefix(m.point, "/proc/")):
		case bwrapDevices[m.point] && m.fstype == "devtmpfs" && m.root == "/"+filepath.Base(m.point):
		case m.point == "/dev" && m.fstype == "tmpfs", m.point == "/dev/shm" && m.fstype == "tmpfs", m.point == "/dev/pts" && m.fstype == "devpts":
		default:
			problems = append(problems, fmt.Sprintf("unexpected mount %s (%s, root %s)", m.point, m.fstype, m.root))
		}
	}
	var missing []string
	for point := range want {
		if !seen[point] {
			missing = append(missing, point)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		problems = append(problems, "expected mounts missing: "+strings.Join(missing, ", "))
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

func mountsAllowlisted(cfg *Config) (string, error) {
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", err
	}
	mounts, err := parseMountinfo(string(raw))
	if err != nil {
		return "", err
	}
	if err := validateMounts(mounts, cfg.Mounts); err != nil {
		return "", err
	}
	return fmt.Sprintf("%d mounts, %d declared, all allowlisted", len(mounts), len(cfg.Mounts)), nil
}

func readOnlyMounts(cfg *Config) (string, error) {
	for _, dir := range cfg.ReadOnlyDirs {
		f, err := os.CreateTemp(dir, "probe-")
		if err == nil {
			name := f.Name()
			f.Close()
			os.Remove(name)
			return "", fmt.Errorf("%s is writable", dir)
		}
	}
	return fmt.Sprintf("%d directories refuse writes", len(cfg.ReadOnlyDirs)), nil
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func environHasCanary(environ []string, name, digest string) error {
	for _, kv := range environ {
		k, v, _ := strings.Cut(kv, "=")
		if k == name {
			return fmt.Errorf("variable %s is set", name)
		}
		if sha(v) == digest {
			return fmt.Errorf("variable %s holds the canary value", k)
		}
	}
	return nil
}

func environmentAllowlisted(cfg *Config) (string, error) {
	allowed := map[string]bool{"PWD": true}
	for _, k := range cfg.AllowedEnv {
		allowed[k] = true
	}
	var keys []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !allowed[k] {
			return "", fmt.Errorf("variable %s is not allowlisted", k)
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, " "), nil
}

func procDirs() ([]string, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	var pids []string
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err == nil {
			pids = append(pids, e.Name())
		}
	}
	return pids, nil
}

func environCanaryAbsent(cfg *Config) (string, error) {
	if cfg.EnvCanaryName == "" || cfg.EnvCanarySHA256 == "" {
		return "", errors.New("no environment canary configured")
	}
	pids, err := procDirs()
	if err != nil {
		return "", err
	}
	read := 0
	for _, pid := range pids {
		raw, err := os.ReadFile("/proc/" + pid + "/environ")
		if err != nil {
			continue // exited, or not readable (bwrap's own init is not dumpable)
		}
		read++
		if err := environHasCanary(strings.Split(string(raw), "\x00"), cfg.EnvCanaryName, cfg.EnvCanarySHA256); err != nil {
			return "", fmt.Errorf("pid %s: %v", pid, err)
		}
	}
	if err := environHasCanary(os.Environ(), cfg.EnvCanaryName, cfg.EnvCanarySHA256); err != nil {
		return "", err
	}
	return fmt.Sprintf("canary absent from %d readable process environments of %d", read, len(pids)), nil
}

func hostProcessesInvisible(cfg *Config) (string, error) {
	if cfg.ProcessMarker == "" {
		return "", errors.New("no process marker configured")
	}
	pids, err := procDirs()
	if err != nil {
		return "", err
	}
	comm, err := os.ReadFile("/proc/1/comm")
	if err != nil || strings.TrimSpace(string(comm)) != "bwrap" {
		return "", fmt.Errorf("pid 1 is %q (%v), want bwrap's init in a new PID namespace", strings.TrimSpace(string(comm)), err)
	}
	for _, pid := range pids {
		raw, _ := os.ReadFile("/proc/" + pid + "/cmdline")
		if strings.Contains(string(raw), cfg.ProcessMarker) {
			return "", fmt.Errorf("host marker process visible as pid %s", pid)
		}
	}
	if len(pids) > 64 {
		return "", fmt.Errorf("%d processes visible; a fresh PID namespace holds only the probe's own", len(pids))
	}
	return fmt.Sprintf("%d sandbox processes, pid 1 is bwrap", len(pids)), nil
}

func inheritedDescriptorsAbsent(cfg *Config) (string, error) {
	sockets := map[string]bool{}
	for _, ino := range cfg.CanarySockets {
		sockets[fmt.Sprintf("socket:[%d]", ino)] = true
	}
	if len(sockets) == 0 || cfg.CanaryFile.Ino == 0 {
		return "", errors.New("no descriptor canaries configured")
	}
	pids, err := procDirs()
	if err != nil {
		return "", err
	}
	seen := 0
	for _, pid := range pids {
		fds, err := os.ReadDir("/proc/" + pid + "/fd")
		if err != nil {
			continue
		}
		for _, fd := range fds {
			path := "/proc/" + pid + "/fd/" + fd.Name()
			seen++
			if link, err := os.Readlink(path); err == nil && sockets[link] {
				return "", fmt.Errorf("pid %s fd %s is a host canary %s", pid, fd.Name(), link)
			}
			if info, err := os.Stat(path); err == nil {
				if st, ok := info.Sys().(*syscall.Stat_t); ok && uint64(st.Dev) == cfg.CanaryFile.Dev && st.Ino == cfg.CanaryFile.Ino {
					return "", fmt.Errorf("pid %s fd %s is the host canary file", pid, fd.Name())
				}
			}
		}
	}
	// This process: stdin is /dev/null, and nothing past stderr names a path.
	if link, _ := os.Readlink("/proc/self/fd/0"); link != "/dev/null" {
		return "", fmt.Errorf("stdin is %q, want /dev/null", link)
	}
	return fmt.Sprintf("%d descriptors checked, no canary", seen), nil
}

func networkLoopbackOnly(cfg *Config) (string, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return "", err
	}
	var names []string
	for _, i := range ifaces {
		names = append(names, i.Name)
	}
	if len(names) != 1 || names[0] != "lo" {
		return "", fmt.Errorf("interfaces %v, want only lo", names)
	}
	for _, addr := range []string{"192.0.2.1:9", "[2001:db8::1]:9"} {
		if c, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
			c.Close()
			return "", fmt.Errorf("reached %s outside the sandbox", addr)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := net.DefaultResolver.LookupHost(ctx, "example.com"); err == nil {
		return "", errors.New("DNS resolution works; the sandbox has a route out")
	}
	return "only lo; TEST-NET dials and DNS fail", nil
}

func hostListenersUnreachable(cfg *Config) (string, error) {
	if cfg.HostTCPAddr == "" || cfg.HostUnixPath == "" || cfg.HostAbstract == "" {
		return "", errors.New("no listener canaries configured")
	}
	if c, err := net.DialTimeout("tcp", cfg.HostTCPAddr, 2*time.Second); err == nil {
		c.Close()
		return "", fmt.Errorf("host loopback canary %s reachable", cfg.HostTCPAddr)
	}
	if c, err := net.DialTimeout("unix", cfg.HostUnixPath, 2*time.Second); err == nil {
		c.Close()
		return "", errors.New("host Unix socket canary reachable")
	}
	if c, err := net.DialTimeout("unix", "@"+cfg.HostAbstract, 2*time.Second); err == nil {
		c.Close()
		return "", errors.New("host abstract socket canary reachable")
	}
	raw, err := os.ReadFile("/proc/net/unix")
	if err == nil && strings.Contains(string(raw), cfg.HostAbstract) {
		return "", errors.New("host abstract socket listed in /proc/net/unix")
	}
	return "TCP, Unix and abstract canaries refuse", nil
}

func pairingExecutableAbsent(cfg *Config) (string, error) {
	if len(cfg.PairingNames) == 0 {
		return "", errors.New("no pairing executable canary configured")
	}
	for _, name := range cfg.PairingNames {
		if p, err := exec.LookPath(name); err == nil {
			return "", fmt.Errorf("%s resolves to %s", name, p)
		}
	}
	return strings.Join(cfg.PairingNames, ", ") + " not on PATH", nil
}

func authToolsMasked(cfg *Config) (string, error) {
	for _, p := range cfg.MaskedTools {
		raw, err := os.ReadFile(p)
		if err != nil {
			return "", fmt.Errorf("masked %s: %v", p, err)
		}
		if sha(string(raw)) != cfg.StubSHA256 {
			return "", fmt.Errorf("%s is not the refusal stub", p)
		}
		if out, err := exec.Command(p).CombinedOutput(); err == nil {
			return "", fmt.Errorf("%s ran successfully: %q", p, out)
		}
	}
	return fmt.Sprintf("%d host tools replaced by a refusing stub", len(cfg.MaskedTools)), nil
}

func homeFresh(cfg *Config) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home != cfg.Home {
		return "", fmt.Errorf("home is %q (%v), want %s", home, err, cfg.Home)
	}
	allowed := map[string]bool{}
	for _, name := range cfg.HomeAllowed {
		allowed[name] = true
	}
	entries, err := os.ReadDir(home)
	if err != nil {
		return "", err
	}
	for _, e := range entries {
		if !allowed[e.Name()] {
			return "", fmt.Errorf("home holds %s", e.Name())
		}
	}
	for _, sub := range []string{".t3", ".claude", ".codex", ".config/gh", ".ssh"} {
		if _, err := os.Lstat(filepath.Join(home, sub)); !errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("home holds %s", sub)
		}
	}
	return fmt.Sprintf("%s with %d entries", home, len(entries)), nil
}

func statusField(name string) (string, error) {
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return "", err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		if k, v, ok := strings.Cut(s.Text(), ":"); ok && k == name {
			return strings.TrimSpace(v), nil
		}
	}
	return "", fmt.Errorf("%s not in /proc/self/status", name)
}

func noPrivileges(cfg *Config) (string, error) {
	nnp, err := statusField("NoNewPrivs")
	if err != nil || nnp != "1" {
		return "", fmt.Errorf("NoNewPrivs %q (%v)", nnp, err)
	}
	for _, field := range []string{"CapEff", "CapPrm", "CapAmb"} {
		v, err := statusField(field)
		if err != nil || strings.Trim(v, "0") != "" {
			return "", fmt.Errorf("%s %q (%v)", field, v, err)
		}
	}
	if os.Geteuid() == 0 {
		return "", errors.New("running as uid 0")
	}
	return "no capabilities, no_new_privs, uid " + strconv.Itoa(os.Geteuid()), nil
}

// A Go process is multithreaded, and unshare(CLONE_NEWUSER) always fails
// there with EINVAL, so the check runs util-linux unshare, which is single
// threaded, and requires it to fail for lack of permission or quota.
func nestedUserNamespaceRefused(cfg *Config) (string, error) {
	out, err := exec.Command("/usr/bin/unshare", "--user", "--map-root-user", "/bin/true").CombinedOutput()
	if err == nil {
		return "", errors.New("unshare --user succeeded inside the sandbox")
	}
	text := strings.TrimSpace(string(out))
	if !strings.Contains(text, "No space left on device") && !strings.Contains(text, "Operation not permitted") {
		return "", fmt.Errorf("unshare --user failed for another reason (%v): %q", err, text)
	}
	return "unshare --user: " + text, nil
}

func validatorsDetectPlantedLeaks(cfg *Config) (string, error) {
	want := map[string]Mount{"/work/src": {Root: "/scratch/src", FSType: "ext4"}}
	planted := "1 0 0:1 / / rw - tmpfs tmpfs rw\n2 1 8:2 /scratch/src /work/src rw - ext4 /dev/x rw\n3 1 8:2 /home/user /home/user ro - ext4 /dev/x rw\n"
	mounts, err := parseMountinfo(planted)
	if err != nil {
		return "", err
	}
	want["/"] = Mount{Root: "/", FSType: "tmpfs"}
	if validateMounts(mounts, want) == nil {
		return "", errors.New("mount validator accepted a planted home bind")
	}
	if validateMounts(mounts[:2], want) != nil {
		return "", errors.New("mount validator refused the clean table")
	}
	if validateMounts(mounts[:1], want) == nil {
		return "", errors.New("mount validator accepted a missing mount")
	}
	if environHasCanary([]string{"X=" + "planted"}, "NAME", sha("planted")) == nil || environHasCanary([]string{"NAME=1"}, "NAME", "") == nil {
		return "", errors.New("environment validator missed a planted canary")
	}
	if environHasCanary([]string{"X=clean"}, "NAME", sha("planted")) != nil {
		return "", errors.New("environment validator flagged a clean environment")
	}
	return "planted mount, missing mount and environment leaks detected", nil
}

func run(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

func goToolchain(cfg *Config) (string, error) {
	if runtime.Version() != cfg.GoVersion {
		return "", fmt.Errorf("probe built by %s, want %s", runtime.Version(), cfg.GoVersion)
	}
	out, err := run("go", "version")
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(out, "go version "+cfg.GoVersion+" ") {
		return "", fmt.Errorf("go version printed %q, want %s", strings.TrimSpace(out), cfg.GoVersion)
	}
	return strings.TrimSpace(out), nil
}

func goBuildAndRun(cfg *Config) (string, error) {
	dir, err := os.MkdirTemp("", "probe-build-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	files := map[string]string{
		"go.mod":  "module probebuild\n\ngo 1.27\n",
		"main.go": "package main\n\nimport \"fmt\"\n\nfunc main() { fmt.Println(\"built inside the sandbox\") }\n",
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			return "", err
		}
	}
	cmd := exec.Command("go", "build", "-o", "probe-bin", ".")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build: %v: %s", err, out)
	}
	out, err := run(filepath.Join(dir, "probe-bin"))
	if err != nil || strings.TrimSpace(out) != "built inside the sandbox" {
		return "", fmt.Errorf("built binary printed %q (%v)", out, err)
	}
	return "compiled and ran a program", nil
}

func gremlinsVersion(cfg *Config) (string, error) {
	if cfg.Gremlins == "" {
		return "no gremlins in this stage", nil
	}
	out, err := run("go", "version", "-m", cfg.Gremlins)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) >= 4 && f[0] == "mod" && f[1] == cfg.GremlinsModule {
			if f[2] != cfg.GremlinsVersion || f[3] != cfg.GremlinsSum {
				return "", fmt.Errorf("gremlins is %s %s, want %s %s", f[2], f[3], cfg.GremlinsVersion, cfg.GremlinsSum)
			}
			return fmt.Sprintf("%s %s %s", f[1], f[2], f[3]), nil
		}
	}
	return "", fmt.Errorf("%s is not a build of %s", cfg.Gremlins, cfg.GremlinsModule)
}

func loopbackHTTP(cfg *Config) (string, error) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "fixture") }))
	defer server.Close()
	resp, err := http.Get(server.URL)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "fixture" {
		return "", fmt.Errorf("fixture answered %q", body)
	}
	return "in-sandbox HTTP fixture on " + server.Listener.Addr().String(), nil
}

func fakeCLI(cfg *Config) (string, error) {
	dir, err := os.MkdirTemp("", "probe-cli-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "fake-cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho \"fake cli $1\"\n"), 0o755); err != nil {
		return "", err
	}
	out, err := run(path, "ok")
	if err != nil || strings.TrimSpace(out) != "fake cli ok" {
		return "", fmt.Errorf("fake CLI printed %q (%v)", out, err)
	}
	return "ran a fake CLI from a temp folder", nil
}

func writableDirs(cfg *Config) (string, error) {
	if os.TempDir() != cfg.TempDir {
		return "", fmt.Errorf("TMPDIR is %s, want %s", os.TempDir(), cfg.TempDir)
	}
	for _, dir := range append([]string{cfg.TempDir, cfg.Home}, cfg.WritableDirs...) {
		f, err := os.CreateTemp(dir, "probe-")
		if err != nil {
			return "", fmt.Errorf("%s: %v", dir, err)
		}
		name := f.Name()
		_, werr := f.WriteString("probe")
		f.Close()
		got, rerr := os.ReadFile(name)
		os.Remove(name)
		if werr != nil || rerr != nil || string(got) != "probe" {
			return "", fmt.Errorf("%s: write %v read %v", dir, werr, rerr)
		}
	}
	return fmt.Sprintf("%d writable directories", 2+len(cfg.WritableDirs)), nil
}
