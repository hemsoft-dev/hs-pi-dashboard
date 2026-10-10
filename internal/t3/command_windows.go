package t3

import (
	osexec "os/exec"
	"syscall"
)

func hideCommandWindow(cmd *osexec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
