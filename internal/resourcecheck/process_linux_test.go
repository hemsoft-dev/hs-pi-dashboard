package resourcecheck

import "testing"

func TestParentPIDReadsFieldAfterCommand(t *testing.T) {
	for stat, want := range map[string]int{
		"123 (go) S 456 123 0":            456,
		"123 (a) b) (c) Z 789 1 1":        789, // a command name with ") "
		"123 (go S 456":                   -1,
		"123 (go) S":                      -1,
		"123 (go) S notanumber 123 0 0 0": -1,
	} {
		if got := parentPID(stat); got != want {
			t.Errorf("parentPID(%q)=%d, want %d", stat, got, want)
		}
	}
}
