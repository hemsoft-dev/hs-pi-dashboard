// Package githubcli locates the installed GitHub CLI for service processes.
package githubcli

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// Binary respects an explicit host-local override before PATH and native install
// locations. This also works under launchd, which has a minimal default PATH.
func Binary() string {
	if configured := strings.TrimSpace(os.Getenv("HS_PI_DASHBOARD_GH_BIN")); configured != "" {
		return configured
	}
	if binary, err := exec.LookPath("gh"); err == nil {
		return binary
	}
	home, _ := os.UserHomeDir()
	paths := []string{filepath.Join(home, ".local", "bin", "gh"), filepath.Join(home, "bin", "gh"), "/opt/homebrew/bin/gh", "/usr/local/bin/gh", "/usr/bin/gh"}
	if runtime.GOOS == "windows" {
		paths = []string{filepath.Join(os.Getenv("ProgramFiles"), "GitHub CLI", "gh.exe"), filepath.Join(os.Getenv("LOCALAPPDATA"), "Microsoft", "WinGet", "Links", "gh.exe")}
	}
	for _, p := range paths {
		if info, err := os.Stat(p); err == nil && info.Mode().IsRegular() {
			return p
		}
	}
	return "gh"
}
