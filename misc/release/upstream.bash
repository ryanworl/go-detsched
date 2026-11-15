#!/usr/bin/env bash
# Copyright 2026 Ryan Worl. All rights reserved.
# Use of this source code is governed by a BSD-style
# license that can be found in the LICENSE file.

# upstream.bash takes a new Go release. It imports the release's tree as a
# commit on branch upstream, which holds Go's releases and nothing else,
# and merges that commit into the current branch (main).
#
# Usage: misc/release/upstream.bash [-n] TAG [URL]
#
# TAG is a tag of the Go repository, such as go1.27.5, fetched from URL
# (default https://go.googlesource.com/go). The merge resolves the files
# the fork owns: VERSION gets TAG's version with this branch's -detschedN,
# Go's files under .github stay replaced by the fork's, and Go's
# SECURITY.md and CONTRIBUTING.md stay removed. It commits the
# merge when nothing else conflicts; otherwise it lists the conflicting
# files and leaves the merge for you to finish with git commit. Either way,
# build and run the gate before pushing upstream and main.
#
# With -n it merges in a temporary worktree, reports the conflicts and
# changes no branch.

set -euo pipefail

dryrun=false
if [ "${1:-}" = -n ]; then
	dryrun=true
	shift
fi
if [ $# -lt 1 ] || [ $# -gt 2 ]; then
	echo "usage: misc/release/upstream.bash [-n] TAG [URL]" >&2
	exit 2
fi
tag=$1
url=${2:-https://go.googlesource.com/go}
cd "$(git rev-parse --show-toplevel)"

base=$(git rev-parse --verify -q refs/heads/upstream || git rev-parse --verify origin/upstream)
git fetch --quiet --depth=1 --no-tags "$url" "$tag"
tree=$(git rev-parse "FETCH_HEAD^{tree}")
if [ "$(git rev-parse "$base^{tree}")" = "$tree" ]; then
	echo "upstream: branch upstream already holds $tag"
	import=$base
else
	import=$(git commit-tree "$tree" -p "$base" -m "Import Go $tag")
	echo "upstream: imported $tag as $import"
fi

if git merge-base --is-ancestor "$import" HEAD; then
	echo "upstream: $(git rev-parse --abbrev-ref HEAD) already has $tag"
	exit 0
fi

if $dryrun; then
	work=$(mktemp -d)
	trap 'git worktree remove --force "$work"' EXIT
	git worktree add --quiet --detach "$work" HEAD
	cd "$work"
else
	if [ -n "$(git status --porcelain --untracked-files=no)" ]; then
		echo "upstream: the worktree has uncommitted changes" >&2
		exit 1
	fi
	git branch -f upstream "$import"
fi

git merge --quiet --no-ff --no-commit "$import" >/dev/null 2>&1 || true

# VERSION: Go's new version with this branch's release number, and Go's time.
n=$(git show HEAD:VERSION | sed -n '1s/.*-detsched//p')
{
	echo "$tag-detsched$n"
	git show "$import:VERSION" 2>/dev/null | sed -n '/^time /p' || true
} >VERSION
git add VERSION

# .github, SECURITY.md, CONTRIBUTING.md: the fork's files stay, and Go's
# files stay removed.
owned=(.github SECURITY.md CONTRIBUTING.md)
git diff --name-only --diff-filter=U -z -- "${owned[@]}" | while IFS= read -r -d '' f; do
	if git cat-file -e "HEAD:$f" 2>/dev/null; then
		git checkout --ours -- "$f"
		git add -- "$f"
	else
		git rm --quiet --force -- "$f"
	fi
done
git diff --cached --name-only --diff-filter=A -z HEAD -- "${owned[@]}" | xargs -0 -r git rm --quiet --force --

conflicts=$(git diff --name-only --diff-filter=U)
if $dryrun; then
	git merge --abort
	if [ -z "$conflicts" ]; then
		echo "upstream: $tag merges into $(git rev-parse --short HEAD) without conflicts"
	else
		echo "upstream: $tag conflicts with $(git rev-parse --short HEAD) in $(echo "$conflicts" | wc -l | tr -d ' ') files:"
		echo "$conflicts" | sed 's/^/	/'
	fi
	exit 0
fi
if [ -n "$conflicts" ]; then
	echo "upstream: resolve these files, then git commit:"
	echo "$conflicts" | sed 's/^/	/'
	exit 1
fi
git commit --quiet -m "all: merge Go $tag"
echo "upstream: merged $tag into $(git rev-parse --abbrev-ref HEAD) as $(git rev-parse --short HEAD)"
echo "upstream: next, misc/release/build.bash && misc/release/gate.bash, then push upstream and main"
