package usage

import (
	"github.com/HemSoft/hs-pi-dashboard/internal/backgroundcmd"
	"os/exec"
)

func credentialCommand(script string) *exec.Cmd {
	return backgroundcmd.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass", "-Command", script)
}
