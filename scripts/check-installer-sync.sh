#!/bin/sh
# Verify the two published copies of each installer are identical, and that
# install.ps1 stays pure ASCII. Used by ci.yml ("Installer scripts").
#
# Why two copies exist: both are live install endpoints documented in
# README.md and docs/site/content/en/docs/install.md.
#
#   install.sh              -> raw.githubusercontent.com/qumo-dev/qumo/main/install.sh
#   docs/site/static/install.sh -> qumo-dev.github.io/qumo/install.sh (GitHub Pages)
#
# Nothing generates one from the other, so an edit to a root script that is
# not mirrored ships a DIFFERENT installer to whichever half of the docs
# points at the stale URL. That is invisible in review — the diff looks
# complete either way.
set -eu

cd "$(dirname "$0")/.."

status=0

# 1. Each root installer must match its docs/site/static twin byte-for-byte.
for f in install.sh install.ps1; do
	if ! cmp -s "$f" "docs/site/static/$f"; then
		diff -u "docs/site/static/$f" "$f" || true
		echo "::error file=docs/site/static/$f::docs/site/static/$f has drifted from $f — both are published install endpoints; run 'cp $f docs/site/static/$f' and commit"
		status=1
	fi
done

# 2. install.ps1 must be pure ASCII, comments included.
#
# The file has no BOM, so Windows PowerShell 5.1 — the interpreter in the
# documented `powershell -c "irm ... | iex"` one-liner — decodes it with the
# system ANSI codepage rather than UTF-8. On a multi-byte codepage (932, 936,
# 949, 950) the trailing byte of a glyph like U+25C7 (0x87) or U+2714 (0x94)
# is itself a lead byte: it consumes the following newline and swallows the
# next line of the script. That silently dropped the $gCheck and $gWarn
# assignments once already (#400), so the glyphs are built from [char] codes
# and named, never pasted, in comments.
for f in install.ps1 docs/site/static/install.ps1; do
	if LC_ALL=C grep -n '[^[:print:][:space:]]' "$f"; then
		echo "::error file=$f::$f contains non-ASCII bytes (listed above) — Windows PowerShell 5.1 misparses them on a multi-byte ANSI codepage; use [char]0xNNNN and name the glyph in comments (see #400)"
		status=1
	fi
done

exit $status
