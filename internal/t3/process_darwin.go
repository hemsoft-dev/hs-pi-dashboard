package t3

import (
	"context"
	"errors"
	"net/url"
	"os"
	osexec "os/exec"
	"strconv"
	"strings"
	"time"
)

func processCreated(ctx context.Context, pid int) (time.Time, error) {
	cmd := osexec.CommandContext(ctx, "/bin/ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "TZ=UTC", "LC_ALL=C")
	data, err := cmd.Output()
	if err != nil {
		return time.Time{}, err
	}
	return time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(strings.Fields(string(data)), " "), time.UTC)
}

func verifyRuntimeProcess(ctx context.Context, pid int, started time.Time, origin *url.URL) error {
	created, err := processCreated(ctx, pid)
	if err != nil || !sameRuntimeStart(created, started) {
		return errors.New("runtime process changed")
	}
	port, err := runtimePort(origin)
	if err != nil {
		return err
	}
	// Output contains only PIDs, never process arguments or credentials.
	cmd := osexec.CommandContext(ctx, "/usr/sbin/lsof", "-t", "-a", "-p", strconv.Itoa(pid), "-iTCP:"+strconv.Itoa(port), "-sTCP:LISTEN")
	data, err := cmd.Output()
	if err != nil || strings.TrimSpace(string(data)) != strconv.Itoa(pid) {
		return errors.New("runtime listener changed")
	}
	return nil
}
