#!/usr/bin/env bash
# Point every sub-module at a real published version of its siblings.
#
# A sub-module's go.mod carries a `replace` so the working tree builds
# against itself. That replace is ignored once the module is a dependency,
# so the `require` line is what a consumer actually resolves, and it has to
# name a version that exists. If it names one that does not, `go get` on the
# sub-module fails outright for anyone who is not already requiring the root
# at a high enough version.
#
# Release runs this with the tag it is about to publish, commits the result,
# and tags the sub-modules at that commit.
#
# Usage: scripts/pin-submodules.sh v1.13.0
set -euo pipefail

VERSION="${1:?usage: pin-submodules.sh vX.Y.Z}"
if [[ ! "$VERSION" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
	echo "error: '$VERSION' is not a semver tag like v1.13.0" >&2
	exit 1
fi

cd "$(dirname "$0")/.."

SUBMODULES=(api dashboard sentinel extension integrations/fabriq)

for mod in "${SUBMODULES[@]}"; do
	f="$mod/go.mod"
	[ -f "$f" ] || { echo "error: $f not found" >&2; exit 1; }
	# Rewrite the root require and any sibling sub-module require. Only the
	# github.com/xraph/cortex tree is touched; third-party pins are left to
	# go mod tidy.
	perl -pi -e "s{^(\t)(github\.com/xraph/cortex(?:/\S+)?) v\S+}{\$1\$2 $VERSION}" "$f"
	echo "pinned $f -> $VERSION"
done

# A require the proxy cannot resolve is the whole failure this guards
# against, so fail loudly rather than tagging something broken.
if grep -rn "github.com/xraph/cortex\S* v0\.0\.0" --include=go.mod .; then
	echo "error: a sub-module still requires v0.0.0" >&2
	exit 1
fi
