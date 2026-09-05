#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# shellcheck source=scripts/verify-tools.env
source "${SCRIPT_DIR}/verify-tools.env"

usage() {
  cat <<'EOF'
usage: scripts/verify.sh <command>

commands:
  quick             Run formatting, tests, preview tests, and vet.
  format            Check Go formatting.
  test              Run the complete deterministic Go test suite.
  preview           Run preview-tagged tests through their production builders.
  race              Run the complete suite with the race detector.
  coverage          Enforce standard and preview coverage floors.
  vet               Run go vet.
  dead-code         Compare production dead code with the reviewed allowlist.
  docs              Run documentation contracts and compile the external example.
  repository        Check shell scripts, Git whitespace, and workflow syntax.
  metadata          Verify module checksums and tidy state.
  security          Run the pinned Go vulnerability scanner.
  fuzz              Run all parser fuzz targets for BSBCTL_FUZZ_TIME or 30 seconds.
  depth             Repeat the complete suite under randomized order.
  linux-pluginhost  Run Linux plugin-host tests; this phase requires Linux.
  preflight         Evaluate release, legal, dependency, and catalog-trust inputs.
  all               Run all deterministic, device-free source gates.
  release           Run all source gates and deterministic Darwin release verification.
EOF
}

phase() {
  printf '\n==> %s\n' "$1"
}

require_command() {
  if ! command -v "$1" >/dev/null 2>&1; then
    echo "required command not found: $1" >&2
    exit 2
  fi
}

root_go() {
  require_command go
  (
    cd "${ROOT}"
    "${SCRIPT_DIR}/go.sh" "$@"
  )
}

run_format() {
  phase "Go formatting"
  local gofmt_binary path unformatted
  local -a go_files=()
  gofmt_binary="$(root_go env GOROOT)/bin/gofmt"
  while IFS= read -r -d '' path; do
    if [[ -f "${ROOT}/${path}" ]]; then
      go_files+=("${path}")
    fi
  done < <(cd "${ROOT}" && git ls-files -z --cached --others --exclude-standard -- '*.go')
  unformatted="$(cd "${ROOT}" && "${gofmt_binary}" -l "${go_files[@]}")"
  if [[ -n "${unformatted}" ]]; then
    printf '%s\n' "${unformatted}" >&2
    exit 1
  fi
}

run_test() {
  phase "Go tests"
  root_go test -mod=readonly ./... -p 2 -shuffle=on -count=1
}

run_preview() {
  phase "preview-tagged tests"
  root_go test -mod=readonly -tags preview ./cmd/previewgen -shuffle=on -count=1
  root_go test -mod=readonly -tags preview ./plugins/calendar ./plugins/codex ./plugins/codexquota ./plugins/macresources -run Preview -shuffle=on -count=1
}

run_race() {
  phase "race tests"
  root_go test -mod=readonly -race ./... -p 2 -shuffle=on -count=1
}

run_coverage() {
  phase "coverage"
  (
    cd "${ROOT}"
    "${SCRIPT_DIR}/test-coverage.sh"
  )
}

run_vet() {
  phase "go vet"
  root_go vet -mod=readonly ./...
}

run_dead_code() {
  phase "dead code"
  (
    cd "${ROOT}"
    "${SCRIPT_DIR}/check-dead-code.sh"
  )
}

run_docs() {
  phase "documentation contracts"
  root_go test -mod=readonly ./docs

  phase "command documentation contracts"
  root_go test -mod=readonly ./cmd/bsbctl ./cmd/releasectl -run '^TestDocumentation'

  phase "external plugin example"
  (
    cd "${ROOT}/docs/examples/external-plugin"
    "${SCRIPT_DIR}/go.sh" test -mod=readonly ./...
  )
}

run_repository() {
  phase "shell syntax"
  require_command shellcheck
  local shellcheck_version
  shellcheck_version="$(shellcheck --version | awk '$1 == "version:" { print $2 }')"
  if [[ "${shellcheck_version}" != "${SHELLCHECK_VERSION}" ]]; then
    echo "ShellCheck ${SHELLCHECK_VERSION} is required; found ${shellcheck_version:-unknown}" >&2
    exit 2
  fi
  bash -n "${SCRIPT_DIR}/verify.sh" "${SCRIPT_DIR}/test-coverage.sh" "${SCRIPT_DIR}/install-shellcheck.sh"
  sh -n "${SCRIPT_DIR}/check-dead-code.sh" "${SCRIPT_DIR}/go.sh" "${SCRIPT_DIR}/verify-local.sh"
  (cd "${ROOT}" && shellcheck -x scripts/*.sh install.sh)

  phase "Git whitespace"
  (cd "${ROOT}" && git diff --check HEAD)

  phase "GitHub workflow syntax"
  root_go run "github.com/rhysd/actionlint/cmd/actionlint@${ACTIONLINT_VERSION}"
}

run_metadata() {
  phase "module checksums"
  root_go mod verify
  phase "module tidy state"
  root_go mod tidy -diff
}

run_security() {
  phase "Go vulnerability scan"
  root_go run "golang.org/x/vuln/cmd/govulncheck@${GOVULNCHECK_VERSION}" ./...
}

run_fuzz() {
  local fuzz_time="${BSBCTL_FUZZ_TIME:-${DEFAULT_FUZZ_TIME}}"
  phase "strict protocol decoding fuzz (${fuzz_time})"
  root_go test -mod=readonly ./sdk/protocol -run '^$' -fuzz '^FuzzDecodeStrictOperationRequest$' -fuzztime="${fuzz_time}"
  phase "SDK JSON-RPC envelope fuzz (${fuzz_time})"
  root_go test -mod=readonly ./sdk/rpc -run '^$' -fuzz '^FuzzDecodeMessage$' -fuzztime="${fuzz_time}"
  phase "Codex app-server envelope fuzz (${fuzz_time})"
  root_go test -mod=readonly ./plugins/codex/internal/appserver -run '^$' -fuzz '^FuzzSessionEnvelope$' -fuzztime="${fuzz_time}"
}

run_depth() {
  phase "repeated randomized tests"
  root_go test -mod=readonly ./... -p 2 -shuffle=on -count=5
}

run_linux_pluginhost() {
  if [[ "$(uname -s)" != "Linux" ]]; then
    echo "linux-pluginhost requires Linux" >&2
    exit 2
  fi
  phase "Linux plugin-host tests"
  root_go test -mod=readonly ./internal/pluginhost -count=1
  root_go test -mod=readonly -race ./internal/pluginhost -count=1
}

run_preflight() {
  phase "release preflight"
  root_go run ./cmd/releasectl preflight --root .
}

run_quick() {
  run_format
  run_test
  run_preview
  run_vet
}

run_all() {
  run_format
  run_metadata
  run_test
  run_preview
  run_race
  run_coverage
  run_vet
  run_dead_code
  run_docs
  run_repository
}

run_release() {
  run_all
  phase "release inputs and deterministic artifacts"
  root_go run ./cmd/releasectl verify --root .
}

case "${1:-}" in
  quick) run_quick ;;
  format) run_format ;;
  test) run_test ;;
  preview) run_preview ;;
  race) run_race ;;
  coverage) run_coverage ;;
  vet) run_vet ;;
  dead-code) run_dead_code ;;
  docs) run_docs ;;
  repository) run_repository ;;
  metadata) run_metadata ;;
  security) run_security ;;
  fuzz) run_fuzz ;;
  depth) run_depth ;;
  linux-pluginhost) run_linux_pluginhost ;;
  preflight) run_preflight ;;
  all) run_all ;;
  release) run_release ;;
  -h|--help|help) usage ;;
  *)
    usage >&2
    exit 2
    ;;
esac
