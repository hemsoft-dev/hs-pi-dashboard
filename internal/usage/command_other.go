//go:build !windows

package usage

import "os/exec"

func hideCredentialWindow(cmd *exec.Cmd) {}
