#!/usr/bin/env bash
set -euo pipefail

# Shared with package.sh so the tag, embedded version, and installer agree.
normalize_release_version() {
  local LC_ALL=C
  local version="${1-}"
  version="${version#v}"
  local number='(0|[1-9][0-9]*)'
  local prerelease='(0|[1-9][0-9]*|[0-9A-Za-z-]*[A-Za-z-][0-9A-Za-z-]*)'
  local metadata='[0-9A-Za-z-]+'
  local pattern="^${number}\\.${number}\\.${number}(-${prerelease}(\\.${prerelease})*)?(\\+${metadata}(\\.${metadata})*)?$"
  if [[ ! "$version" =~ $pattern ]]; then
    echo 'VERSION must be a SemVer such as 0.2.0 or v0.2.0-rc.1, without whitespace or paths.' >&2
    return 1
  fi
  printf '%s\n' "$version"
}

validate_build_target() {
  case "$1" in
    linux/amd64|linux/arm64|darwin/amd64|darwin/arm64) ;;
    *) echo 'Supported targets: linux/amd64 linux/arm64 darwin/amd64 darwin/arm64.' >&2; return 1 ;;
  esac
}

build_main() {
  if (( $# != 0 )); then
    echo 'Usage: [VERSION=0.2.0] [GOOS=linux GOARCH=amd64] [OUTPUT=path] bash scripts/build.sh' >&2
    return 1
  fi
  local project_root target_os target_arch output_path version
  project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
  cd "$project_root"
  command -v go >/dev/null || { echo 'Go is required to build from source. Install the version in mise.toml with mise install.' >&2; return 1; }
  target_os="${GOOS:-$(go env GOOS)}"
  target_arch="${GOARCH:-$(go env GOARCH)}"
  validate_build_target "$target_os/$target_arch" || return 1
  local ldflags='-s -w -buildid='
  if [[ -n "${VERSION+x}" ]]; then
    version="$(normalize_release_version "$VERSION")" || return 1
    ldflags+=" -X github.com/Moozy23232/Codex-gateway/internal/gateway.Version=$version"
  fi
  output_path="${OUTPUT:-$project_root/dist/${target_os}_${target_arch}/codex-gateway}"
  mkdir -p "$(dirname "$output_path")"
  CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build \
    -mod=readonly -trimpath -buildvcs=false -ldflags="$ldflags" \
    -o "$output_path" ./cmd/codex-gateway
  printf '%s\n' "$output_path"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
  build_main "$@"
fi
