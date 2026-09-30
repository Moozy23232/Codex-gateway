#!/usr/bin/env bash
set -euo pipefail
project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$project_root"
command -v go >/dev/null || { echo 'Go is required to build from source. Install the version in mise.toml with mise install.' >&2; exit 1; }
target_os="${GOOS:-$(go env GOOS)}"
target_arch="${GOARCH:-$(go env GOARCH)}"
case "$target_os" in linux|darwin) ;; *) echo 'Supported build targets: linux and darwin.' >&2; exit 1;; esac
output_path="${OUTPUT:-$project_root/dist/${target_os}_${target_arch}/codex-gateway}"
mkdir -p "$(dirname "$output_path")"
CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build -mod=readonly -trimpath -ldflags='-s -w' -o "$output_path" ./cmd/codex-gateway
printf '%s\n' "$output_path"
