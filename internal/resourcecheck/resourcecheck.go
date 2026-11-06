// Package resourcecheck reads the resources a long-running Go process can
// retain, so tests can show that repeated collection cycles return to a steady
// state. It reads goroutines, OS threads, live heap after a forced GC, open
// file descriptors (Windows: process handles) and live child processes.
//
// See docs/resource-growth.md for how the agent and fleet tests use it and why
// their bounds are what they are.
package resourcecheck

import (
	"fmt"
	"runtime"
	"runtime/pprof"
	"time"
)

// Unmeasured marks a resource this platform cannot read.
const Unmeasured = -1

// Reading is one sample of the current process.
type Reading struct {
	Goroutines int
	// Threads counts the Go runtime's OS threads. The runtime never releases
	// one unless a goroutine exits while locked to it, so the count rises to
	// the peak number of goroutines blocked in system calls at once and stays.
	Threads int
	// HeapBytes is runtime.MemStats.HeapAlloc right after two forced GCs: the
	// live heap. Two cycles also empty sync.Pool victim caches.
	HeapBytes int64
	// Handles counts open file descriptors on Unix and kernel handles on
	// Windows, or is Unmeasured.
	Handles int
	// Children counts live direct child processes, including exited children
	// that were never waited for (Linux zombies), or is Unmeasured.
	Children int
}

func (r Reading) String() string {
	return fmt.Sprintf("goroutines=%d threads=%d heap=%dKiB handles=%s children=%s",
		r.Goroutines, r.Threads, r.HeapBytes>>10, count(r.Handles), count(r.Children))
}

func count(n int) string {
	if n == Unmeasured {
		return "unmeasured"
	}
	return fmt.Sprint(n)
}

// Measure forces garbage collection and samples the process.
func Measure() Reading {
	runtime.GC()
	runtime.GC()
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	r := Reading{
		Goroutines: runtime.NumGoroutine(),
		Threads:    pprof.Lookup("threadcreate").Count(),
		HeapBytes:  int64(stats.HeapAlloc),
	}
	// Handles before children: the child scan opens and closes its own handle.
	r.Handles = openHandles()
	r.Children = childProcesses()
	return r
}

// Bounds is the growth over a baseline each resource may show and still count
// as steady.
type Bounds struct {
	Goroutines int
	Threads    int
	HeapBytes  int64
	// Handles is the growth allowed besides the handles new OS threads hold;
	// see HandlesPerThread.
	Handles  int
	Children int
}

// Violation is one resource that grew past its bound.
type Violation struct {
	Resource        string
	Baseline, Final int64
	Bound           int64
}

func (v Violation) String() string {
	if v.Baseline == Unmeasured || v.Final == Unmeasured {
		return fmt.Sprintf("%s measurement unavailable: baseline %d, final %d", v.Resource, v.Baseline, v.Final)
	}
	return fmt.Sprintf("%s grew by %d (bound %d): baseline %d, final %d",
		v.Resource, v.Final-v.Baseline, v.Bound, v.Baseline, v.Final)
}

// Violations lists every resource in final that grew past its bound over
// baseline. Resources unmeasured on both readings are unsupported and skipped.
// Losing a previously available measurement, or having no usable baseline,
// is a qualification failure rather than evidence of steady resources.
//
// Each OS thread the runtime adds holds HandlesPerThread handles of its own,
// so the handle bound grows by that much per new thread. Thread growth has its
// own bound, so this cannot hide a thread leak.
func (b Bounds) Violations(baseline, final Reading) []Violation {
	var out []Violation
	check := func(name string, base, now, bound int64) {
		if base == Unmeasured && now == Unmeasured {
			return
		}
		if base == Unmeasured || now == Unmeasured {
			out = append(out, Violation{Resource: name, Baseline: base, Final: now, Bound: bound})
			return
		}
		if now-base > bound {
			out = append(out, Violation{Resource: name, Baseline: base, Final: now, Bound: bound})
		}
	}
	newThreads := max(0, final.Threads-baseline.Threads)
	check("goroutines", int64(baseline.Goroutines), int64(final.Goroutines), int64(b.Goroutines))
	check("OS threads", int64(baseline.Threads), int64(final.Threads), int64(b.Threads))
	check("live heap bytes", baseline.HeapBytes, final.HeapBytes, b.HeapBytes)
	check(HandleName, int64(baseline.Handles), int64(final.Handles), int64(b.Handles+HandlesPerThread*newThreads))
	check("child processes", int64(baseline.Children), int64(final.Children), int64(b.Children))
	return out
}

// Baseline samples the process several times and keeps each resource's lowest
// reading. A goroutine or connection still winding down from warm-up would
// otherwise raise the baseline and hide that much later growth.
func Baseline(samples int, gap time.Duration) Reading {
	low := Measure()
	for i := 1; i < samples; i++ {
		time.Sleep(gap)
		r := Measure()
		low.Goroutines = min(low.Goroutines, r.Goroutines)
		low.Threads = min(low.Threads, r.Threads)
		low.HeapBytes = min(low.HeapBytes, r.HeapBytes)
		low.Handles = lowest(low.Handles, r.Handles)
		low.Children = lowest(low.Children, r.Children)
	}
	return low
}

func lowest(a, b int) int {
	if a == Unmeasured || b == Unmeasured {
		return Unmeasured
	}
	return min(a, b)
}

// Settle measures until every resource is within bounds of baseline or the
// timeout passes, and returns the last reading with its violations.
//
// Teardown the code under test has already started finishes asynchronously:
// a closed connection's goroutines exit, a server notices the peer left, the
// OS releases a reaped child. Waiting for that is not slack in the bound. A
// retained resource never returns, so it is still reported at the timeout.
func Settle(baseline Reading, bounds Bounds, timeout time.Duration) (Reading, []Violation) {
	deadline := time.Now().Add(timeout)
	for {
		final := Measure()
		violations := bounds.Violations(baseline, final)
		if len(violations) == 0 || !time.Now().Before(deadline) {
			return final, violations
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Plan is one steady-state check: warm-up cycles fill caches, pools and
// keep-alive connections, then Cycles more run between the baseline and the
// final reading.
type Plan struct {
	// Warmup cycles run before the baseline. While a block of Warmup cycles
	// still adds OS threads or handles, warm-up runs another block, up to
	// MaxWarmup cycles in all: the Go runtime and Windows fill pools that live
	// as long as the process (threads, socket state) during the first bursts.
	// A leak never stops growing, so it reaches MaxWarmup and still shows in
	// the measured cycles.
	Warmup, MaxWarmup int
	Cycles            int
	Bounds            Bounds
	// SettleTimeout bounds the wait for teardown already in progress after
	// the last cycle; see Settle.
	SettleTimeout time.Duration
}

// Result reports one Plan run.
type Result struct {
	Baseline, Final Reading
	Violations      []Violation
	WarmupCycles    int
	Elapsed         time.Duration // measured cycles only
}

// warmupMeasure takes the readings that decide whether warm-up goes on. Tests
// script it: the Go runtime starts OS threads of its own at any time.
var warmupMeasure = Measure

// Run calls cycle for every warm-up and measured cycle, telling it which, and
// stops at the first error.
func (p Plan) Run(cycle func(measured bool) error) (Result, error) {
	var result Result
	before := warmupMeasure()
	for {
		for i := 0; i < p.Warmup; i++ {
			if err := cycle(false); err != nil {
				return result, fmt.Errorf("warm-up cycle %d: %w", result.WarmupCycles, err)
			}
			result.WarmupCycles++
		}
		after := warmupMeasure()
		if !poolsGrew(before, after) || result.WarmupCycles+p.Warmup > p.MaxWarmup {
			break
		}
		before = after
	}
	result.Baseline = Baseline(5, 50*time.Millisecond)
	started := time.Now()
	for i := 0; i < p.Cycles; i++ {
		if err := cycle(true); err != nil {
			return result, fmt.Errorf("cycle %d: %w", i, err)
		}
	}
	result.Elapsed = time.Since(started)
	result.Final, result.Violations = Settle(result.Baseline, p.Bounds, p.SettleTimeout)
	return result, nil
}

func poolsGrew(before, after Reading) bool {
	return after.Threads > before.Threads || (before.Handles != Unmeasured && after.Handles > before.Handles)
}
