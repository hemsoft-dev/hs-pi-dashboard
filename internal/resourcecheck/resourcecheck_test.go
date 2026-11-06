package resourcecheck

import (
	"errors"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/HemSoft/hs-pi-dashboard/internal/backgroundcmd"
)

const childEnv = "HS_PI_DASHBOARD_RESOURCECHECK_CHILD"

// A child started by these tests is this test binary, waiting for stdin to
// close. It exits before testing parses any flag.
func init() {
	if os.Getenv(childEnv) == "wait" {
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	}
}

func platformMeasuresProcesses() bool { return runtime.GOOS == "linux" || runtime.GOOS == "windows" }

func TestMeasureReadsThisProcess(t *testing.T) {
	r := Measure()
	if r.Goroutines < 1 || r.Threads < 1 || r.HeapBytes <= 0 {
		t.Fatalf("reading=%+v", r)
	}
	if platformMeasuresProcesses() && (r.Handles <= 0 || r.Children < 0) {
		t.Fatalf("reading=%+v on %s", r, runtime.GOOS)
	}
	if !strings.Contains(r.String(), "goroutines=") {
		t.Fatalf("String()=%q", r.String())
	}
}

// Each reader must see a retained resource of its kind, and Settle must
// report it until it is released, then return within bounds.
func TestSettleReportsRetainedResourcesUntilReleased(t *testing.T) {
	// Warm up: the first process start and file open load one-time state
	// (Windows keeps about a dozen handles for it).
	if err := retainAndRelease(); err != nil {
		t.Fatal(err)
	}
	baseline := Baseline(3, 10*time.Millisecond)

	stop := make(chan struct{})
	go func() { <-stop }()
	retained := make([]byte, 4<<20)
	// More files than the handles credited to the few threads the child's
	// wait may add (HandlesPerThread each).
	var files []*os.File
	closeFiles := func() {
		for _, f := range files {
			f.Close()
		}
	}
	defer closeFiles()
	for i := 0; i < 50; i++ {
		file, err := os.Open(os.Args[0])
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, file)
	}
	child, err := startChild()
	if err != nil {
		t.Fatal(err)
	}

	_, violations := Settle(baseline, Bounds{}, 200*time.Millisecond)
	reported := map[string]bool{}
	for _, v := range violations {
		reported[v.Resource] = true
		if !strings.Contains(v.String(), "grew by") {
			t.Fatalf("String()=%q", v.String())
		}
	}
	want := []string{"goroutines", "live heap bytes"}
	if baseline.Handles != Unmeasured {
		want = append(want, HandleName)
	}
	if baseline.Children != Unmeasured {
		want = append(want, "child processes")
	}
	for _, resource := range want {
		if !reported[resource] {
			t.Errorf("retained %s not reported: %v", resource, violations)
		}
	}
	runtime.KeepAlive(retained)

	close(stop)
	retained = nil
	closeFiles()
	files = nil
	if err := child.stop(); err != nil {
		t.Fatal(err)
	}
	// Released: the reading returns to baseline, apart from runtime
	// bookkeeping (the exited goroutine's descriptor stays in the heap).
	final, violations := Settle(baseline, Bounds{Goroutines: 1, Threads: 4, HeapBytes: 256 << 10, Handles: 2}, 5*time.Second)
	if len(violations) > 0 {
		t.Fatalf("baseline %s, final %s: %v", baseline, final, violations)
	}
}

func TestViolationsRequireMeasurementsAndCreditThreads(t *testing.T) {
	base := Reading{Goroutines: 10, Threads: 5, HeapBytes: 100, Handles: Unmeasured, Children: 0}
	now := Reading{Goroutines: 10, Threads: 5, HeapBytes: 100, Handles: 500, Children: Unmeasured}
	if v := (Bounds{}).Violations(base, now); len(v) != 2 {
		t.Fatalf("missing measurements accepted: %v", v)
	}
	now.Children = base.Children
	base.Handles = 100
	now.Handles = 100 + 3*HandlesPerThread + 1
	now.Threads = base.Threads + 3
	v := Bounds{Threads: 3}.Violations(base, now)
	if len(v) != 1 || v[0].Resource != HandleName || v[0].Bound != int64(3*HandlesPerThread) {
		t.Fatalf("violations=%v", v)
	}
	now.Handles--
	if v := (Bounds{Threads: 3}).Violations(base, now); len(v) != 0 {
		t.Fatalf("handles of new threads reported: %v", v)
	}
	now.Threads++ // the thread bound still applies on its own
	if v := (Bounds{Threads: 3}).Violations(base, now); len(v) != 1 || v[0].Resource != "OS threads" {
		t.Fatalf("violations=%v", v)
	}
}

// HandlesPerThread must cover what a new OS thread really holds here,
// including what Windows adds once the thread has used a socket, as the
// collectors' threads do.
func TestHandlesPerThread(t *testing.T) {
	if Measure().Handles == Unmeasured {
		t.Skipf("handles unmeasured on %s", runtime.GOOS)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	// Warm up: the process's first connection loads one-time socket state.
	warm, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	warm.Close()
	for _, useSocket := range []bool{false, true} {
		before := Baseline(3, 20*time.Millisecond)
		stop := make(chan struct{})
		started := make(chan error)
		const n = 20
		for i := 0; i < n; i++ {
			go func() {
				runtime.LockOSThread() // exits with the goroutine, ending the thread
				var err error
				if useSocket {
					var conn net.Conn
					if conn, err = net.Dial("tcp", listener.Addr().String()); err == nil {
						err = conn.Close()
					}
				}
				started <- err
				<-stop
			}()
		}
		for i := 0; i < n; i++ {
			if err := <-started; err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(100 * time.Millisecond) // the accepting side closes its ends
		during := Measure()
		close(stop)
		threads, handles := during.Threads-before.Threads, during.Handles-before.Handles
		t.Logf("socket=%v: %d new threads, %d new handles", useSocket, threads, handles)
		// An earlier run in this process can leave idle threads to reuse.
		if threads < n/2 {
			t.Fatalf("only %d new threads", threads)
		}
		// Measurement noise of half a handle per thread: the accepting side
		// closes its sockets asynchronously, and a reused idle thread gains
		// socket state without counting as new.
		if 2*handles > (2*HandlesPerThread+1)*threads+8 {
			t.Fatalf("%d threads hold %d handles, more than %d each", threads, handles, HandlesPerThread)
		}
		time.Sleep(100 * time.Millisecond) // the locked threads exit
	}
}

func TestPlanRunReportsCycleErrors(t *testing.T) {
	boom := errors.New("boom")
	calls := 0
	_, err := Plan{Warmup: 2, Cycles: 3}.Run(func(measured bool) error {
		calls++
		if calls == 2 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "warm-up cycle 1") || calls != 2 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	measured := 0
	_, err = Plan{Warmup: 1, Cycles: 3}.Run(func(m bool) error {
		if m {
			measured++
		}
		if measured == 2 {
			return boom
		}
		return nil
	})
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "cycle 1") {
		t.Fatalf("err=%v", err)
	}
	loose := Bounds{Goroutines: 100, Threads: 100, HeapBytes: 1 << 30, Handles: 100, Children: 100}
	result, err := Plan{Warmup: 1, Cycles: 2, Bounds: loose}.Run(func(bool) error { return nil })
	if err != nil || len(result.Violations) != 0 || result.Final.Goroutines == 0 || result.WarmupCycles != 1 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

// Warm-up repeats while a block still adds handles, up to MaxWarmup, and
// stops at the first block that adds none.
func TestPlanWarmupWaitsForPoolsToFill(t *testing.T) {
	if Measure().Handles == Unmeasured {
		t.Skipf("handles unmeasured on %s", runtime.GOOS)
	}
	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	open := func(limit int) func(bool) error {
		return func(measured bool) error {
			if measured || len(files) >= limit {
				return nil
			}
			f, err := os.Open(os.Args[0])
			files = append(files, f)
			return err
		}
	}
	loose := Bounds{Goroutines: 100, Threads: 100, HeapBytes: 1 << 30, Handles: 100, Children: 100}
	// Growth for 3 blocks, then flat: 4 blocks of warm-up.
	result, err := Plan{Warmup: 2, MaxWarmup: 20, Cycles: 1, Bounds: loose}.Run(open(6))
	if err != nil || result.WarmupCycles != 8 {
		t.Fatalf("warm-up %d cycles, err=%v", result.WarmupCycles, err)
	}
	// Growth that never stops ends at MaxWarmup.
	result, err = Plan{Warmup: 2, MaxWarmup: 6, Cycles: 1, Bounds: loose}.Run(open(1000))
	if err != nil || result.WarmupCycles != 6 {
		t.Fatalf("warm-up %d cycles, err=%v", result.WarmupCycles, err)
	}
}

func retainAndRelease() error {
	file, err := os.Open(os.Args[0])
	if err != nil {
		return err
	}
	defer file.Close()
	c, err := startChild()
	if err != nil {
		return err
	}
	return c.stop()
}

type child struct {
	stdin io.Closer
	wait  func() error
}

func startChild() (*child, error) {
	cmd := backgroundcmd.Command(os.Args[0])
	// GORACE: a race-instrumented child would otherwise sleep 1 s on exit.
	cmd.Env = append(os.Environ(), childEnv+"=wait", "GORACE=atexit_sleep_ms=0")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &child{stdin: stdin, wait: cmd.Wait}, nil
}

func (c *child) stop() error {
	c.stdin.Close()
	return c.wait()
}
