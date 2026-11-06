package usage

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// productionVaultReader is ReadAntigravityVault as the package builds it,
// captured before TestMain installs the guard. Tests compare it, never call it.
var productionVaultReader func() string

// hostVaultReads counts reads that reached the guard: a Fetcher without a
// fixture reader asking for the host's vault entry.
var hostVaultReads atomic.Int64

// installHostVaultGuard keeps package tests away from host credentials. The
// default vault reader answers "no entry", as on a hosted runner, instead of
// calling Credential Manager, and a credentials-file override in the
// developer's environment is dropped.
func installHostVaultGuard() {
	productionVaultReader = ReadAntigravityVault
	ReadAntigravityVault = hostVaultGuard
	os.Unsetenv("ANTIGRAVITY_CREDENTIALS_FILE")
}

func hostVaultGuard() string {
	hostVaultReads.Add(1)
	return ""
}

func funcPointer(fn any) uintptr { return reflect.ValueOf(fn).Pointer() }

func TestHostVaultGuardReplacesCredentialManager(t *testing.T) {
	if productionVaultReader == nil || funcPointer(productionVaultReader) != funcPointer(credManagerAntigravity) {
		t.Fatal("the production vault reader is not credManagerAntigravity")
	}
	if funcPointer(ReadAntigravityVault) != funcPointer(hostVaultGuard) {
		t.Fatal("package tests can reach the real Credential Manager reader")
	}
	if !strings.Contains(credManagerScript, "CredRead('gemini:antigravity', 1, 0, [ref]$ptr)") {
		t.Fatal("the production reader no longer queries gemini:antigravity")
	}
}

func TestAntigravityCandidatesReachVaultOnlyThroughGuard(t *testing.T) {
	before := hostVaultReads.Load()
	f := &Fetcher{Home: t.TempDir()}
	if got := len(f.antigravityCandidates()); got != 0 {
		t.Fatalf("found %d candidates with no sources", got)
	}
	if hostVaultReads.Load() == before {
		t.Fatal("the vault source was not consulted through the guard")
	}
}

func TestAntigravityCandidatesOrderFileBeforeVaultFixture(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, ".codexbar", "antigravity")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oauth_creds.json"), []byte(`{"access_token":"fixture-file-access","expiry_date":1793000000000}`), 0o600); err != nil {
		t.Fatal(err)
	}
	before := hostVaultReads.Load()
	f := &Fetcher{Home: home, vaultRead: func() string {
		return `{"token":{"access_token":"fixture-vault-access"}}`
	}}
	var sources []string
	for _, c := range f.antigravityCandidates() {
		sources = append(sources, c.Source+":"+c.AccessToken)
	}
	if want := []string{"file:fixture-file-access", "vault:fixture-vault-access"}; !reflect.DeepEqual(sources, want) {
		t.Fatalf("candidates %v, want %v", sources, want)
	}
	if hostVaultReads.Load() != before {
		t.Fatal("a fixture reader fell through to the guard")
	}
}

func TestVaultAntigravityCredsParsesFixtureBlobs(t *testing.T) {
	expiry := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC).UnixMilli()
	for _, tc := range []struct {
		name string
		blob string
		want *agCreds
	}{
		{
			name: "CLI profile",
			blob: `{"client_id":"fixture-client","token":{"access_token":" fixture-access ","refresh_token":"fixture-refresh","expiry":"2026-11-05T12:00:00Z"}}`,
			want: &agCreds{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", ExpiryMs: expiry, Source: "vault"},
		},
		{name: "no entry", blob: "", want: nil},
		{name: "malformed blob", blob: `{"token":`, want: nil},
		{name: "no access token", blob: `{"token":{"refresh_token":"fixture-refresh"}}`, want: nil},
	} {
		blob := tc.blob
		before := hostVaultReads.Load()
		got := (&Fetcher{vaultRead: func() string { return blob }}).vaultAntigravityCreds()
		if hostVaultReads.Load() != before {
			t.Fatalf("%s: a fixture reader fell through to the guard", tc.name)
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestCredsFromMapFixtures(t *testing.T) {
	rfc := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC).UnixMilli()
	for _, tc := range []struct {
		name string
		json string
		want *agCreds
	}{
		{
			name: "flat export with numeric expiry",
			json: `{"access_token":"fixture-access","refresh_token":"fixture-refresh","expiry":1793000000000}`,
			want: &agCreds{AccessToken: "fixture-access", RefreshToken: "fixture-refresh", ExpiryMs: 1793000000000, Source: "fixture"},
		},
		{
			name: "expiry_date",
			json: `{"access_token":"fixture-access","expiry_date":1793000000001}`,
			want: &agCreds{AccessToken: "fixture-access", ExpiryMs: 1793000000001, Source: "fixture"},
		},
		{
			name: "nested CLI token with RFC 3339 expiry",
			json: `{"client_id":"fixture-client","token":{"access_token":"fixture-access","expiry":"2026-11-05T12:00:00Z"}}`,
			want: &agCreds{AccessToken: "fixture-access", ExpiryMs: rfc, Source: "fixture"},
		},
		{
			name: "unparseable expiry keeps the token",
			json: `{"access_token":"fixture-access","expiry":"tomorrow"}`,
			want: &agCreds{AccessToken: "fixture-access", Source: "fixture"},
		},
		{
			name: "non-string refresh token is ignored",
			json: `{"access_token":"fixture-access","refresh_token":7}`,
			want: &agCreds{AccessToken: "fixture-access", Source: "fixture"},
		},
		{name: "blank access token", json: `{"access_token":"   ","refresh_token":"fixture-refresh"}`, want: nil},
		{name: "missing access token", json: `{"refresh_token":"fixture-refresh"}`, want: nil},
		{name: "non-string access token", json: `{"access_token":42}`, want: nil},
	} {
		var m map[string]any
		if err := json.Unmarshal([]byte(tc.json), &m); err != nil {
			t.Fatalf("%s: fixture: %v", tc.name, err)
		}
		if got := credsFromMap(m, "fixture"); !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%s: got %+v, want %+v", tc.name, got, tc.want)
		}
	}
}
