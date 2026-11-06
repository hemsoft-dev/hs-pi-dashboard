package t3

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"

	"github.com/HemSoft/hs-pi-dashboard/internal/backgroundcmd"
)

// The fixture child is this test binary re-run with fixtureEnv set. It owns one
// loopback listener and exits when its stdin closes, so every process and port
// these tests check belongs to the test run.
const (
	fixtureEnv      = "HS_PI_DASHBOARD_T3_LISTENER_FIXTURE"
	fixtureDeadline = 30 * time.Second
	verifyDeadline  = 10 * time.Second
)

func TestRuntimeListenerFixtureProcess(t *testing.T) {
	if os.Getenv(fixtureEnv) != "1" {
		t.Skip("fixture child only")
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		fmt.Println("error", err)
		os.Exit(2)
	}
	fmt.Println(listener.Addr().(*net.TCPAddr).Port)
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, os.Stdin)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(fixtureDeadline):
	}
	listener.Close()
	os.Exit(0)
}

type listenerFixture struct {
	pid     int
	port    int
	created time.Time
	stop    func()
}

func startListenerFixture(t *testing.T) listenerFixture {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), fixtureDeadline)
	cmd := backgroundcmd.CommandContext(ctx, os.Args[0], "-test.run=^TestRuntimeListenerFixtureProcess$", "-test.count=1")
	cmd.Env = append(os.Environ(), fixtureEnv+"=1")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if stopped {
			return
		}
		stopped = true
		stdin.Close()
		done := make(chan struct{})
		go func() {
			_ = cmd.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(verifyDeadline):
			_ = cmd.Process.Kill()
			<-done
		}
		cancel()
	}
	t.Cleanup(stop)

	line := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		scanner.Scan()
		line <- strings.TrimSpace(scanner.Text())
		_, _ = io.Copy(io.Discard, stdout)
	}()
	var text string
	select {
	case text = <-line:
	case <-time.After(verifyDeadline):
		t.Fatal("fixture child did not report its listener")
	}
	port, err := strconv.Atoi(text)
	if err != nil || port < 1 || port > 65535 {
		t.Fatalf("fixture child reported %q", text)
	}
	return listenerFixture{pid: cmd.Process.Pid, port: port, created: processCreatedWindows(t, cmd.Process.Pid), stop: stop}
}

func processCreatedWindows(t *testing.T, pid int) time.Time {
	t.Helper()
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		t.Fatalf("open process %d: %v", pid, err)
	}
	defer windows.CloseHandle(handle)
	var created, exited, kernel, user windows.Filetime
	if err := windows.GetProcessTimes(handle, &created, &exited, &kernel, &user); err != nil {
		t.Fatalf("process times %d: %v", pid, err)
	}
	return time.Unix(0, created.Nanoseconds())
}

func listen(t *testing.T, network, address string) (net.Listener, *url.URL) {
	t.Helper()
	listener, err := net.Listen(network, address)
	if err != nil {
		t.Skipf("loopback %s unavailable: %v", network, err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener, &url.URL{Scheme: "http", Host: listener.Addr().String()}
}

func portOrigin(port int) *url.URL {
	return &url.URL{Scheme: "http", Host: net.JoinHostPort("127.0.0.1", strconv.Itoa(port))}
}

// verifyWithin bounds each check so a hung IP Helper call fails the test.
func verifyWithin(t *testing.T, ctx context.Context, pid int, started time.Time, origin *url.URL) error {
	t.Helper()
	result := make(chan error, 1)
	go func() { result <- verifyRuntimeProcess(ctx, pid, started, origin) }()
	select {
	case err := <-result:
		return err
	case <-time.After(verifyDeadline):
		t.Fatalf("verifyRuntimeProcess(%d, %s) did not return within %s", pid, origin, verifyDeadline)
		return nil
	}
}

func wantError(t *testing.T, err error, message, what string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s accepted", what)
	}
	if message != "" && err.Error() != message {
		t.Fatalf("%s: got %q, want %q", what, err, message)
	}
}

func TestVerifyRuntimeProcessAcceptsOwnedListener(t *testing.T) {
	self := os.Getpid()
	started := processCreatedWindows(t, self).Add(time.Second)
	for _, tc := range []struct{ network, address string }{
		{"tcp4", "127.0.0.1:0"},
		{"tcp6", "[::1]:0"},
	} {
		t.Run(tc.network, func(t *testing.T) {
			_, origin := listen(t, tc.network, tc.address)
			if err := verifyWithin(t, context.Background(), self, started, origin); err != nil {
				t.Fatalf("owned %s listener refused: %v", tc.network, err)
			}
		})
	}
}

func TestVerifyRuntimeProcessAcceptsFixtureChild(t *testing.T) {
	child := startListenerFixture(t)
	if err := verifyWithin(t, context.Background(), child.pid, child.created.Add(time.Second), portOrigin(child.port)); err != nil {
		t.Fatalf("fixture child's own listener refused: %v", err)
	}
}

func TestVerifyRuntimeProcessReturnsContextError(t *testing.T) {
	self := os.Getpid()
	_, origin := listen(t, "tcp4", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := verifyWithin(t, ctx, self, processCreatedWindows(t, self).Add(time.Second), origin)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled check returned %v", err)
	}
}

func TestVerifyRuntimeProcessRejectsChangedStart(t *testing.T) {
	self := os.Getpid()
	created := processCreatedWindows(t, self)
	_, origin := listen(t, "tcp4", "127.0.0.1:0")
	for name, started := range map[string]time.Time{
		"recorded long after creation": created.Add(time.Hour),
		"recorded before creation":     created.Add(-time.Minute),
	} {
		t.Run(name, func(t *testing.T) {
			wantError(t, verifyWithin(t, context.Background(), self, started, origin), "runtime process changed", "reused PID")
		})
	}
}

func TestVerifyRuntimeProcessRejectsForeignListener(t *testing.T) {
	self := os.Getpid()
	selfStarted := processCreatedWindows(t, self).Add(time.Second)
	_, selfOrigin := listen(t, "tcp4", "127.0.0.1:0")
	child := startListenerFixture(t)
	wantError(t, verifyWithin(t, context.Background(), self, selfStarted, portOrigin(child.port)),
		"runtime listener changed", "port owned by the fixture child")
	wantError(t, verifyWithin(t, context.Background(), child.pid, child.created.Add(time.Second), selfOrigin),
		"runtime listener changed", "fixture child claiming the test's port")
}

func TestVerifyRuntimeProcessRejectsClosedListener(t *testing.T) {
	self := os.Getpid()
	listener, origin := listen(t, "tcp4", "127.0.0.1:0")
	listener.Close()
	wantError(t, verifyWithin(t, context.Background(), self, processCreatedWindows(t, self).Add(time.Second), origin),
		"runtime listener changed", "closed listener")
}

func TestVerifyRuntimeProcessRejectsExitedProcess(t *testing.T) {
	child := startListenerFixture(t)
	child.stop()
	wantError(t, verifyWithin(t, context.Background(), child.pid, child.created.Add(time.Second), portOrigin(child.port)),
		"", "exited fixture child")
}

func TestVerifyRuntimeProcessRejectsInvalidPort(t *testing.T) {
	self := os.Getpid()
	origin := &url.URL{Scheme: "http", Host: "127.0.0.1:0"}
	wantError(t, verifyWithin(t, context.Background(), self, processCreatedWindows(t, self).Add(time.Second), origin),
		"invalid runtime port", "port 0")
}
