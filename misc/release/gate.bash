#!/usr/bin/env bash
# Copyright 2026 Ryan Worl. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

# gate.bash runs the checks every change must pass, with the go and
# detsched in GOROOT/bin, where GOROOT is this tree or the directory given
# as the argument, such as an unpacked release:
#
#   - the detsched command's tests;
#   - the runtime, testing/synctest, internal/godebug and sync tests with
#     GOEXPERIMENT=detsched, in -short mode;
#   - the linker's check that a debugger's breakpoint on an inlined call
#     is on its hot path with GOEXPERIMENT=detsched;
#   - a stock hello world has no detsched symbols.
#
# Usage: misc/release/gate.bash [GOROOT]
#
# Build the toolchain first with misc/release/build.bash.

set -euo pipefail

goroot=$(cd "${1:-$(dirname "$0")/../..}" && pwd)
export PATH=$goroot/bin:$PATH
unset GOROOT GOEXPERIMENT GOFLAGS GOOS GOARCH
export GOTOOLCHAIN=local

v=$(go env GOVERSION)
case $v in
*-detsched[0-9]*) ;;
*) echo "gate: $goroot/bin/go is $v, not a detsched toolchain" >&2; exit 1 ;;
esac
echo "gate: $v in $goroot"

echo "gate: detsched command tests"
(cd "$goroot/misc/detsched/cmd/detsched" && go test -count=1 .)

echo "gate: runtime tests with GOEXPERIMENT=detsched"
GOEXPERIMENT=detsched go test -short -count=1 runtime testing/synctest internal/godebug sync

echo "gate: debug info of inlined calls with GOEXPERIMENT=detsched"
go test -count=1 -run '^TestDetsched' cmd/link/internal/ld

echo "gate: stock hello world has no detsched symbols"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
cat >"$tmp/hello.go" <<'EOF'
package main

func main() { println("hello") }
EOF
(cd "$tmp" && go build -o hello hello.go)
n=$(go tool nm "$tmp/hello" | grep -cE 'runtime\.det|detsim' || true)
if [ "$n" != 0 ]; then
	echo "gate: stock hello world has $n detsched symbols" >&2
	exit 1
fi
echo "gate: passed"
