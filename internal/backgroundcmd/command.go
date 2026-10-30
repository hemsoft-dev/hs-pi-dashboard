// Package backgroundcmd constructs subprocesses for unattended collection.
package backgroundcmd

import (
	"context"
	"os/exec"
)

// CommandContext preserves cancellation while preventing Windows console creation.
func CommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	hideWindow(cmd)
	return cmd
}

// Command prevents Windows console creation for a command without cancellation.
func Command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	hideWindow(cmd)
	return cmd
}
