#!/usr/bin/env bash
set -euo pipefail

project_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
gateway_binary="$(bash "$project_root/scripts/build.sh")"
if [[ $# -eq 0 ]]; then set -- run; fi
exec "$gateway_binary" "$@"
