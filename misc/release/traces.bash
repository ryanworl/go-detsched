#!/usr/bin/env bash
# Copyright 2026 Ryan Worl. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

# traces.bash checks that a seed's schedule does not depend on the machine
# that runs it. With -build, it builds the test binaries of a few
# simulation programs for GOOS/GOARCH into DIR; otherwise it runs each
# binary in DIR for seeds 1 to 3, under RUNNER if given (such as
# qemu-aarch64-static), and prints one line per run with its trace, which
# carries the schedule's hash, switches, and busy time. CI builds the
# linux/arm64 binaries on linux/amd64, runs them there under qemu, and
# runs the same binaries on a linux/arm64 machine: the lines must match.
#
# Usage: misc/release/traces.bash -build DIR
#        misc/release/traces.bash DIR [RUNNER]
#
# Run it with the detsched toolchain's go first on PATH.

set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
pkgs="simbusy simkv simcompose simlease"

if [ "${1:-}" = -build ]; then
	dir=$(mkdir -p "$2" && cd "$2" && pwd)
	for p in $pkgs; do
		(cd "$root/misc/detsched/testdata/$p" && GOEXPERIMENT=detsched CGO_ENABLED=0 go test -c -o "$dir/$p.test" .)
	done
	exit 0
fi

dir=$(cd "$1" && pwd)
runner=${2:-}
cd "$(mktemp -d)"
for p in $pkgs; do
	for seed in 1 2 3; do
		out=$(env -i PATH="$PATH" HOME="$HOME" GODETSEED=$seed GODEBUG=dettrace=1 $runner "$dir/$p.test" 2>&1 || true)
		echo "$p seed $seed: $(echo "$out" | grep -E '^detsched: (trace|busy)' | tr '\n' ' ')"
	done
done
