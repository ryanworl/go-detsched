#!/usr/bin/env bash
# Copyright 2026 Ryan Worl. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

# smoke.bash tests a release archive on the platform it is for, as a user
# would get it: it checks the archive's checksum (when the .sha256 file is
# next to it), unpacks it in a new directory, and with an empty environment
#
#   - checks that go version names the archive's version and platform;
#   - runs the archive's own misc/release/gate.bash;
#   - runs detsched -n 100 on the planted bug in testdata/bugcache, which
#     must fail, and runs the replay line it prints under env -i, which must
#     fail with the same message and trace line.
#
# Usage: misc/release/smoke.bash go1.27.4-detsched1.linux-amd64.tar.gz

set -euo pipefail

archive=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
name=$(basename "$archive" .tar.gz)
version=${name%.*}
platform=${name##*.}

if [ -f "$archive.sha256" ]; then
	if command -v sha256sum >/dev/null; then
		(cd "$(dirname "$archive")" && sha256sum -c "$archive.sha256")
	else
		(cd "$(dirname "$archive")" && shasum -a 256 -c "$archive.sha256")
	fi
fi

work=$(mktemp -d)
trap 'chmod -R u+w "$work"; rm -rf "$work"' EXIT
mkdir "$work/home" "$work/tmp"
tar -C "$work" -xzf "$archive"
goroot=$work/$version
clean() {
	env -i HOME="$work/home" TMPDIR="$work/tmp" PATH="$goroot/bin:/usr/bin:/bin:/usr/sbin:/sbin" "$@"
}

got=$(clean go version)
want="go version $version ${platform/-//}"
if [ "$got" != "$want" ]; then
	echo "smoke: go version printed \"$got\", want \"$want\"" >&2
	exit 1
fi
echo "smoke: $got"

clean bash "$goroot/misc/release/gate.bash" "$goroot"

echo "smoke: detsched -n 100 in testdata/bugcache"
cd "$goroot/misc/detsched/testdata/bugcache"
status=0
out=$(clean detsched -n 100 2>&1) || status=$?
printf '%s\n' "$out"
if [ "$status" != 1 ]; then
	echo "smoke: detsched exited with status $status, want 1 (a failure found)" >&2
	exit 1
fi
replay=$(printf '%s\n' "$out" | sed -n 's/^[[:space:]]*replay: //p' | tail -n 1)
log=$(printf '%s\n' "$out" | sed -n 's/^[[:space:]]*log: *\([^ ]*\).*/\1/p' | tail -n 1)
trace=$(grep -m 1 '^detsched: trace=' "$log")
if [ "$(head -n 1 "$log")" != "# $replay" ] || [ -z "$trace" ]; then
	echo "smoke: $log does not start with the replay line or has no trace line" >&2
	exit 1
fi

echo "smoke: env -i /bin/sh -c '$replay'"
status=0
out=$(cd / && env -i /bin/sh -c "$replay" 2>&1) || status=$?
if [ "$status" = 0 ] ||
	! printf '%s\n' "$out" | grep -q 'panic: computed answer twice' ||
	! printf '%s\n' "$out" | grep -qxF "$trace"; then
	printf '%s\n' "$out"
	echo "smoke: the replay exited with status $status and did not repeat the failure ($trace)" >&2
	exit 1
fi
echo "smoke: the replay failed again with $trace"
echo "smoke: $name passed"
