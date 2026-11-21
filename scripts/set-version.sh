#!/usr/bin/env bash
#
# set-version.sh VERSION points every reference to the published images at
# release VERSION (without the "v"): ghcr.io/lldeters/quackmux:VERSION and
# ghcr.io/lldeters/duckdb-engine:<duckdb>-VERSION, with <duckdb> the
# DUCKDB_VERSION of the Makefile. See docs/releasing.md.
#
# IMAGE_REGISTRY overrides the registry prefix (default ghcr.io/lldeters).
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")/.."

version=${1:-}
if ! [[ $version =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
	echo "usage: scripts/set-version.sh MAJOR.MINOR.PATCH[-PRERELEASE]" >&2
	exit 2
fi
registry=${IMAGE_REGISTRY:-ghcr.io/lldeters}
duckdb=$(sed -n 's/^DUCKDB_VERSION *?= *//p' Makefile)
re="${registry//./\\.}"

git grep -lE "$re/(quackmux|duckdb-engine):" -- ':!.github' ':!scripts' | while read -r f; do
	# perl, not sed -i: the same command works with GNU and BSD tools.
	VERSION=$version DUCKDB=$duckdb RE=$re perl -pi -e '
		s{($ENV{RE}/quackmux):[0-9][0-9A-Za-z._-]*[0-9A-Za-z_]}{$1:$ENV{VERSION}}g;
		s{($ENV{RE}/duckdb-engine):[0-9][0-9A-Za-z._-]*[0-9A-Za-z_]}{$1:$ENV{DUCKDB}-$ENV{VERSION}}g;
	' "$f"
	echo "updated $f"
done
bash scripts/check-image-refs.sh "$version"
