#!/usr/bin/env bash
# third-party-licenses.sh OUT [PACKAGE] writes OUT/THIRD_PARTY_LICENSES: the
# license and notice files of the Go standard library and of every module whose
# code PACKAGE (default ./cmd/quackmux) links in for the GOOS and GOARCH of the
# environment. The release archives and the gateway image ship it next to
# LICENSE and NOTICE. A module without a license file fails the run.
set -euo pipefail

out=${1:?usage: third-party-licenses.sh OUT [PACKAGE]}
pkg=${2:-./cmd/quackmux}
mkdir -p "$out"
dest=$out/THIRD_PARTY_LICENSES

# section TITLE DIR prints the license and notice files at the top of DIR.
section() {
	local title=$1 dir=$2 found=0 f
	for f in "$dir"/*; do
		[ -f "$f" ] || continue
		case $(basename "$f" | tr '[:lower:]' '[:upper:]') in
		LICENSE* | LICENCE* | COPYING* | NOTICE* | PATENTS*) ;;
		*) continue ;;
		esac
		printf '%s\n%s (%s)\n%s\n\n' "================================================================================" \
			"$title" "$(basename "$f")" "================================================================================"
		cat "$f"
		printf '\n\n'
		found=1
	done
	if [ "$found" = 0 ]; then
		echo "third-party-licenses.sh: no license file for $title in $dir" >&2
		return 1
	fi
}

modules=$(go list -deps -f '{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}} {{.Dir}}{{end}}{{end}}' "$pkg" | sort -u)
{
	echo "Third-party software in the quackmux gateway ($pkg, $(go env GOOS)/$(go env GOARCH)): the Go"
	echo "standard library and the Go modules below, each with its license and notice files."
	echo "quackmux itself is licensed under the Apache License, Version 2.0 (LICENSE, NOTICE)."
	echo
	section "Go standard library and runtime $(go env GOVERSION)" "$(go env GOROOT)"
	while read -r path version dir; do
		[ -n "$path" ] || continue
		if [ -z "$dir" ]; then
			echo "third-party-licenses.sh: module $path $version is not downloaded (go mod download)" >&2
			exit 1
		fi
		section "$path $version" "$dir"
	done <<<"$modules"
} >"$dest"
echo "wrote $dest ($(grep -c '^====' "$dest" | awk '{print $1/2}') license files, $(printf '%s\n' "$modules" | grep -c .) modules)"
