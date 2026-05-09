#!/usr/bin/env bash
set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

cd "$repo_root"

export GOCACHE="${GOCACHE:-$repo_root/build/go-build-cache}"

go test ./...
make build

(
	cd packages/contracts
	go test ./...
)
