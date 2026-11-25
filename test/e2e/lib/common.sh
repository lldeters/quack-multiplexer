# shellcheck shell=bash
#
# Helpers shared by the quackmux end-to-end tests (compose/run.sh and the kind
# scripts). Source this file; do not execute it.
#
# Everything here works with bash 3.2 (the /bin/bash of macOS): no associative
# arrays, no "{fd}>" redirections, no "wait -n", no "${var,,}".
#
# Callers must set E2E_WORK (a scratch directory) before they use the HTTP,
# metrics or session helpers, and REPO_ROOT before they call build_images.

# Output ---------------------------------------------------------------------

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
	C_BOLD=$'\033[1m' C_DIM=$'\033[2m' C_RED=$'\033[31m' C_GREEN=$'\033[32m' C_YELLOW=$'\033[33m' C_OFF=$'\033[0m'
else
	C_BOLD='' C_DIM='' C_RED='' C_GREEN='' C_YELLOW='' C_OFF=''
fi

say() { printf '%s%s%s\n' "$C_DIM" "$*" "$C_OFF"; }
step() { printf '\n%s==> %s%s\n' "$C_BOLD" "$*" "$C_OFF"; }
ok() { printf '  %sok%s    %s\n' "$C_GREEN" "$C_OFF" "$*"; }
warn() { printf '  %swarn%s  %s\n' "$C_YELLOW" "$C_OFF" "$*" >&2; }
# bad reports a failed check. It returns 0, so callers decide what happens
# next (usually "bad ...; return 1").
bad() { printf '  %sFAIL%s  %s\n' "$C_RED" "$C_OFF" "$*" >&2; }
die() {
	printf '%serror:%s %s\n' "$C_RED" "$C_OFF" "$*" >&2
	exit 1
}

# indent prints files (or stdin) indented on stderr, as details of a failure.
indent() { sed 's/^/        | /' "$@" >&2; }

# Basics ---------------------------------------------------------------------

need() {
	local c
	for c in "$@"; do
		command -v "$c" >/dev/null 2>&1 || die "required command not found: $c"
	done
}

now() { date +%s; }

make_workdir() { mktemp -d "${TMPDIR:-/tmp}/quackmux-e2e.XXXXXX"; }

# sleep_until UNIX_TIME sleeps until the given time (if it is in the future).
sleep_until() {
	local d=$(($1 - $(now)))
	if [ "$d" -gt 0 ]; then sleep "$d"; fi
}

# nlines TEXT prints the number of non-empty lines of TEXT.
nlines() { printf '%s\n' "$1" | grep -c . || true; }

# list_remove LIST WORD prints the space separated LIST without WORD.
list_remove() {
	local out='' x
	for x in $1; do
		[ "$x" = "$2" ] || out="$out $x"
	done
	printf '%s' "${out# }"
}

# wait_for TIMEOUT DESCRIPTION COMMAND [ARG...] runs COMMAND (quietly) once a
# second until it succeeds. It fails after TIMEOUT seconds.
wait_for() {
	local timeout=$1 what=$2 deadline
	shift 2
	deadline=$(($(now) + timeout))
	until "$@" >/dev/null 2>&1; do
		if [ "$(now)" -ge "$deadline" ]; then
			bad "timed out after ${timeout}s waiting for $what"
			return 1
		fi
		sleep 1
	done
}

# Checks ---------------------------------------------------------------------
# Every check prints "ok DESCRIPTION" or "FAIL DESCRIPTION: details" and
# returns 0 or 1.

# expect_status ACTUAL WANT DESCRIPTION; WANT is an exit status or "nonzero".
expect_status() {
	local actual=$1 want=$2 what=$3
	if [ "$want" = nonzero ]; then
		if [ "$actual" -ne 0 ]; then
			ok "$what"
			return 0
		fi
	elif [ "$actual" -eq "$want" ]; then
		ok "$what"
		return 0
	fi
	bad "$what: exit status $actual, want $want"
	return 1
}

# expect_eq ACTUAL WANT DESCRIPTION
expect_eq() {
	if [ "$1" = "$2" ]; then
		ok "$3"
		return 0
	fi
	bad "$3: got '$1', want '$2'"
	return 1
}

# expect_ge ACTUAL MINIMUM DESCRIPTION (integers)
expect_ge() {
	if [ -n "$1" ] && [ "$1" -ge "$2" ] 2>/dev/null; then
		ok "$3"
		return 0
	fi
	bad "$3: got '$1', want at least $2"
	return 1
}

# expect_output FILE TEXT DESCRIPTION: FILE contains TEXT (a fixed string).
expect_output() {
	if grep -qF -- "$2" "$1"; then
		ok "$3"
		return 0
	fi
	bad "$3: the output does not contain '$2'"
	return 1
}

# expect_no_output FILE TEXT DESCRIPTION: FILE does not contain TEXT.
expect_no_output() {
	if ! grep -qF -- "$2" "$1"; then
		ok "$3"
		return 0
	fi
	bad "$3: the output unexpectedly contains '$2'"
	return 1
}

# Admin API and metrics -------------------------------------------------------

# api METHOD BASE_URL PATH OUTFILE calls the gateway's admin API. The body goes
# to OUTFILE and the status code to HTTP_STATUS ("000" if the request failed).
# ADMIN_HEADER_FILE, if set, holds the "Authorization: Bearer ..." header; a
# file keeps the token out of process listings.
api() {
	local method=$1 url=$2$3 out=$4
	HTTP_STATUS=$(curl -sS -X "$method" -o "$out" -w '%{http_code}' \
		${ADMIN_HEADER_FILE:+-H "@$ADMIN_HEADER_FILE"} "$url" 2>/dev/null) || HTTP_STATUS=000
}

# The admin API pretty-prints its JSON with one field per line, so plain text
# tools are enough to read it (no jq needed).

# json_value FILE KEY prints the first value of "KEY": VALUE, without quotes.
json_value() {
	awk -v k="\"$2\":" '$1 == k { v = $2; sub(/,$/, "", v); gsub(/"/, "", v); print v; exit }' "$1"
}

# json_count FILE KEY RAW_VALUE counts the fields "KEY": RAW_VALUE. String
# values must be given with their quotes ('"READY"'), others without (true).
json_count() {
	awk -v k="\"$2\":" -v want="$3" '$1 == k { v = $2; sub(/,$/, "", v); if (v == want) n++ } END { print n + 0 }' "$1"
}

# session_count BASE_URL USER prints the number of open sessions of USER.
session_count() {
	local out
	out=$(mktemp "$E2E_WORK/sessions.XXXXXX")
	api GET "$1" "/api/v1/sessions?user=$2" "$out"
	if [ "$HTTP_STATUS" != 200 ]; then
		rm -f "$out"
		return 1
	fi
	json_value "$out" count
	rm -f "$out"
}

# session_count_is BASE_URL USER N succeeds if USER has exactly N sessions.
session_count_is() { [ "$(session_count "$1" "$2")" = "$3" ]; }

# expect_sessions BASE_URL USER N DESCRIPTION
expect_sessions() {
	local base=$1 user=$2 want=$3 what=$4 out n
	out=$E2E_WORK/sessions-$user.json
	api GET "$base" "/api/v1/sessions?user=$user" "$out"
	if [ "$HTTP_STATUS" != 200 ]; then
		bad "$what: GET /api/v1/sessions?user=$user answered HTTP $HTTP_STATUS"
		indent "$out"
		return 1
	fi
	n=$(json_value "$out" count)
	if [ "$n" != "$want" ]; then
		bad "$what: $user has ${n:-?} open session(s), want $want"
		indent "$out"
		return 1
	fi
	ok "$what"
}

# metric_sum BASE_URL NAME [FILTER...] sums the samples of the Prometheus
# metric NAME (from BASE_URL/metrics) whose labels contain every FILTER, for
# example 'type="SEND_DATA_REQUEST"'. It prints 0 if there is no such sample.
# A metric without labels matches without FILTER.
metric_sum() {
	local base=$1 name=$2 file
	shift 2
	file=$(mktemp "$E2E_WORK/metrics.XXXXXX")
	if ! curl -fsS -o "$file" "$base/metrics" 2>/dev/null; then
		rm -f "$file"
		echo 0
		return 1
	fi
	awk -v name="$name" -v filters="$(printf '%s\034' "$@")" '
		BEGIN { n = split(filters, f, "\034") }
		index($0, name "{") == 1 || index($0, name " ") == 1 {
			for (i = 1; i <= n; i++) if (f[i] != "" && index($0, f[i]) == 0) next
			sum += $NF
		}
		END { printf "%.0f\n", sum }' "$file"
	rm -f "$file"
}

# DuckDB clients ---------------------------------------------------------------

# Extension directory inside the engine image (images/engine installs quack
# and httpfs there). Set it to an empty string to use DuckDB's default.
E2E_EXTENSION_DIR=${E2E_EXTENSION_DIR-/opt/duckdb/extensions}

# client_preamble prints the SQL every client starts with: list output and the
# extensions of the image (never downloaded: clusters may be air-gapped).
client_preamble() {
	echo ".mode list"
	if [ -n "$E2E_EXTENSION_DIR" ]; then
		printf "SET extension_directories = ['%s'];\n" "$E2E_EXTENSION_DIR"
	fi
	echo "SET autoinstall_known_extensions = false;"
	echo "LOAD httpfs;"
	echo "LOAD quack;"
}

# Interactive sessions -----------------------------------------------------------
#
# A session is a command (normally a DuckDB CLI in a container) that runs in
# the background with its standard input connected to a FIFO:
#
#   session_start ID CMD [ARG...]   start CMD; stdout and stderr go to $E2E_WORK/ID.log
#   session_send ID SQL             write SQL (one or more lines) to it
#   session_wait ID TEXT TIMEOUT [FAIL_TEXT]
#                                   wait until the output since the last
#                                   session_send contains TEXT; fail at once if
#                                   it contains FAIL_TEXT (default "Error", the
#                                   suffix of every DuckDB error type; pass ""
#                                   when an error is expected)
#   session_end ID [TIMEOUT]        send .quit, close the input, wait for the
#                                   exit; returns the exit status of CMD
#
# The DuckDB CLI flushes its output before it reads the next input line, so
# the result of a statement is in the log as soon as the statement finished.
# CMD must be an executable (not a shell function). Session ids must be shell
# identifiers.

SESSION_IDS=''
SESSION_FREE_FDS='3 4 5 6 7 8 9'
SESSION_USED_FDS=''

session_get() { eval "printf '%s' \"\${SESSION_${1}_$2:-}\""; }

# close_session_fds closes the FIFO writers of all sessions in the current
# (sub)shell. Background processes must not inherit them: a session whose
# writer is held open elsewhere never sees the end of its input.
close_session_fds() {
	local f
	for f in $SESSION_USED_FDS; do
		eval "exec $f>&-"
	done
}

session_start() {
	local id=$1 fd fifo log
	shift
	case $id in
	'' | *[!A-Za-z0-9_]*) die "invalid session id '$id'" ;;
	esac
	fd=${SESSION_FREE_FDS%% *}
	[ -n "$fd" ] || die "too many concurrent sessions"
	SESSION_FREE_FDS=$(list_remove "$SESSION_FREE_FDS" "$fd")
	fifo=$E2E_WORK/$id.fifo
	log=$E2E_WORK/$id.log
	rm -f "$fifo"
	mkfifo "$fifo"
	: >"$log"
	(
		close_session_fds
		exec "$@"
	) <"$fifo" >"$log" 2>&1 &
	eval "SESSION_PID_$id=\$!"
	# Opening the writer unblocks the reader side opened by the child.
	eval "exec $fd>\"\$fifo\""
	eval "SESSION_FD_$id=\$fd SESSION_OFF_$id=0"
	SESSION_USED_FDS="$SESSION_USED_FDS $fd"
	SESSION_IDS="$SESSION_IDS $id"
}

session_send() {
	local id=$1 fd pid size
	shift
	fd=$(session_get FD "$id")
	pid=$(session_get PID "$id")
	if [ -z "$fd" ]; then
		bad "session $id is not running"
		return 1
	fi
	if ! kill -0 "$pid" 2>/dev/null; then
		bad "session $id has exited; its output:"
		indent "$E2E_WORK/$id.log"
		return 1
	fi
	# shellcheck disable=SC2034 # read through the eval below
	size=$(wc -c <"$E2E_WORK/$id.log" | tr -d ' ')
	eval "SESSION_OFF_$id=\$size"
	if ! printf '%s\n' "$*" >&"$fd"; then
		bad "cannot write to session $id"
		return 1
	fi
}

# session_output ID prints the output of the session since the last
# session_send.
session_output() {
	local off
	off=$(session_get OFF "$1")
	tail -c +$((${off:-0} + 1)) "$E2E_WORK/$1.log"
}

session_wait() {
	local id=$1 text=$2 timeout=$3 failtext=${4-Error} pid deadline out
	pid=$(session_get PID "$id")
	deadline=$(($(now) + timeout))
	while :; do
		out=$(session_output "$id")
		case $out in *"$text"*) return 0 ;; esac
		if [ -n "$failtext" ]; then
			case $out in
			*"$failtext"*)
				bad "session $id printed '$failtext' instead of '$text':"
				printf '%s\n' "$out" | indent
				return 1
				;;
			esac
		fi
		if ! kill -0 "$pid" 2>/dev/null; then
			out=$(session_output "$id")
			case $out in *"$text"*) return 0 ;; esac
			bad "session $id ended before it printed '$text':"
			printf '%s\n' "$out" | indent
			return 1
		fi
		if [ "$(now)" -ge "$deadline" ]; then
			bad "timed out after ${timeout}s waiting for '$text' in session $id:"
			printf '%s\n' "$out" | indent
			return 1
		fi
		sleep 0.5
	done
}

session_end() {
	local id=$1 timeout=${2:-60} fd pid rc=0 deadline
	fd=$(session_get FD "$id")
	pid=$(session_get PID "$id")
	[ -n "$pid" ] || return 0
	if [ -n "$fd" ]; then
		printf '.quit\n' 1>&"$fd" 2>/dev/null || true
		eval "exec $fd>&-"
		SESSION_USED_FDS=$(list_remove "$SESSION_USED_FDS" "$fd")
		SESSION_FREE_FDS="${SESSION_FREE_FDS:+$SESSION_FREE_FDS }$fd"
	fi
	deadline=$(($(now) + timeout))
	while kill -0 "$pid" 2>/dev/null; do
		if [ "$(now)" -ge "$deadline" ]; then
			warn "session $id did not exit within ${timeout}s; killing it"
			kill "$pid" 2>/dev/null || true
			break
		fi
		sleep 0.5
	done
	wait "$pid" 2>/dev/null || rc=$?
	eval "SESSION_PID_$id='' SESSION_FD_$id=''"
	SESSION_IDS=$(list_remove "$SESSION_IDS" "$id")
	return "$rc"
}

# session_kill_all stops every session that is still running (cleanup).
session_kill_all() {
	local id pid fd
	for id in $SESSION_IDS; do
		fd=$(session_get FD "$id")
		pid=$(session_get PID "$id")
		if [ -n "$fd" ]; then eval "exec $fd>&-" 2>/dev/null || true; fi
		if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; fi
	done
	SESSION_IDS=''
	SESSION_USED_FDS=''
	SESSION_FREE_FDS='3 4 5 6 7 8 9'
}

# Scenarios ------------------------------------------------------------------

SCENARIOS_PASSED=0
SCENARIOS_FAILED=''

# run_scenario NAME TITLE FUNCTION runs one scenario and records the result.
# A failing scenario does not stop the run. If the caller defines
# scenario_failed_hook, it is called with NAME after a failure.
#
# FUNCTION runs as an "if" condition, where "set -e" has no effect: scenario
# functions check every step themselves and return 1 on failure.
run_scenario() {
	local name=$1 title=$2 fn=$3
	step "[$name] $title"
	if "$fn"; then
		SCENARIOS_PASSED=$((SCENARIOS_PASSED + 1))
	else
		SCENARIOS_FAILED="$SCENARIOS_FAILED $name"
		bad "scenario $name failed"
		if declare -F scenario_failed_hook >/dev/null; then scenario_failed_hook "$name"; fi
	fi
}

# summary TITLE prints the result of all scenarios; it fails if one failed.
summary() {
	if [ -z "$SCENARIOS_FAILED" ]; then
		printf '\n%s%s: all %d scenarios passed%s\n' "$C_GREEN" "$1" "$SCENARIOS_PASSED" "$C_OFF"
		return 0
	fi
	printf '\n%s%s: %d passed, failed:%s%s\n' "$C_RED" "$1" "$SCENARIOS_PASSED" "$SCENARIOS_FAILED" "$C_OFF" >&2
	return 1
}

# Images ---------------------------------------------------------------------

# build_images ENGINE_IMAGE GATEWAY_IMAGE builds the engine image from
# images/engine and the gateway image from the root Dockerfile.
# DOCKER_BUILD_FLAGS (split at spaces, as in the Makefile) are passed to both
# builds, e.g. "--network host --build-arg GOPROXY=..." behind a proxy.
build_images() {
	[ -f "$REPO_ROOT/images/engine/Dockerfile" ] || die "$REPO_ROOT/images/engine/Dockerfile not found"
	[ -f "$REPO_ROOT/Dockerfile" ] || die "$REPO_ROOT/Dockerfile not found"
	local v sha_amd64 sha_arm64
	# The DuckDB version and the checksums of its CLI archives (Makefile).
	v=$(sed -n 's/^DUCKDB_VERSION *?= *//p' "$REPO_ROOT/Makefile")
	sha_amd64=$(sed -n 's/^DUCKDB_SHA256_AMD64 *?= *//p' "$REPO_ROOT/Makefile")
	sha_arm64=$(sed -n 's/^DUCKDB_SHA256_ARM64 *?= *//p' "$REPO_ROOT/Makefile")
	say "building engine image $1 (images/engine, DuckDB ${v:-default})"
	# shellcheck disable=SC2086 # DOCKER_BUILD_FLAGS is a list of flags
	docker build ${DOCKER_BUILD_FLAGS:-} ${v:+--build-arg DUCKDB_VERSION=$v} \
		${sha_amd64:+--build-arg DUCKDB_SHA256_AMD64=$sha_amd64} ${sha_arm64:+--build-arg DUCKDB_SHA256_ARM64=$sha_arm64} \
		-t "$1" "$REPO_ROOT/images/engine"
	say "building gateway image $2 (Dockerfile)"
	# shellcheck disable=SC2086
	docker build ${DOCKER_BUILD_FLAGS:-} -t "$2" -f "$REPO_ROOT/Dockerfile" "$REPO_ROOT"
}
