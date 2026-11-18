#!/usr/bin/env bash
# Copyright 2026 Ryan Worl. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

# build.bash builds the toolchain with make.bash and the detsched command
# next to go, in bin/ (bin/$GOOS_$GOARCH when GOOS or GOARCH names another
# platform, as make.bash does).
#
# Usage: misc/release/build.bash [-distpack] [make.bash flags]
#
# GOROOT_BOOTSTRAP, GOOS and GOARCH work as they do for make.bash. With
# -distpack, it also writes the release archive
# pkg/distpack/$VERSION.$GOOS-$GOARCH.tar.gz, whose top directory is named
# $VERSION and which holds go, gofmt, detsched and Delve's dlv in bin/, and
# its checksum in the same name plus .sha256. Building dlv downloads its
# module.

set -euo pipefail

root=$(cd "$(dirname "$0")/../.." && pwd)
distpack=false
for a in "$@"; do
	[ "$a" = -distpack ] && distpack=true
done

version=$(sed -n 1p "$root/VERSION")
# A release's version must be one Delve parses: it reads a version with a
# dot after the patch number as go0.0 and refuses to debug its binaries.
if $distpack && ! echo "$version" | grep -qE '^go[0-9]+\.[0-9]+\.[0-9]+-detsched[0-9]+$'; then
	echo "build: VERSION is $version, not goX.Y.Z-detschedN" >&2
	exit 1
fi

(cd "$root/src" && ./make.bash "$@")

export GOFLAGS='' GOTOOLCHAIN=local
go=$root/bin/go
goos=$("$go" env GOOS)
goarch=$("$go" env GOARCH)
bin=$root/bin
if [ "$goos/$goarch" != "$("$go" env GOHOSTOS)/$("$go" env GOHOSTARCH)" ]; then
	bin=$root/bin/${goos}_$goarch
fi
# -buildvcs=false: the same tree builds the same archive in the development
# repository and in the published one, whose commits differ.
(cd "$root/misc/detsched" && CGO_ENABLED=0 "$go" build -trimpath -buildvcs=false -o "$bin/" ./cmd/detsched)
echo "Installed detsched in $bin"

if ! $distpack; then
	exit 0
fi

# make.bash -distpack packs only go and gofmt in bin/, so the archive is
# unpacked, given detsched and a versioned top directory, and packed again
# the way distpack packs it: sorted, owned by root, dated by VERSION.
mtime=$(sed -n 's/^time //p' "$root/VERSION")
archive=$root/pkg/distpack/$version.$goos-$goarch.tar.gz
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
umask 022
tar -C "$tmp" -xzf "$archive"
cp "$bin/detsched" "$tmp/go/bin/detsched"
chmod 0755 "$tmp/go/bin/detsched"

# Delve, which the debug: lines detsched prints run, at the version the
# docs were tested with, with delve.patch, built by this toolchain for the
# target, with its license and the patch.
delve=v1.27.2
export GOPATH=$tmp/gopath GOFLAGS=-modcacherw
src=$("$go" mod download -json "github.com/go-delve/delve@$delve" | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",$/\1/p')
cp -R "$src" "$tmp/delve"
chmod -R u+w "$tmp/delve"
patch -s -d "$tmp/delve" -p1 <"$root/misc/release/delve.patch"
(cd "$tmp/delve" && CGO_ENABLED=0 GOOS=$goos GOARCH=$goarch "$go" build -trimpath -o "$tmp/go/bin/dlv" ./cmd/dlv)
chmod 0755 "$tmp/go/bin/dlv"
mkdir -p "$tmp/go/misc/delve"
cp "$tmp/delve/LICENSE" "$tmp/go/misc/delve/LICENSE"
cp "$root/misc/release/delve.patch" "$tmp/go/misc/delve/delve.patch"
chmod 0644 "$tmp/go/misc/delve/LICENSE" "$tmp/go/misc/delve/delve.patch"
unset GOPATH GOFLAGS
rm -rf "$tmp/gopath" "$tmp/delve"
mv "$tmp/go" "$tmp/$version"
tar -C "$tmp" --sort=name --owner=0 --group=0 --numeric-owner --mtime="$mtime" \
	--format=gnu -cf - "$version" | gzip -9n >"$archive.new"
mv "$archive.new" "$archive"
(cd "$root/pkg/distpack" && sha256sum "$(basename "$archive")" >"$(basename "$archive").sha256")
echo "Packed $archive"
cat "$archive.sha256"
