//go:build !linux && !darwin && !windows

package t3

import (
	"context"
	"errors"
	"net/url"
	"time"
)

func verifyRuntimeProcess(context.Context, int, time.Time, *url.URL) error {
	return errors.New("runtime process verification unsupported")
}
