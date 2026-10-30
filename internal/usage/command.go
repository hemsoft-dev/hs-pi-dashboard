package usage

import "os/exec"

func credentialCommand(script string) *exec.Cmd {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-ExecutionPolicy", "Bypass", "-Command", script)
	hideCredentialWindow(cmd)
	return cmd
}
