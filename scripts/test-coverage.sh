#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# shellcheck source=scripts/verify-tools.env
source "${SCRIPT_DIR}/verify-tools.env"

TMP_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${TMP_DIR}"
}
trap cleanup EXIT INT TERM

check_profile() {
  local label="$1"
  shift
  local profile="${TMP_DIR}/${label}.out"

  (
    cd "${ROOT}"
    "${SCRIPT_DIR}/go.sh" test -mod=readonly -p 2 -count=1 -coverprofile="${profile}" "$@" ./...
  )

  local coverage
  coverage="$("${SCRIPT_DIR}/go.sh" tool cover -func="${profile}" | awk '/^total:/ {gsub(/%/, "", $3); print $3}')"
  awk -v label="${label}" -v coverage="${coverage}" -v minimum="${MIN_COVERAGE}" 'BEGIN {
    if (coverage + 0 < minimum + 0) {
      printf "%s coverage %.1f%% is below %.1f%%\n", label, coverage, minimum > "/dev/stderr"
      exit 1
    }
    printf "%s coverage %.1f%% meets %.1f%%\n", label, coverage, minimum
  }'
}

check_profile standard
check_profile preview -tags preview
