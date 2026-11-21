#!/usr/bin/env bash
#
# check-image-refs.sh [VERSION] checks that the repository references exactly
# one release of the published images:
#
#   ghcr.io/lldeters/quackmux:<version>
#   ghcr.io/lldeters/duckdb-engine:<duckdb>-<version>
#
# with <duckdb> the DUCKDB_VERSION of the Makefile. With VERSION (the release
# workflow passes the tag without its "v"), <version> must be that one.
# scripts/set-version.sh changes all references at once (docs/releasing.md).
#
# IMAGE_REGISTRY overrides the registry prefix (default ghcr.io/lldeters).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

registry=${IMAGE_REGISTRY:-ghcr.io/lldeters}
want=${1:-}
duckdb=$(sed -n 's/^DUCKDB_VERSION *?= *//p' Makefile)
if [ -z "$duckdb" ]; then
	echo "check-image-refs: no DUCKDB_VERSION in the Makefile" >&2
	exit 1
fi
# A release tag starts with a digit (placeholders such as X.Y.Z in the docs do
# not count) and ends with a letter or digit, not with the full stop of a
# sentence.
re="${registry//./\\.}/(quackmux|duckdb-engine):[0-9][0-9A-Za-z._-]*[0-9A-Za-z_]"

refs=$(git grep -hoE "$re" -- ':!.github' ':!scripts' | sort -u)
if [ -z "$refs" ]; then
	echo "check-image-refs: no references to images in $registry" >&2
	exit 1
fi

versions=''
bad=0
while read -r ref; do
	tag=${ref##*:}
	case $ref in
	*/quackmux:*)
		versions+="$tag"$'\n'
		;;
	*/duckdb-engine:*)
		if [[ $tag != "$duckdb"-* ]]; then
			echo "check-image-refs: $ref: the engine tag must be $duckdb-<version> (DUCKDB_VERSION $duckdb)" >&2
			bad=1
			continue
		fi
		versions+="${tag#"$duckdb"-}"$'\n'
		;;
	esac
done <<<"$refs"

versions=$(printf '%s' "$versions" | sort -u)
if [ "$(printf '%s\n' "$versions" | grep -c .)" -ne 1 ]; then
	echo "check-image-refs: the references name more than one release ($(echo "$versions" | tr '\n' ' ')):" >&2
	git grep -nE "$re" -- ':!.github' ':!scripts' >&2
	bad=1
elif [ -n "$want" ] && [ "$versions" != "$want" ]; then
	echo "check-image-refs: the references name release $versions, not $want (run scripts/set-version.sh $want):" >&2
	git grep -nE "$re" -- ':!.github' ':!scripts' >&2
	bad=1
fi
if [ "$bad" -ne 0 ]; then
	exit 1
fi
echo "image references: release $versions ($registry/quackmux:$versions, $registry/duckdb-engine:$duckdb-$versions)"
