#!/bin/sh
set -eu

usage() {
    printf '%s\n' \
        'Install codex-gateway and its codex entry without Go or Python.' \
        '' \
        'Usage: sh install.sh [--version VERSION] [--install-dir PATH]' \
        '' \
        '  --version VERSION   Release version, with or without v (default: latest).' \
        '  --install-dir PATH  Destination directory (default: $HOME/.local/bin).' \
        '  --help, -h          Show this help without downloading or writing files.' \
        '' \
        'Environment: CODEX_GATEWAY_VERSION, CODEX_GATEWAY_INSTALL_DIR.' \
        'Command-line options take precedence over environment variables.'
}

fail() {
    printf 'error: %s\n' "$*" >&2
    exit 1
}

install_dir=${CODEX_GATEWAY_INSTALL_DIR:-}
version=${CODEX_GATEWAY_VERSION:-latest}
release_base=${CODEX_GATEWAY_RELEASE_BASE_URL:-https://github.com/Moozy23232/Codex-gateway/releases}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --help|-h) usage; exit 0 ;;
        --install-dir)
            [ "$#" -ge 2 ] && [ -n "$2" ] || fail '--install-dir requires a directory.'
            install_dir=$2
            shift 2
            ;;
        --install-dir=*)
            install_dir=${1#*=}
            [ -n "$install_dir" ] || fail '--install-dir requires a directory.'
            shift
            ;;
        --version)
            [ "$#" -ge 2 ] && [ -n "$2" ] || fail '--version requires a version.'
            version=$2
            shift 2
            ;;
        --version=*)
            version=${1#*=}
            [ -n "$version" ] || fail '--version requires a version.'
            shift
            ;;
        *) fail "Unknown argument: $1" ;;
    esac
done

for tool in curl tar uname awk mktemp mkdir mv chmod rm; do
    command -v "$tool" >/dev/null 2>&1 || fail "Required command not found: $tool"
done
if command -v sha256sum >/dev/null 2>&1; then
    checksum_tool=sha256sum
elif command -v shasum >/dev/null 2>&1; then
    checksum_tool=shasum
else
    fail 'SHA256 verification requires sha256sum or shasum.'
fi

LC_ALL=C
export LC_ALL
valid_version() {
    case "$1" in ''|*[!0-9A-Za-z.+-]*) return 1 ;; esac
    awk -v value="$1" 'BEGIN {
        if (value !~ /^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$/) exit 1
        core = value
        sub(/[-+].*$/, "", core)
        split(core, numbers, "\\.")
        for (i = 1; i <= 3; i++) if (numbers[i] !~ /^(0|[1-9][0-9]*)$/) exit 1
        pre = value
        sub(/\+.*/, "", pre)
        dash = index(pre, "-")
        if (dash) {
            count = split(substr(pre, dash + 1), identifiers, "\\.")
            for (i = 1; i <= count; i++)
                if (identifiers[i] ~ /^[0-9]+$/ && identifiers[i] !~ /^(0|[1-9][0-9]*)$/) exit 1
        }
    }'
}

if [ "$version" != latest ]; then
    version=${version#v}
    valid_version "$version" || fail 'Version must be a valid semantic version, optionally prefixed with v.'
fi

system=$(uname -s)
machine=$(uname -m)
case "$system" in
    Linux) platform=linux ;;
    Darwin) platform=darwin ;;
    *) fail "Unsupported platform: $system ($machine)." ;;
esac
case "$machine" in
    x86_64) architecture=amd64 ;;
    aarch64|arm64) architecture=arm64 ;;
    *) fail "Unsupported architecture: $machine on $system." ;;
esac
asset="codex-gateway_${platform}_${architecture}.tar.gz"

while [ "${release_base%/}" != "$release_base" ]; do
    release_base=${release_base%/}
done
# Reject credentials, query strings, fragments, backslashes and control bytes.
case "$release_base" in *'?'*|*'@'*|*'#'*|*'\'*) fail 'Invalid release base URL.' ;; esac
awk -v value="$release_base" 'BEGIN {
    if (value !~ /^[!-~]+$/ || value ~ /[?@#\\]/) exit 1
}' || fail 'Invalid release base URL.'
case "$release_base" in
    https://*)
        authority=${release_base#https://}
        authority=${authority%%/*}
        [ -n "$authority" ] || fail 'Release base URL requires a host.'
        protocols='=https'
        ;;
    http://*)
        authority=${release_base#http://}
        authority=${authority%%/*}
        awk -v host="$authority" 'BEGIN {
            if (host !~ /^(127\.0\.0\.1|localhost)(:[0-9]+)?$/) exit 1
            if (index(host, ":")) {
                split(host, parts, ":")
                if (parts[2] < 1 || parts[2] > 65535) exit 1
            }
        }' || fail 'HTTP release URLs are allowed only for localhost or 127.0.0.1 test servers.'
        protocols='=http,https'
        ;;
    *) fail 'Release base URL must use HTTPS (HTTP is allowed only on loopback).' ;;
esac

if [ -z "$install_dir" ]; then
    [ -n "${HOME:-}" ] || fail 'Set HOME or specify --install-dir.'
    install_dir="$HOME/.local/bin"
fi
case "$install_dir" in
    /*) ;;
    *) install_dir="$(pwd)/$install_dir" ;;
esac
while [ "$install_dir" != / ] && [ "${install_dir%/}" != "$install_dir" ]; do
    install_dir=${install_dir%/}
done
destination="$install_dir/codex-gateway"
[ ! -d "$destination" ] || fail "Installation target is a directory: $destination"
if [ -e "$destination" ] && [ ! -f "$destination" ] && [ ! -L "$destination" ]; then
    fail "Installation target is not a regular file: $destination"
fi

if [ "$version" = latest ]; then
    # GitHub /latest returns the selected tag in Location. Do not request latest
    # again or follow its redirect; all subsequent downloads use this exact tag.
    if resolved=$(curl --disable --fail --silent --show-error --head \
        --proto "$protocols" --connect-timeout 15 --max-time 60 \
        --output /dev/null --write-out '%{redirect_url}' "$release_base/latest"); then
        :
    else
        fail 'Unable to resolve the latest release.'
    fi
    case "$resolved" in
        "$release_base"/tag/*) tag=${resolved#"$release_base/tag/"} ;;
        *) fail 'Latest release did not redirect to a tag under the release base URL.' ;;
    esac
    version=${tag#v}
    valid_version "$version" || fail 'Latest release has an invalid semantic version tag.'
else
    tag="v$version"
fi

work_dir=
staging=
cleanup() {
    if [ -n "$staging" ]; then rm -f "$staging" || :; fi
    if [ -n "$work_dir" ]; then rm -rf "$work_dir" || :; fi
}
trap cleanup 0
trap 'exit 130' 2
trap 'exit 143' 15
temp_root=${TMPDIR:-/tmp}
case "$temp_root" in /*) ;; *) temp_root="$(pwd)/$temp_root" ;; esac
work_dir=$(mktemp -d "$temp_root/codex-gateway-install.XXXXXX") || fail 'Cannot create a temporary download directory.'

download() {
    # Asset redirects may use HTTPS only, including when a loopback test server
    # was selected. Existing HTTP(S)/ALL_PROXY environment settings are retained.
    curl --disable --fail --silent --show-error --location \
        --proto "$protocols" --proto-redir '=https' \
        --connect-timeout 15 --max-time 300 --output "$2" "$1" || fail "Unable to download $3."
}
download "$release_base/download/$tag/SHA256SUMS" "$work_dir/SHA256SUMS" SHA256SUMS
if expected=$(awk -v name="$asset" '
    $2 == name || $2 == "*" name {
        count++
        if (NF != 2 || length($1) != 64 || $1 ~ /[^0-9a-fA-F]/) invalid = 1
        hash = tolower($1)
    }
    END { if (count != 1 || invalid) exit 1; print hash }
' "$work_dir/SHA256SUMS"); then
    :
else
    fail "SHA256SUMS has no unique valid checksum for $asset."
fi
archive="$work_dir/$asset"
download "$release_base/download/$tag/$asset" "$archive" "$asset"
if [ "$checksum_tool" = sha256sum ]; then
    actual=$(sha256sum "$archive") || fail 'Cannot calculate archive SHA256.'
else
    actual=$(shasum -a 256 "$archive") || fail 'Cannot calculate archive SHA256.'
fi
actual=${actual%% *}
[ "$actual" = "$expected" ] || fail "SHA256 mismatch for $asset; existing installation was preserved."

mkdir -p "$install_dir" || fail "Cannot create installation directory: $install_dir"
staging=$(mktemp "$install_dir/.codex-gateway.XXXXXX") || fail 'Cannot create a temporary executable in the installation directory.'
# Read the one expected archive member to stdout; never extract archive paths.
TAR_OPTIONS= tar -xOzf "$archive" codex-gateway > "$staging" || fail 'Archive does not contain a readable codex-gateway binary.'
[ -s "$staging" ] || fail 'Archive contains an empty codex-gateway binary.'
chmod 755 "$staging" || fail 'Cannot make the downloaded binary executable.'
if actual_version=$("$staging" --version </dev/null); then
    :
else
    fail 'Downloaded binary cannot run on this platform.'
fi
[ "$actual_version" = "$version" ] || fail "Downloaded binary does not report release version $version."
# The verified executable installs the binary, codex entry and manifest as one
# transaction, preserving an existing codex launcher and rolling back failures.
# Paths are passed as arguments, never interpolated into generated shell code.
[ ! -d "$destination" ] || fail "Installation target is a directory: $destination"
"$staging" __install --install-dir "$install_dir" </dev/null || fail 'Cannot install codex-gateway and its codex entry; see the recovery details above.'
staging=
printf 'Installed codex-gateway %s at %s\n' "$version" "$destination"
printf 'The codex entry is available at %s/codex.\n' "$install_dir"
case ":${PATH:-}:" in
    *:"$install_dir":*)
        selected_codex=$(command -v codex || :)
        if [ "$selected_codex" != "$install_dir/codex" ]; then
            printf 'Move %s to the beginning of PATH so codex uses this installation.\n' "$install_dir"
        fi
        ;;
    *) printf 'Add %s to the beginning of PATH to run codex and codex-gateway by name.\n' "$install_dir" ;;
esac
