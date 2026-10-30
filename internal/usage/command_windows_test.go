package usage

import "testing"

func TestCredentialCommandDoesNotCreateConsole(t *testing.T) {
	cmd := credentialCommand("Write-Output fixture")
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&0x08000000 == 0 {
		t.Fatal("credential PowerShell must run without a console")
	}
	if cmd.Path == "" || cmd.Args[len(cmd.Args)-1] != "Write-Output fixture" {
		t.Fatal("script arguments changed")
	}
}
