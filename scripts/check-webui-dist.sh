#!/bin/sh
# Rebuild the playground web UI and verify the committed playground/dist
# matches byte-for-byte. Shared by ci.yml ("Web UI dist freshness") and
# release.yml (pre-GoReleaser gate) so the PR gate and the tag-time gate
# cannot drift apart. Requires deno on PATH — the build output must be
# reproducible, so the deno version is pinned in both workflows. The Docker
# image embeds the committed dist too, without rebuilding it. Node is NOT needed: `deno task build` runs Vite on the
# Deno runtime via @deno/vite-plugin, resolving npm deps from deno.lock.
# Same build recipe as `mage webbuild` (magefiles/magefile.go).
#
# Why dist must be committed and fresh: Go module archives contain only
# git-tracked files, so `go install` embeds whatever dist is in git (#376).
# Vite's content-hashed output is deterministic for a locked dep set
# (deno.lock), so a fresh build must leave the tree byte-identical; any
# difference means playground/src (or the deps) changed without re-committing
# the dist.
set -eu

cd "$(dirname "$0")/../playground"
# --frozen: install exactly what deno.lock pins and fail if the lock is out
# of date (deps bumped in deno.json without re-locking), instead of silently
# resolving fresh versions and validating a dist built against unpinned deps.
deno install --frozen
deno task build
cd ..

# 1. Every built file must be TRACKED. The byte-comparison below is blind to
#    this: `git diff` only looks at tracked files and `git status --porcelain`
#    hides ignored ones, so a dist file that is both untracked and ignored (a
#    re-added `dist/` rule, a new `*.map`, anything caught by playground/
#    .gitignore's `*.local`) is reproduced on disk, passes both checks, and is
#    still absent from the module zip — bug #376 all over again. Comparing the
#    built file list against the index is the only check that sees that.
built=$(find playground/dist -type f | LC_ALL=C sort)
tracked=$(git ls-files -- playground/dist | LC_ALL=C sort)
if [ "$built" != "$tracked" ]; then
	echo "tracked by git:"
	printf "%s\n" "$tracked"
	echo "built on disk:"
	printf "%s\n" "$built"
	echo "::error::playground/dist file list differs from what git tracks — 'git add' the missing build output (a file that is untracked AND gitignored is silently dropped from the module zip, see #376)"
	exit 1
fi

# 2. Every tracked file must match the fresh build byte-for-byte.
if ! git diff --exit-code -- playground/dist; then
	echo "::error::playground/dist is stale — run 'mage webbuild' and commit the result (go install embeds the committed dist, see #376)"
	exit 1
fi
if [ -n "$(git status --porcelain playground/dist)" ]; then
	git status --porcelain playground/dist
	echo "::error::playground/dist has uncommitted build output — commit the rebuilt dist (go install embeds the committed dist, see #376)"
	exit 1
fi
