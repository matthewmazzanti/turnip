# turnip

## Checks

- `just precommit` — quick Go pass: `gofmt -w` (formats in place), `go vet`, and unit tests over `cmd/...` and `internal/...`. Run this before committing. It excludes the integration suite.
- `just itest` — integration suite; boots both dev VMs first (slow, needs the VM harness). See `justfile` and `nix/vm.sh`.
