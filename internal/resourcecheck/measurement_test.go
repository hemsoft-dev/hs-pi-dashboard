package resourcecheck

import "testing"

func TestViolationsRejectLostMeasurement(t *testing.T) {
	baseline := Reading{Goroutines: 2, Threads: 2, HeapBytes: 1024, Handles: 10, Children: 0}
	final := baseline
	final.Handles = Unmeasured
	if len((Bounds{}).Violations(baseline, final)) == 0 {
		t.Fatal("loss of a previously measured handle count is silently accepted")
	}
}
