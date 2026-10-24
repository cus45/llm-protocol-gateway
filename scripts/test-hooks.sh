#!/usr/bin/env bash
# Runs the end-to-end tests that need the `testhooks` build tag on internal/gateway.
# Those hooks are kept out of the production build on purpose (see
# internal/gateway/quota_keeper_e2e_hooks.go), so plain `go test ./...` skips the
# external tests that call them; `go test ./...` below covers everything else.
set -euo pipefail
cd "$(dirname "$0")/.."
go test -tags testhooks ./internal/gateway/ ./internal/app/ "$@"
go test ./...
