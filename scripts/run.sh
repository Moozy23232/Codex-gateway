#!/usr/bin/env bash
set -euo pipefail

GATEWAY_PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
export PYTHONPATH="${GATEWAY_PROJECT_ROOT}/src${PYTHONPATH:+:${PYTHONPATH}}"

if [ "$#" -eq 0 ]; then
  set -- run
fi

exec python3 -m codex_gateway "$@"
