//go:build !windows

package backgroundcmd

import "os/exec"

func hideWindow(*exec.Cmd) {}
