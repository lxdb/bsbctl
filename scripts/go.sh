#!/bin/sh
set -eu

script_directory=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
repository_root=$(dirname -- "$script_directory")
toolchain=$(awk '$1 == "toolchain" { print $2 }' "$repository_root/go.mod")

if ! printf '%s\n' "$toolchain" | grep -Eq '^go[0-9][0-9]*\.[0-9][0-9]*\.[0-9][0-9]*$'; then
	echo "go.mod must declare an exact Go toolchain" >&2
	exit 2
fi

unset AR CC CFLAGS CGO_CFLAGS CGO_CPPFLAGS CGO_CXXFLAGS CGO_LDFLAGS
unset CPATH CPPFLAGS CXX CXXFLAGS FC LDFLAGS LIBRARY_PATH PKG_CONFIG
unset C_INCLUDE_PATH CPLUS_INCLUDE_PATH DYLD_LIBRARY_PATH MACOSX_DEPLOYMENT_TARGET SDKROOT
unset CGO_ENABLED GO386 GOAMD64 GOARCH GOARM GOARM64 GOAUTH GOEXPERIMENT GOFIPS140 GOFLAGS
unset GOINSECURE GOMIPS GOMIPS64 GONOPROXY GONOSUMDB GOPPC64 GOPRIVATE GOPROXY GORISCV64 GOSUMDB
unset GOVCS GOWASM

export GOENV=off
export GOTOOLCHAIN="$toolchain"
export GOWORK=off

exec go "$@"
