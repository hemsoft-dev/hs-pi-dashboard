package agent

import (
	"os"
	"reflect"
	"testing"

	"github.com/HemSoft/hs-pi-dashboard/internal/usage"
)

// productionVaultReader is usage.ReadAntigravityVault before TestMain swaps it.
var productionVaultReader func() string

// The usage stores these tests start fetch Antigravity, which falls back to
// Windows Credential Manager. Answer "no entry", as on a hosted runner, so no
// test reads the host's credential.
func TestMain(m *testing.M) {
	productionVaultReader = usage.ReadAntigravityVault
	usage.ReadAntigravityVault = func() string { return "" }
	os.Exit(m.Run())
}

func TestUsageStoresCannotReachHostVault(t *testing.T) {
	if productionVaultReader == nil || reflect.ValueOf(usage.ReadAntigravityVault).Pointer() == reflect.ValueOf(productionVaultReader).Pointer() {
		t.Fatal("agent tests can reach the real Credential Manager reader")
	}
}
