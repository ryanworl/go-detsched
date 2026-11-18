#!/usr/bin/env bash
# Copyright 2026 Ryan Worl. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

# delve.bash builds the Delve the release bundles: Delve's dlv at the
# version the docs were tested with, with the patches in misc/release/delve
# applied in order, built by this tree's go for $GOOS/$GOARCH (default the
# host). It downloads Delve's module.
#
# Usage: misc/release/delve.bash OUT [SRCDIR]
#
# OUT is the dlv binary to write. With SRCDIR, it also leaves the patched
# source there. misc/release/README.md says how to change the patches.
#
# Build the toolchain first with misc/release/build.bash; build.bash
# -distpack runs this for the release archive.

set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
out=$1
srcdir=${2:-}
delve=v1.27.2

export GOFLAGS='' GOTOOLCHAIN=local
go=$root/bin/go
tmp=$(mktemp -d)
trap 'chmod -R u+w "$tmp"; rm -rf "$tmp"' EXIT
export GOPATH=$tmp/gopath GOFLAGS=-modcacherw
src=$("$go" mod download -json "github.com/go-delve/delve@$delve" | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p')
cp -R "$src" "$tmp/delve"
chmod -R u+w "$tmp/delve"
for p in "$root"/misc/release/delve/*.patch; do
	patch -s -d "$tmp/delve" -p1 <"$p"
done
case $out in /*) ;; *) out=$PWD/$out ;; esac
(cd "$tmp/delve" && CGO_ENABLED=0 "$go" build -trimpath -o "$out" ./cmd/dlv)
if [ -n "$srcdir" ]; then
	mkdir -p "$srcdir"
	cp -R "$tmp/delve/." "$srcdir"
fi
