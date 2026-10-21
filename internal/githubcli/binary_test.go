package githubcli

import (
	"path/filepath"
	"testing"
)

func TestExplicitServiceBinary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gh-custom")
	t.Setenv("HS_PI_DASHBOARD_GH_BIN", path)
	if Binary() != path {
		t.Fatal("service override ignored")
	}
}
