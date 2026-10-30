package usage

import (
	"os/exec"
	"syscall"
)

func hideCredentialWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
}
