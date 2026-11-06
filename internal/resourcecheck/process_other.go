//go:build !linux && !windows

package resourcecheck

import "os"

// HandleName names what Handles counts on this platform.
const HandleName = "open file descriptors"

// HandlesPerThread is 0: Unix threads hold no file descriptors.
const HandlesPerThread = 0

// openHandles lists /dev/fd, which macOS and the BSDs provide.
func openHandles() int {
	entries, err := os.ReadDir("/dev/fd")
	if err != nil {
		return Unmeasured
	}
	return len(entries)
}

// childProcesses is unmeasured here: listing another process's parent needs
// sysctl or ps, beyond the standard library.
func childProcesses() int { return Unmeasured }
