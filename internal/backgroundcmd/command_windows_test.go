package backgroundcmd

import (
	"context"
	"strings"
	"testing"
)

func TestBackgroundCommandsHaveNoConsole(t *testing.T) {
	const script = `Add-Type -Name Native -Namespace ConsoleProbe -MemberDefinition '[DllImport("kernel32.dll")] public static extern System.IntPtr GetConsoleWindow();'; [ConsoleProbe.Native]::GetConsoleWindow().ToInt64()`
	for _, contextual := range []bool{false, true} {
		cmd := Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
		if contextual {
			cmd = CommandContext(context.Background(), "powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
		}
		if cmd.SysProcAttr == nil || !cmd.SysProcAttr.HideWindow || cmd.SysProcAttr.CreationFlags&0x08000000 == 0 {
			t.Fatal("missing console suppression")
		}
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("console probe: %v", err)
		}
		if strings.TrimSpace(string(out)) != "0" {
			t.Fatalf("child has console: %q", out)
		}
	}
}
