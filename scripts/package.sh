#!/usr/bin/env bash
set -euo pipefail
umask 022

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
source "$project_root/scripts/build.sh"
cd "$project_root"

fail() { printf '%s\n' "$*" >&2; exit 1; }

(( $# == 0 )) || fail 'Usage: VERSION=0.2.0 [TARGETS="linux/amd64 darwin/arm64"] [OUTPUT_DIR=path] bash scripts/package.sh'
[[ -n "${VERSION:-}" ]] || fail 'Set VERSION explicitly, for example VERSION=0.2.0 bash scripts/package.sh.'
release_version="$(normalize_release_version "$VERSION")"
targets_input="${TARGETS-linux/amd64 linux/arm64 darwin/amd64 darwin/arm64}"
targets=()
IFS=$' \t' read -r -a targets <<< "${targets_input//$'\n'/ }"
(( ${#targets[@]} > 0 )) || fail 'TARGETS must contain at least one supported platform.'
seen_targets=' '
for target in "${targets[@]}"; do
  validate_build_target "$target"
  [[ "$seen_targets" != *" $target "* ]] || fail "Duplicate target: $target"
  seen_targets+="$target "
done

epoch="${SOURCE_DATE_EPOCH-0}"
[[ "$epoch" =~ ^[0-9]{1,10}$ ]] || fail 'SOURCE_DATE_EPOCH must be a nonnegative timestamp supported by ustar.'
epoch=$((10#$epoch))
(( epoch <= 8589934591 )) || fail 'SOURCE_DATE_EPOCH is outside the ustar timestamp range.'

for command in go tar gzip sha256sum mktemp cp mv mkdir chmod rm sort; do
  command -v "$command" >/dev/null || fail "Required packaging command is missing: $command"
done
[[ "$(tar --version 2>/dev/null)" == *'GNU tar'* ]] || fail 'Release packaging requires GNU tar (run it on Linux).'
[[ "$(mv --version 2>/dev/null)" == *'GNU coreutils'* ]] || fail 'Release packaging requires GNU coreutils mv.'
[[ -f "$project_root/scripts/install.sh" ]] || fail 'scripts/install.sh is required in the release assets.'

output_dir="${OUTPUT_DIR-$project_root/dist/releases/v$release_version}"
[[ -n "$output_dir" ]] || fail 'OUTPUT_DIR cannot be empty.'
[[ ! -e "$output_dir" && ! -L "$output_dir" ]] || fail "Output already exists; refusing to overwrite: $output_dir"
mkdir -p "$(dirname "$output_dir")"
output_parent="$(cd "$(dirname "$output_dir")" && pwd)"
output_dir="$output_parent/$(basename "$output_dir")"
[[ ! -e "$output_dir" && ! -L "$output_dir" ]] || fail "Output already exists; refusing to overwrite: $output_dir"

staging_dir="$(mktemp -d "$output_parent/.codex-gateway-package.XXXXXXXX")"
finish() {
  local status=$?
  if (( status == 0 )); then
    rm -rf -- "$staging_dir"
  else
    printf '%s\n' "$status" > "$staging_dir/exit-status"
    printf 'Packaging failed; diagnostic files retained at: %s\n' "$staging_dir" >&2
  fi
}
trap finish EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

assets_dir="$staging_dir/assets"
mkdir -p "$assets_dir" "$staging_dir/builds"
printf 'version=%s\ntargets=%s\nsource_date_epoch=%s\noutput=%s\n' \
  "$release_version" "${targets[*]}" "$epoch" "$output_dir" > "$staging_dir/package-config.txt"
for target in "${targets[@]}"; do
  target_os="${target%/*}"
  target_arch="${target#*/}"
  build_dir="$staging_dir/builds/${target_os}_${target_arch}"
  mkdir -p "$build_dir"
  printf 'Building %s (version %s)\n' "$target" "$release_version" >&2
  if VERSION="$release_version" GOOS="$target_os" GOARCH="$target_arch" \
    OUTPUT="$build_dir/codex-gateway" bash "$project_root/scripts/build.sh" \
    > "$build_dir/build.log" 2>&1; then
    :
  else
    build_status=$?
    printf 'Build failed for %s; see %s\n' "$target" "$build_dir/build.log" >&2
    exit "$build_status"
  fi
  archive="$assets_dir/codex-gateway_${target_os}_${target_arch}.tar.gz"
  TAR_OPTIONS= tar --format=ustar --owner=0 --group=0 --numeric-owner \
    --mtime="@$epoch" --mode=0755 -C "$build_dir" -cf - codex-gateway \
    | GZIP= gzip -n -9 > "$archive"
done
cp -- "$project_root/scripts/install.sh" "$assets_dir/install.sh"
chmod 0755 "$assets_dir/install.sh"
(
  cd "$assets_dir"
  LC_ALL=C sha256sum -- *.tar.gz install.sh | LC_ALL=C sort -k2 > SHA256SUMS
)

# GNU mv -n also handles an output directory created during the build. It can
# return success without moving anything, so check that the source disappeared.
if ! mv -T -n -- "$assets_dir" "$output_dir"; then
  if [[ -e "$output_dir" || -L "$output_dir" ]]; then
    fail "Output appeared during packaging; refusing to overwrite: $output_dir"
  fi
  fail "Cannot finalize the release assets at: $output_dir"
fi
[[ ! -e "$assets_dir" ]] || fail "Output appeared during packaging; refusing to overwrite: $output_dir"
printf '%s\n' "$output_dir"
