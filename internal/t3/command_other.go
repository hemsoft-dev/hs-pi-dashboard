//go:build !windows

package t3

import osexec "os/exec"

func hideCommandWindow(*osexec.Cmd) {}
