#!/bin/bash
#
# quackmux-engine-selftest - smoke test of the quackmux engine image.
#
# Runs the entrypoint in three scenarios: once with the image defaults, once
# with a database file, init and post-start SQL from the environment and from
# a file, and server settings, once hardened like a shared engine (the
# authorization macro of the image and the post-start settings of its README).
# Each time it waits until the Quack server answers GET /,
# checks that the token shows up neither in a process command line nor in the
# output, stops the engine with SIGTERM and expects exit status 0. In the
# second scenario a Quack client (the DuckDB CLI of the image) checks what a
# session of the engine must not get: the token through FROM _ and the
# standard input of the engine's CLI; in the third one that the macro lets
# ordinary queries through and rejects quack_stop and getenv, and that file
# access is off. Finally it opens the database files and checks that all four
# scripts ran, the post-start SQL while the server ran.
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
authorize_sql=/usr/local/share/quackmux/authorize.sql
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

# check_session NAME PORT LOG runs a Quack client against the engine on PORT
# (whose output goes to LOG). The CLI of the engine answers FROM _ with its
# last result, which must not be the row of quack_serve with the token, and
# its standard input must not be writable through /proc.
check_session() {
	local name=$1 port=$2 log=$3 out=$dir/$1-client.log
	(
		umask 077
		cat >"$dir/client.sql" <<EOF
SET extension_directories = ['${QUACKMUX_EXTENSION_DIR:-/opt/duckdb/extensions}'];
LOAD quack;
CREATE SECRET (TYPE quack, TOKEN '$QUACK_TOKEN', SCOPE 'quack:127.0.0.1:$port');
ATTACH 'quack:127.0.0.1:$port' AS engine (DISABLE_SSL true);
SELECT 'session: ' || quackmux_engine FROM engine.query('FROM _');
FROM engine.query('COPY (SELECT ''.print selftest-injected'') TO ''/proc/self/fd/0'' (FORMAT csv, HEADER false, QUOTE '''')');
EOF
	)
	timeout 60 "$duckdb_bin" -init /dev/null -noheader -list <"$dir/client.sql" >"$out" 2>&1 || true
	if ! grep -q '^session: started$' "$out"; then
		show_log "$out" >&2
		fail "scenario '$name': a session of the engine did not get the harmless last result through FROM _"
	fi
	if grep -qF -f "$dir/token" "$out"; then
		fail "scenario '$name': a session of the engine read the token"
	fi
	sleep 0.5
	if grep -q selftest-injected "$log"; then
		fail "scenario '$name': a session of the engine wrote to the standard input of its CLI"
	fi
	say "scenario '$name': a session gets neither the token nor the CLI's standard input"
}

# check_hardened NAME PORT LOG runs a Quack client against an engine with the
# authorization macro and the post-start settings.
check_hardened() {
	local name=$1 port=$2 out=$dir/$1-client.log
	(
		umask 077
		cat >"$dir/client.sql" <<EOF
SET extension_directories = ['${QUACKMUX_EXTENSION_DIR:-/opt/duckdb/extensions}'];
LOAD quack;
CREATE SECRET (TYPE quack, TOKEN '$QUACK_TOKEN', SCOPE 'quack:127.0.0.1:$port');
ATTACH 'quack:127.0.0.1:$port' AS engine (DISABLE_SSL true);
SELECT 'query: ' || answer FROM engine.selftest_hardened;
FROM engine.query('CALL quack_stop(''quack:0.0.0.0:$port'')');
FROM engine.query('SELECT getenv(''QUACK_TOKEN'')');
FROM engine.query('SELECT * FROM read_text(''/proc/1/environ'')');
SELECT 'still serving: ' || answer FROM engine.selftest_hardened;
EOF
	)
	timeout 60 "$duckdb_bin" -init /dev/null -noheader -list <"$dir/client.sql" >"$out" 2>&1 || true
	if ! grep -q '^query: 42$' "$out" || ! grep -q '^still serving: 42$' "$out"; then
		show_log "$out" >&2
		fail "scenario '$name': an ordinary query of a session failed"
	fi
	if [[ $(grep -c 'quackmux_authorize: not allowed on this engine' "$out") != 2 ]]; then
		show_log "$out" >&2
		fail "scenario '$name': the authorization macro did not reject quack_stop and getenv"
	fi
	if ! grep -q 'Permission Error' "$out"; then
		show_log "$out" >&2
		fail "scenario '$name': a session could read a file"
	fi
	if grep -qF -f "$dir/token" "$out"; then
		fail "scenario '$name': a session of the engine read the token"
	fi
	say "scenario '$name': the macro rejects quack_stop and getenv, file access is off, queries run"
}

# check_duckdb_env fails if a running DuckDB process has VAR in its
# environment (the entrypoint keeps the init and post-start SQL out of it).
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

# run_scenario NAME PORT CHECK [VAR=VALUE...] starts the engine with the given
# environment, probes it on PORT, runs the function CHECK (unless it is "-")
# with NAME, PORT and the engine's log and stops the engine again.
run_scenario() {
	local name=$1 port=$2 check=$3 out pid response rc=0 deadline
	shift 3
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
	check_duckdb_env QUACKMUX_POST_START_SQL
	if [[ $check != - ]]; then
		"$check" "$name" "$port" "$out"
	fi

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

run_scenario defaults "${QUACKMUX_PORT:-9494}" -

cat >"$dir/init.sql" <<'EOF'
-- quackmux engine self-test: a comment with a 'quote
CREATE TABLE selftest_file AS
    SELECT 'it''s from the file' AS s;
EOF
cat >"$dir/post-start.sql" <<'EOF'
-- runs after quack_serve: the server is listed
CREATE TABLE selftest_post_start_file AS
    SELECT 'it''s after the start: ' || count(*) AS s FROM quack_server_list();
EOF
run_scenario configured 19494 check_session \
	QUACKMUX_PORT=19494 \
	QUACKMUX_BIND=0.0.0.0 \
	QUACKMUX_DATABASE="$dir/selftest.duckdb" \
	QUACKMUX_INIT_SQL="CREATE TABLE selftest_env AS SELECT 'it''s from the environment' AS s;" \
	QUACKMUX_INIT_SQL_FILE="$dir/init.sql" \
	QUACKMUX_POST_START_SQL="CREATE TABLE selftest_post_start_env AS SELECT 'it''s after the start: ' || count(*) AS s FROM quack_server_list();" \
	QUACKMUX_POST_START_SQL_FILE="$dir/post-start.sql" \
	QUACKMUX_MAX_CONNECTIONS=64 \
	QUACKMUX_KEEP_ALIVE_TIMEOUT=300 \
	QUACKMUX_ENGINE_ID=quack-selftest \
	QUACKMUX_SHARE_LEVEL=SERVER \
	QUACKMUX_PROFILE=selftest

run_scenario hardened 19495 check_hardened \
	QUACKMUX_PORT=19495 \
	QUACKMUX_DATABASE="$dir/hardened.duckdb" \
	QUACKMUX_INIT_SQL="CREATE OR REPLACE TABLE selftest_hardened AS SELECT 42 AS answer;
.read $authorize_sql" \
	QUACKMUX_POST_START_SQL="SET enable_external_access = false;
SET lock_configuration = true;"
# The engine stopped cleanly with the configuration locked; the table is on
# disk.
if ! result=$("$duckdb_bin" -init /dev/null -bail "$dir/hardened.duckdb" -c "SELECT answer FROM selftest_hardened;" 2>&1) ||
	[[ $result != *42* ]]; then
	fail "reading the database of the hardened engine failed: $result"
fi

# All four scripts must have run, quotes intact, the post-start SQL while
# the server was listed, and the data must be on disk.
if ! result=$("$duckdb_bin" -init /dev/null -bail "$dir/selftest.duckdb" \
	-c "SELECT s FROM selftest_env UNION ALL SELECT s FROM selftest_file UNION ALL SELECT s FROM selftest_post_start_env UNION ALL SELECT s FROM selftest_post_start_file;" 2>&1); then
	fail "reading the database written by the engine failed: $result"
fi
if [[ $result != *"it's from the environment"* || $result != *"it's from the file"* ]]; then
	fail "the init SQL did not run as expected: $result"
fi
if [[ $(grep -c "it's after the start: 1" <<<"$result") != 2 ]]; then
	fail "the post-start SQL did not run as expected (after the start of the server): $result"
fi
say "init SQL from QUACKMUX_INIT_SQL and QUACKMUX_INIT_SQL_FILE ran"
say "post-start SQL from QUACKMUX_POST_START_SQL and QUACKMUX_POST_START_SQL_FILE ran while the server was running"
say "OK"
