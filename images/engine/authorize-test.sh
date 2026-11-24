#!/bin/bash
#
# quackmux-authorize-test - checks the authorization macro (authorize.sql) of
# a hardened shared engine against a list of cases.
#
#   QUACK_TOKEN=<token> quackmux-authorize-test HOST:PORT [CASES]
#
# A DuckDB client (the CLI of the image) attaches quack:HOST:PORT as "r" over
# plain HTTP with the token in QUACK_TOKEN: the engine token for an engine,
# credential?options for a quackmux gateway in front of one. In this one
# session it runs the cases of CASES (default
# /usr/local/share/quackmux/authorize-cases.tsv, which describes the format)
# one after the other and compares each outcome with the expected one:
#
#   BLOCK  rejected by the macro ("quackmux_authorize: not allowed ...")
#   PERM   refused by DuckDB: no file or network access
#   LOCK   refused by DuckDB: the configuration is locked
#   ERR    an error of any kind
#   OK     no error
#
# Neither the token nor QUACKMUX_TEST_SECRET (for example the engine token
# behind a gateway) may appear in the output of the client. The script prints
# the cases that fail and a summary, and exits with status 0 only if every
# case passes. QUACKMUX_TEST_VERBOSE=1 prints every case,
# QUACKMUX_TEST_OUT=DIR keeps the client's script and output in DIR.
#
# It needs only bash, coreutils, grep and the DuckDB CLI with the quack
# extension (QUACKMUX_DUCKDB_BIN, QUACKMUX_EXTENSION_DIR as for the engine).

set -euo pipefail

prog=${0##*/}
target=${1:-}
cases=${2:-/usr/local/share/quackmux/authorize-cases.tsv}
duckdb_bin=${QUACKMUX_DUCKDB_BIN:-duckdb}
extension_dir=${QUACKMUX_EXTENSION_DIR:-/opt/duckdb/extensions}
timeout=${QUACKMUX_TEST_TIMEOUT:-300}

die() {
	printf '%s: %s\n' "$prog" "$*" >&2
	exit 2
}

if [[ -z $target || $# -gt 2 ]]; then
	die "usage: QUACK_TOKEN=<token> $prog HOST:PORT [CASES]"
fi
if [[ -z ${QUACK_TOKEN:-} ]]; then
	die "QUACK_TOKEN is not set (the token the client presents)"
fi
if [[ ! -f $cases || ! -r $cases ]]; then
	die "cannot read the cases file '$cases'"
fi

if [[ -n ${QUACKMUX_TEST_OUT:-} ]]; then
	work=$QUACKMUX_TEST_OUT
	mkdir -p -- "$work"
else
	work=$(mktemp -d)
	trap 'rm -rf -- "$work"' EXIT
fi

# sql_string VALUE prints VALUE as a single-quoted SQL string literal.
sql_string() {
	local q="'"
	printf "'%s'" "${1//$q/$q$q}"
}

# decode SQL prints SQL with the placeholders of the cases file replaced by
# the characters they stand for (as UTF-8 bytes, whatever the locale).
decode() {
	local s=$1
	s=${s//\{nl\}/$'\n'}
	s=${s//\{tab\}/$'\t'}
	s=${s//\{vt\}/$'\v'}
	s=${s//\{nbsp\}/$'\xc2\xa0'}
	s=${s//\{zwsp\}/$'\xe2\x80\x8b'}
	s=${s//\{idsp\}/$'\xe3\x80\x80'}
	s=${s//\{kelvin\}/$'\xe2\x84\xaa'}
	s=${s//\{longs\}/$'\xc5\xbf'}
	s=${s//\{cyr_a\}/$'\xd0\xb0'}
	s=${s//\{cyr_u\}/$'\xd1\x83'}
	printf '%s' "$s"
}

ids=()
declare -A expect kind sql got
n=0
while IFS=$'\t' read -r id exp k text || [[ -n $id ]]; do
	n=$((n + 1))
	if [[ -z $id || $id == \#* ]]; then
		continue
	fi
	if [[ ! $id =~ ^[a-z][a-z0-9]*$ || -n ${expect[$id]:-} ]]; then
		die "$cases:$n: the id '$id' is invalid or not unique"
	fi
	case $exp in
	BLOCK | PERM | LOCK | ERR | OK) ;;
	*) die "$cases:$n: unknown outcome '$exp'" ;;
	esac
	case $k in
	Q | C) ;;
	*) die "$cases:$n: unknown kind '$k'" ;;
	esac
	if [[ -z $text ]]; then
		die "$cases:$n: no SQL"
	fi
	ids+=("$id")
	expect[$id]=$exp
	kind[$id]=$k
	sql[$id]=$(decode "$text")
done <"$cases"
if ((${#ids[@]} == 0)); then
	die "$cases: no cases"
fi

# Every case is preceded by a marker: an error with its id. Errors go to
# stderr in the order of the statements, so the errors between two markers
# belong to the case of the first one.
(
	umask 077
	{
		printf 'SET extension_directories = [%s];\n' "$(sql_string "$extension_dir")"
		printf 'LOAD quack;\n'
		printf 'CREATE SECRET (TYPE quack, TOKEN %s, SCOPE %s);\n' "$(sql_string "$QUACK_TOKEN")" "$(sql_string "quack:$target")"
		printf 'ATTACH %s AS r (DISABLE_SSL true);\n' "$(sql_string "quack:$target")"
		for id in "${ids[@]}"; do
			printf "SELECT error('quackmux-case %s');\n" "$id"
			if [[ ${kind[$id]} == Q ]]; then
				printf 'FROM r.query(%s);\n' "$(sql_string "${sql[$id]}")"
			else
				printf '%s;\n' "${sql[$id]}"
			fi
		done
		printf "SELECT error('quackmux-case end');\n"
	} >"$work/client.sql"
)
timeout "$timeout" "$duckdb_bin" -init /dev/null -noheader -list <"$work/client.sql" \
	>"$work/client.out" 2>"$work/client.err" || true

current='' preamble=''
while IFS= read -r line || [[ -n $line ]]; do
	if [[ $line =~ ^Invalid\ Input\ Error:\ quackmux-case\ ([a-z0-9]+)$ ]]; then
		current=${BASH_REMATCH[1]}
		got[$current]=
	elif [[ -z $current ]]; then
		preamble+=$line$'\n'
	else
		got[$current]+=$line$'\n'
	fi
done <"$work/client.err"

if [[ $preamble == *Error* ]]; then
	printf '%s: the client could not attach quack:%s:\n' "$prog" "$target"
	printf '%s' "$preamble" | sed 's/^/  | /'
	exit 1
fi

# report RESULT ID [OUTPUT] prints one case and the start of its output.
report() {
	printf '%s %s %s %s %s\n' "$1" "$2" "${expect[$2]}" "${kind[$2]}" "$(printf '%s' "${sql[$2]}" | tr '\n\t\v' '   ' | cut -c1-100)"
	if [[ -n ${3:-} ]]; then
		printf '%s' "$3" | grep -v '^$' | head -n 3 | cut -c1-200 | sed 's/^/    | /' || true
	fi
}

pass=0 failed=0
for id in "${ids[@]}"; do
	ok=0 out=${got[$id]-}
	if [[ -v got[$id] ]]; then
		case ${expect[$id]} in
		BLOCK) [[ $out == *"quackmux_authorize: not allowed on this engine"* ]] && ok=1 ;;
		PERM) [[ $out == *"Permission Error"* && $out != *quackmux_authorize:* ]] && ok=1 ;;
		LOCK) [[ $out == *"the configuration has been locked"* && $out != *quackmux_authorize:* ]] && ok=1 ;;
		ERR) [[ $out == *Error* ]] && ok=1 ;;
		OK) [[ -z $out ]] && ok=1 ;;
		esac
	else
		out="no result: the client stopped before this case"$'\n'
	fi
	if ((ok)); then
		pass=$((pass + 1))
		if [[ ${QUACKMUX_TEST_VERBOSE:-} == 1 ]]; then
			report PASS "$id"
		fi
	else
		failed=$((failed + 1))
		report FAIL "$id" "$out"
	fi
done
if [[ ! -v got[end] ]]; then
	failed=$((failed + 1))
	printf 'FAIL the client did not reach the end of the cases\n'
fi

# grep reads the secrets from a file, so they are not in its command line.
(
	umask 077
	printf '%s\n' "${QUACK_TOKEN%%\?*}" >"$work/secrets"
	if [[ -n ${QUACKMUX_TEST_SECRET:-} ]]; then
		printf '%s\n' "$QUACKMUX_TEST_SECRET" >>"$work/secrets"
	fi
)
if grep -qF -f "$work/secrets" "$work/client.out" "$work/client.err"; then
	failed=$((failed + 1))
	printf 'FAIL a token appeared in the output of the client\n'
fi
rm -f -- "$work/secrets"

printf 'authorization cases: %d passed, %d failed\n' "$pass" "$failed"
((failed == 0))
