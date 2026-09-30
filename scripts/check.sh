#!/usr/bin/env bash
set -euo pipefail

GATEWAY_PROJECT_ROOT="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
GATEWAY_CHECK_ROOT="${GATEWAY_PROJECT_ROOT}/runs/_tests"
GATEWAY_CHECK_DIR="${GATEWAY_CHECK_ROOT}/check_$(date -u +%Y%m%dT%H%M%SZ)_$$"
export PYTHONPATH="${GATEWAY_PROJECT_ROOT}/src${PYTHONPATH:+:${PYTHONPATH}}"

mkdir -p -- "${GATEWAY_CHECK_ROOT}"
# A collision is an error: never append to or overwrite an earlier check.
mkdir -- "${GATEWAY_CHECK_DIR}"
cd -- "${GATEWAY_PROJECT_ROOT}"

GATEWAY_CHECK_CODE=0
python3 -m unittest discover -s tests >"${GATEWAY_CHECK_DIR}/test.log" 2>&1 || GATEWAY_CHECK_CODE=$?
printf '%s\n' "${GATEWAY_CHECK_CODE}" >"${GATEWAY_CHECK_DIR}/exit-code.txt"

if [ "${GATEWAY_CHECK_CODE}" -eq 0 ]; then
  printf '检查通过。结果目录：%s\n' "${GATEWAY_CHECK_DIR}"
  tail -n 4 -- "${GATEWAY_CHECK_DIR}/test.log"
else
  printf '检查失败（退出码 %s）。结果目录：%s\n' "${GATEWAY_CHECK_CODE}" "${GATEWAY_CHECK_DIR}" >&2
  cat -- "${GATEWAY_CHECK_DIR}/test.log" >&2
fi

exit "${GATEWAY_CHECK_CODE}"
