#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$project_root"
mkdir -p runs/_tests
run_dir="$(mktemp -d "$project_root/runs/_tests/check_$(date -u +%Y%m%dT%H%M%SZ)_XXXXXX")"
trap 'result=$?; printf "%s\n" "$result" > "$run_dir/exit-code"' EXIT
go version > "$run_dir/toolchain.txt"
cp go.mod go.sum mise.toml "$run_dir/"
printf 'Validation output: %s\n' "$run_dir"
go mod verify 2>&1 | tee "$run_dir/modules.log"
go vet ./... 2>&1 | tee "$run_dir/vet.log"
go test -race -count=1 -timeout=120s ./... 2>&1 | tee "$run_dir/tests.log"
OUTPUT="$run_dir/codex-gateway" bash scripts/build.sh 2>&1 | tee "$run_dir/build.log"
"$run_dir/codex-gateway" --version | tee "$run_dir/version.txt"
