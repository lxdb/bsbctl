#!/bin/sh
set -eu

script_directory=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repository_root=$(dirname -- "$script_directory")
allowlist="$script_directory/deadcode-allowlist.txt"
deadcode_package=golang.org/x/tools/cmd/deadcode@v0.45.0

cd -- "$repository_root"

raw_findings=$("$script_directory/go.sh" run "$deadcode_package" -f='{{range .Funcs}}{{printf "%s\t%s\n" $.Path .Name}}{{end}}' ./...)
actual=$(mktemp "${TMPDIR:-/tmp}/bsbctl-deadcode-actual.XXXXXX")
expected=$(mktemp "${TMPDIR:-/tmp}/bsbctl-deadcode-expected.XXXXXX")
trap 'rm -f -- "$actual" "$expected"' EXIT HUP INT TERM

printf '%s\n' "$raw_findings" | sed '/^[[:space:]]*$/d' | LC_ALL=C sort >"$actual"
sed '/^[[:space:]]*#/d; /^[[:space:]]*$/d' "$allowlist" | LC_ALL=C sort >"$expected"

if cmp -s "$expected" "$actual"; then
	exit 0
fi

echo "production dead-code findings differ from the reviewed allowlist:" >&2
diff -u "$expected" "$actual" >&2 || true
echo "" >&2
"$script_directory/go.sh" run "$deadcode_package" ./... >&2 || true
exit 1
