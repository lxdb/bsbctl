#!/bin/sh

set -eu

repository=https://github.com/lxdb/bsbctl
apps=
apps_set=false
device_url=
update_path=true
local_build=false

usage() {
  cat <<'EOF'
Usage: install.sh [--local] [--apps APP-ID,...|none] [--device-url URL] [--no-path-update]

Install the latest stable bsbctl release, configure its service, and optionally
install first-party apps. Without --apps, an interactive terminal is required
and bsbctl asks about every available app.

With --local, build this checkout and refresh configured local first-party
plugins. --apps also adds selected apps. Requires Go and an existing bsbctl
configuration; does not download releases or change PATH.
EOF
}

fail() {
  printf 'bsbctl installer: %s\n' "$1" >&2
  exit 1
}

has_controlling_terminal() {
  (exec 3<>/dev/tty) 2>/dev/null
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --local)
      local_build=true
      shift
      ;;
    --apps)
      [ "$#" -ge 2 ] || fail "--apps requires a value"
      apps=$2
      apps_set=true
      shift 2
      ;;
    --device-url)
      [ "$#" -ge 2 ] || fail "--device-url requires a value"
      device_url=$2
      shift 2
      ;;
    --no-path-update)
      update_path=false
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      fail "unknown argument: $1"
      ;;
  esac
done

[ -n "${HOME:-}" ] || fail "HOME is not set"
case "$HOME" in
  /*) ;;
  *) fail "HOME must be an absolute path" ;;
esac

[ "$(uname -s)" = Darwin ] || fail "only macOS is supported"
case "$(uname -m)" in
  arm64) architecture=arm64 ;;
  x86_64) architecture=amd64 ;;
  *) fail "this Mac architecture is not supported" ;;
esac

if [ "$local_build" = true ]; then
  [ -z "$device_url" ] || fail "--local preserves device settings; --device-url is only for release setup"
  script_directory=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
  [ -f "$script_directory/go.mod" ] && [ -f "$script_directory/cmd/localinstall/main.go" ] || fail "--local must run from a source checkout"
  cd "$script_directory"
  set -- run ./cmd/localinstall
  if [ "$apps_set" = true ]; then
    set -- "$@" --apps "$apps"
  fi
  exec sh "$script_directory/scripts/go.sh" "$@"
fi

for command_name in curl tar shasum awk grep mktemp; do
  command -v "$command_name" >/dev/null 2>&1 || fail "$command_name is required"
done

latest_url=$(curl -fsSL -o /dev/null -w '%{url_effective}' "$repository/releases/latest") || fail "could not resolve the latest stable release"
tag=${latest_url##*/}
[ "$latest_url" = "$repository/releases/tag/$tag" ] || fail "the latest release redirect is invalid"
printf '%s\n' "$tag" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$' || fail "the latest release tag is not a stable semantic version"
version=${tag#v}

archive=bsbctl_${version}_darwin_${architecture}.tar.gz
checksums=SHA256SUMS-darwin-${architecture}
temporary_directory=$(mktemp -d "${TMPDIR:-/tmp}/bsbctl-install.XXXXXX") || fail "could not create a temporary directory"
install_temporary=
cleanup() {
  rm -rf "$temporary_directory"
  if [ -n "$install_temporary" ]; then
    rm -f "$install_temporary"
  fi
}
trap cleanup EXIT HUP INT TERM

curl -fsSL -o "$temporary_directory/$archive" "$repository/releases/download/$tag/$archive" || fail "could not download $archive"
curl -fsSL -o "$temporary_directory/$checksums" "$repository/releases/download/$tag/$checksums" || fail "could not download $checksums"

expected_checksum=$(awk -v filename="$archive" '$2 == filename { count++; digest=$1 } END { if (count == 1) print digest }' "$temporary_directory/$checksums")
printf '%s\n' "$expected_checksum" | grep -Eq '^[0-9a-f]{64}$' || fail "the release checksum manifest is invalid"
actual_checksum=$(shasum -a 256 "$temporary_directory/$archive" | awk '{ print $1 }')
[ "$actual_checksum" = "$expected_checksum" ] || fail "the downloaded archive checksum does not match the release"

archive_listing=$(tar -tzf "$temporary_directory/$archive") || fail "the release archive is invalid"
printf '%s\n' "$archive_listing" | awk '$0 == "bsbctl" { count++ } END { exit count != 1 }' || fail "the release archive does not contain exactly one bsbctl executable"
tar -xzf "$temporary_directory/$archive" -C "$temporary_directory" bsbctl || fail "could not extract bsbctl"
[ -f "$temporary_directory/bsbctl" ] && [ ! -L "$temporary_directory/bsbctl" ] || fail "the extracted bsbctl is not a regular file"

install_directory=$HOME/.local/bin
mkdir -p "$install_directory" || fail "could not create $install_directory"
install_temporary=$(mktemp "$install_directory/.bsbctl.XXXXXX") || fail "could not prepare the installed executable"
cp "$temporary_directory/bsbctl" "$install_temporary" || fail "could not copy the verified executable"
chmod 755 "$install_temporary" || fail "could not make the installed executable runnable"
mv -f "$install_temporary" "$install_directory/bsbctl" || fail "could not install bsbctl"
install_temporary=

case ":$PATH:" in
  *":$install_directory:"*) ;;
  *)
    if [ "$update_path" = true ] && has_controlling_terminal; then
      case "${SHELL:-}" in
        */zsh) profile=$HOME/.zprofile ;;
        */bash) profile=$HOME/.bash_profile ;;
        *) profile=$HOME/.profile ;;
      esac
      printf 'Add %s to PATH in %s? [y/N] ' "$install_directory" "$profile" >/dev/tty
      IFS= read -r answer </dev/tty || answer=
      case "$answer" in
        y|Y|yes|YES|Yes)
          # Keep the variables literal for the shell profile that sources this line.
          # shellcheck disable=SC2016
          path_line='export PATH="$HOME/.local/bin:$PATH"'
          if ! grep -Fqx "$path_line" "$profile" 2>/dev/null; then
            printf '\n%s\n' "$path_line" >>"$profile" || fail "could not update $profile"
          fi
          printf 'PATH updated. Open a new terminal to use bsbctl by name.\n' >&2
          ;;
        *) printf 'PATH was not changed. Run %s directly or add it later.\n' "$install_directory/bsbctl" >&2 ;;
      esac
    else
      printf '%s is not on PATH; add it to use bsbctl by name.\n' "$install_directory" >&2
    fi
    ;;
esac

set -- setup
if [ "$apps_set" = true ]; then
  set -- "$@" --apps "$apps"
fi
if [ -n "$device_url" ]; then
  set -- "$@" --device-url "$device_url"
fi

if [ "$apps_set" = false ] && has_controlling_terminal; then
  "$install_directory/bsbctl" "$@" </dev/tty
else
  "$install_directory/bsbctl" "$@"
fi
