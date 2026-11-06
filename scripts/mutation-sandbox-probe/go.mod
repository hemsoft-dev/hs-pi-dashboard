// A separate module, so `go list ./...`, vet, staticcheck, deadcode and the
// mutation scope of the dashboard module never include this probe.
module mutationsandboxprobe

go 1.27.3
