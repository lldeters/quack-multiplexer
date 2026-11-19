#!/bin/bash
#
# quackmux-engine-selftest - smoke test of the quackmux engine image.
#
# Runs the entrypoint in two scenarios: once with the image defaults, once
# with a database file, init SQL from the environment and from a file, and
# server settings. Each time it waits until the Quack server answers GET /,
# checks that the token shows up neither in a process command line nor in the
# output, stops the engine with SIGTERM and expects exit status 0. Finally it
# opens the database file and checks that both init scripts ran.
#
# The image build runs it (build argument ENGINE_SELFTEST=true). It also works
# in derived images:
#
#   docker run --rm --entrypoint quackmux-engine-selftest IMAGE
#
# It needs only bash, coreutils and grep; HTTP requests go through /dev/tcp.
#
#   quackmux-engine-selftest [ENTRYPOINT]

set -euo pipefail

entrypoint=${1:-/usr/local/bin/quackmux-engine}
duckdb_bin=${QUACKMUX_DUCKDB_BIN:-duckdb}
timeout=${SELFTEST_TIMEOUT:-120}

say() {
	printf 'selftest: %s\n' "$*"
}

fail() {
	printf 'selftest: FAILED: %s\n' "$*" >&2
	exit 1
}

dir=$(mktemp -d)
trap 'rm -rf -- "$dir"' EXIT

# A random token that must not appear anywhere.
QUACK_TOKEN="selftest-$(od -An -N24 -tx1 /dev/urandom | tr -d ' \n')"
export QUACK_TOKEN
# grep reads the token from this file, so it is not in grep's command line.
printf '%s\n' "$QUACK_TOKEN" >"$dir/token"

# http_get PORT prints the response to GET / on 127.0.0.1:PORT.
http_get() {
	(
		exec 4<>"/dev/tcp/127.0.0.1/$1"
		printf 'GET / HTTP/1.0\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n' >&4
		timeout 5 cat <&4
	) 2>/dev/null
}

show_log() {
	sed 's/^/  | /' "$1"
}

# check_duckdb_env fails if a running DuckDB process has VAR in its
# environment (the entrypoint keeps the init SQL out of it).
check_duckdb_env() {
	local var=$1 proc environ
	for proc in /proc/[0-9]*; do
		if [[ $(cat "$proc/comm" 2>/dev/null || true) != duckdb ]]; then
			continue
		fi
		environ=$(tr '\0' '\n' <"$proc/environ" 2>/dev/null || true)
		if [[ $'\n'$environ == *$'\n'"$var="* ]]; then
			fail "$var is in the environment of DuckDB (${proc#/proc/})"
		fi
	done
}

# run_scenario NAME PORT [VAR=VALUE...] starts the engine with the given
# environment, probes it on PORT and stops it again.
run_scenario() {
	local name=$1 port=$2 out pid response rc=0 deadline
	shift 2
	out=$dir/$name.log
	say "scenario '$name': starting the engine"
	env "$@" "$entrypoint" >"$out" 2>&1 &
	pid=$!
	deadline=$((SECONDS + timeout))
	# Wait for the server and for the entrypoint to report it, so that the
	# SIGTERM below hits a serving engine.
	until response=$(http_get "$port") && [[ $response == "HTTP/1."?" 200"* ]] &&
		grep -q 'serving on quack:' "$out"; do
		if ! kill -0 "$pid" 2>/dev/null; then
			show_log "$out" >&2
			fail "scenario '$name': the engine exited before it served"
		fi
		if ((SECONDS >= deadline)); then
			kill -KILL "$pid" 2>/dev/null || true
			show_log "$out" >&2
			fail "scenario '$name': the engine did not serve on port $port within ${timeout}s"
		fi
		sleep 0.2
	done
	if [[ $response != *Quack* ]]; then
		kill -KILL "$pid" 2>/dev/null || true
		fail "scenario '$name': unexpected answer to GET /: $response"
	fi
	say "scenario '$name': GET / answered"

	if grep -lsF -f "$dir/token" /proc/[0-9]*/cmdline; then
		kill -KILL "$pid" 2>/dev/null || true
		fail "scenario '$name': the token is visible in a process command line"
	fi
	check_duckdb_env QUACKMUX_INIT_SQL

	kill -TERM "$pid"
	wait "$pid" || rc=$?
	show_log "$out"
	if ((rc != 0)); then
		fail "scenario '$name': the engine exited with status $rc after SIGTERM"
	fi
	if grep -qF -f "$dir/token" "$out"; then
		fail "scenario '$name': the token appeared in the output"
	fi
	say "scenario '$name': stopped cleanly"
}

run_scenario defaults "${QUACKMUX_PORT:-9494}"

cat >"$dir/init.sql" <<'EOF'
-- quackmux engine self-test: a comment with a 'quote
CREATE TABLE selftest_file AS
    SELECT 'it''s from the file' AS s;
EOF
run_scenario configured 19494 \
	QUACKMUX_PORT=19494 \
	QUACKMUX_BIND=0.0.0.0 \
	QUACKMUX_DATABASE="$dir/selftest.duckdb" \
	QUACKMUX_INIT_SQL="CREATE TABLE selftest_env AS SELECT 'it''s from the environment' AS s;" \
	QUACKMUX_INIT_SQL_FILE="$dir/init.sql" \
	QUACKMUX_MAX_CONNECTIONS=64 \
	QUACKMUX_KEEP_ALIVE_TIMEOUT=300 \
	QUACKMUX_ENGINE_ID=quack-selftest \
	QUACKMUX_SHARE_LEVEL=SERVER \
	QUACKMUX_PROFILE=selftest

# Both init scripts must have run, quotes intact, and the data must be on disk.
if ! result=$("$duckdb_bin" -init /dev/null -bail "$dir/selftest.duckdb" \
	-c "SELECT s FROM selftest_env UNION ALL SELECT s FROM selftest_file;" 2>&1); then
	fail "reading the database written by the engine failed: $result"
fi
if [[ $result != *"it's from the environment"* || $result != *"it's from the file"* ]]; then
	fail "the init SQL did not run as expected: $result"
fi
say "init SQL from QUACKMUX_INIT_SQL and QUACKMUX_INIT_SQL_FILE ran"
say "OK"
