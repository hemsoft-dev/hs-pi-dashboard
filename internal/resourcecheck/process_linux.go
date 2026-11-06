package resourcecheck

import (
	"os"
	"strconv"
	"strings"
)

// HandleName names what Handles counts on this platform.
const HandleName = "open file descriptors"

// HandlesPerThread is 0: Linux threads hold no file descriptors.
const HandlesPerThread = 0

func openHandles() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return Unmeasured
	}
	// ReadDir's own descriptor is listed while it reads and is closed after,
	// so it is a constant offset in every reading.
	return len(entries)
}

func childProcesses() int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return Unmeasured
	}
	self := os.Getpid()
	children := 0
	for _, entry := range entries {
		if _, err := strconv.Atoi(entry.Name()); err != nil {
			continue
		}
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue // exited between the listing and the read
		}
		if parentPID(string(stat)) == self {
			children++
		}
	}
	return children
}

// parentPID reads field 4 of /proc/<pid>/stat. The command name in field 2 can
// hold spaces and parentheses, so fields are counted after its last ')'.
func parentPID(stat string) int {
	end := strings.LastIndexByte(stat, ')')
	if end < 0 {
		return -1
	}
	fields := strings.Fields(stat[end+1:])
	if len(fields) < 2 {
		return -1
	}
	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return -1
	}
	return ppid
}
