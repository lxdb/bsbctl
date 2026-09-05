#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=scripts/verify-tools.env
source "${SCRIPT_DIR}/verify-tools.env"

if [[ "$#" -ne 1 || -z "$1" ]]; then
  echo "usage: scripts/install-shellcheck.sh <destination-directory>" >&2
  exit 2
fi

case "$(uname -m)" in
  arm64)
    platform="darwin.aarch64"
    archive_sha256="56affdd8de5527894dca6dc3d7e0a99a873b0f004d7aabc30ae407d3f48b0a79"
    ;;
  x86_64)
    platform="darwin.x86_64"
    archive_sha256="3c89db4edcab7cf1c27bff178882e0f6f27f7afdf54e859fa041fca10febe4c6"
    ;;
  *)
    echo "unsupported ShellCheck installation architecture: $(uname -m)" >&2
    exit 2
    ;;
esac

archive="$(mktemp "${TMPDIR:-/tmp}/bsbctl-shellcheck.XXXXXX.tar.xz")"
extract_directory="$(mktemp -d "${TMPDIR:-/tmp}/bsbctl-shellcheck.XXXXXX")"
cleanup() {
  rm -rf "${archive}" "${extract_directory}"
}
trap cleanup EXIT INT TERM

url="https://github.com/koalaman/shellcheck/releases/download/v${SHELLCHECK_VERSION}/shellcheck-v${SHELLCHECK_VERSION}.${platform}.tar.xz"
curl --fail --location --silent --show-error --output "${archive}" "${url}"
printf '%s  %s\n' "${archive_sha256}" "${archive}" | shasum -a 256 --check
tar -xJf "${archive}" -C "${extract_directory}"
mkdir -p "$1"
install -m 0755 "${extract_directory}/shellcheck-v${SHELLCHECK_VERSION}/shellcheck" "$1/shellcheck"
