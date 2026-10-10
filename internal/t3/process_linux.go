package t3

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func processCreated(pid int) (time.Time, error) {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return time.Time{}, err
	}
	fields := strings.Fields(string(data)[strings.LastIndex(string(data), ")")+1:])
	if len(fields) < 20 || fields[0] == "Z" {
		return time.Time{}, errors.New("process unavailable")
	}
	ticks, err := strconv.ParseInt(fields[19], 10, 64)
	if err != nil {
		return time.Time{}, err
	}
	boot, err := os.ReadFile("/proc/stat")
	if err != nil {
		return time.Time{}, err
	}
	for _, line := range strings.Split(string(boot), "\n") {
		if value, ok := strings.CutPrefix(line, "btime "); ok {
			seconds, err := strconv.ParseInt(value, 10, 64)
			// Linux exposes process times using the stable USER_HZ=100 ABI.
			return time.Unix(seconds, ticks*int64(time.Second)/100), err
		}
	}
	return time.Time{}, errors.New("process start unavailable")
}

func verifyRuntimeProcess(ctx context.Context, pid int, started time.Time, origin *url.URL) error {
	created, err := processCreated(pid)
	if err != nil || !sameRuntimeStart(created, started) {
		return errors.New("runtime process changed")
	}
	port, err := runtimePort(origin)
	if err != nil {
		return err
	}
	// Match the discovered process's socket inode, not merely a live PID.
	fds, err := os.ReadDir(filepath.Join("/proc", strconv.Itoa(pid), "fd"))
	if err != nil {
		return err
	}
	sockets := map[string]bool{}
	for _, fd := range fds {
		link, _ := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "fd", fd.Name()))
		if strings.HasPrefix(link, "socket:[") {
			sockets[strings.TrimSuffix(strings.TrimPrefix(link, "socket:["), "]")] = true
		}
	}
	for _, table := range []string{"tcp", "tcp6"} {
		data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "net", table))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 10 || fields[3] != "0A" || !sockets[fields[9]] {
				continue
			}
			_, encoded, ok := strings.Cut(fields[1], ":")
			n, err := strconv.ParseInt(encoded, 16, 32)
			if ok && err == nil && int(n) == port {
				return ctx.Err()
			}
		}
	}
	return errors.New("runtime listener changed")
}
