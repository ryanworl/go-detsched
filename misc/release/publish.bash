#!/usr/bin/env bash
# Copyright 2026 Ryan Worl. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

# publish.bash publishes the development repository's main branch to the
# public GitHub repository. The public history has one commit per publish:
# each holds the tree of origin's main, and its only parent is the previous
# public commit, so no development history is reachable from it.
#
# Usage: misc/release/publish.bash [-n] [-tag] [-root] [-m message] [REMOTE]
#
# REMOTE is the public repository (default
# https://github.com/ryanworl/go-detsched.git).
#
#   -tag   also tag the new commit (or, when REMOTE's main already has the
#          tree, its last commit) with VERSION's first line, such as
#          go1.27.4-detsched1, which starts the release workflow, and with
#          misc/detsched/v0.1.0, the SDK version of release 1; and tag
#          origin's main with the same version.
#   -root  start the public history over: the new commit has no parent and
#          replaces REMOTE's main.
#   -m     the commit message (default: the version, with "(unreleased)"
#          unless -tag).
#   -n     make the commit and print what would be pushed, but push nothing.
#
# The commit's author and committer are $PUBLISH_AUTHOR (default
# "Ryan Worl <ryantworl@gmail.com>"). publish.bash refuses to publish when
# the identity, the message or a tag name contains a word that matches
# $PUBLISH_DENY (an extended regular expression matched case-insensitively
# against whole words), or when a file of the tree has more such words than
# the same file in Go's tree on origin's branch upstream.

set -euo pipefail

dryrun=false tag=false root=false msg=
while [ $# -gt 0 ]; do
	case $1 in
	-n) dryrun=true ;;
	-tag) tag=true ;;
	-root) root=true ;;
	-m) msg=$2; shift ;;
	-*) echo "usage: misc/release/publish.bash [-n] [-tag] [-root] [-m message] [REMOTE]" >&2; exit 2 ;;
	*) break ;;
	esac
	shift
done
remote=${1:-https://github.com/ryanworl/go-detsched.git}
cd "$(git rev-parse --show-toplevel)"

author=${PUBLISH_AUTHOR:-Ryan Worl <ryantworl@gmail.com>}
deny=${PUBLISH_DENY:-'[a]mp|[a]mpcode'}

git fetch --quiet origin upstream
up=$(git rev-parse FETCH_HEAD)
git fetch --quiet origin main
src=$(git rev-parse FETCH_HEAD)
tree=$(git rev-parse "$src^{tree}")
version=$(git show "$src:VERSION" | sed -n 1p)
# Delve reads a version with a dot after the patch number as go0.0 and
# refuses to debug its binaries.
if ! echo "$version" | grep -qE '^go[0-9]+\.[0-9]+\.[0-9]+-detsched[0-9]+$'; then
	echo "publish: VERSION is $version, not goX.Y.Z-detschedN" >&2
	exit 1
fi
n=${version##*-detsched}
sdk=misc/detsched/v0.$n.0

parent=
if ! $root && git fetch --quiet "$remote" main 2>/dev/null; then
	parent=$(git rev-parse FETCH_HEAD)
	if [ -n "$(git merge-base "$parent" "$src" || true)" ]; then
		echo "publish: $remote main shares history with origin's main; is $remote the public repository?" >&2
		exit 1
	fi
fi
same=false
if [ -n "$parent" ] && [ "$(git rev-parse "$parent^{tree}")" = "$tree" ]; then
	same=true
	if ! $tag; then
		echo "publish: $remote main already has the tree of origin's main"
		exit 0
	fi
fi
if $tag; then
	for t in "$version" "$sdk"; do
		if [ -n "$(git ls-remote --tags "$remote" "refs/tags/$t")" ]; then
			echo "publish: $remote already has tag $t" >&2
			exit 1
		fi
	done
fi

if [ -z "$msg" ]; then
	msg=$version
	$tag || msg="$version (unreleased)"
fi
if printf '%s\n' "$author" "$msg" "$version" "$sdk" | grep -iwE "$deny"; then
	echo "publish: the identity, message or tags above match PUBLISH_DENY" >&2
	exit 1
fi
bad=$(awk -F: 'NR == FNR { up[$2] = $3; next } $3 > up[$2] + 0 { print $2 }' \
	<(git grep -c -iwE "$deny" "$up" -- || true) <(git grep -c -iwE "$deny" "$src" -- || true))
if [ -n "$bad" ]; then
	printf 'publish: these files match PUBLISH_DENY more often than Go'"'"'s do:\n%s\n' "$bad" >&2
	exit 1
fi

if $same; then
	# The public main already has this tree: tag it rather than add a
	# commit that changes nothing.
	commit=$parent
	msg=$(git log -1 --format=%s "$commit")
	refs=()
else
	name=${author% <*}
	email=${author##*<}
	email=${email%>}
	commit=$(GIT_AUTHOR_NAME=$name GIT_AUTHOR_EMAIL=$email GIT_COMMITTER_NAME=$name GIT_COMMITTER_EMAIL=$email \
		git commit-tree "$tree" ${parent:+-p "$parent"} -m "$msg")
	refs=("$commit:refs/heads/main")
	$root && refs=("+$commit:refs/heads/main")
fi
$tag && refs+=("$commit:refs/tags/$version" "$commit:refs/tags/$sdk")

echo "publish: origin main $src, tree $tree"
echo "publish: commit $commit (${parent:-no parent}): $msg"
if $dryrun; then
	echo "publish: would push to $remote: ${refs[*]}"
	$tag && echo "publish: would push to origin: $src:refs/tags/$version"
	exit 0
fi
git push --atomic "$remote" "${refs[@]}"
if $tag; then
	git push origin "$src:refs/tags/$version"
fi
