package t3

import (
	"errors"
	"net/url"
	"strconv"
	"time"
)

func runtimePort(origin *url.URL) (int, error) {
	port := origin.Port()
	if port == "" {
		port = "80"
		if origin.Scheme == "https" {
			port = "443"
		}
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return 0, errors.New("invalid runtime port")
	}
	return n, nil
}

// T3 records server startup after creating the process. Bound that delay so
// PID reuse cannot turn an old runtime record into proof of a new process.
func sameRuntimeStart(created, started time.Time) bool {
	delay := started.Sub(created)
	return !created.IsZero() && delay >= -2*time.Second && delay <= 2*time.Minute
}
