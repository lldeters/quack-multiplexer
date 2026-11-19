#!/bin/bash
#
# quackmux-engine - entrypoint of the quackmux DuckDB engine image.
#
# Implements the engine container contract of quackmux (see
# internal/engine/initsql.go and images/engine/README.md): it opens
# QUACKMUX_DATABASE with the DuckDB CLI, loads the quack extension, runs the
# init SQL and then serves the Quack protocol over plain HTTP on
# QUACKMUX_BIND:QUACKMUX_PORT (default 0.0.0.0:9494) with the token from
# QUACK_TOKEN. It stays in the foreground until SIGTERM, SIGINT or SIGHUP and
# then stops the server gracefully. Run it under an init process that forwards
# signals (the image uses tini).
#
# The DuckDB CLI has no daemon mode: it serves only while it runs, and it exits
# as soon as its stdin reaches EOF. Its stdin is therefore a FIFO in a private
# directory that this script holds open read-write on fd 3, and all statements
# are written to fd 3 instead of being passed as arguments. The init SQL is
# handed over as private files that DuckDB reads with .read, so it is executed
# byte for byte and can contain any quoting. The token is read inside DuckDB
# with getenv('QUACK_TOKEN'); it never appears in a command line, a statement
# or a log line. Never enable xtrace (set -x) in this script.

set -euo pipefail

readonly prog=quackmux-engine
# Exit status for configuration errors (EX_CONFIG in sysexits.h).
readonly ex_config=78
# Characters allowed in paths that are written into DuckDB dot commands.
readonly safe_path_re='^[A-Za-z0-9._/-]+$'
readonly bind_re='^[][A-Za-z0-9._:-]+$'

# State shared with the signal handler.
signal_count=0   # TERM/INT/HUP signals received so far
stop_requested=0 # set by the first signal
force_kill=0     # set by a second signal: kill DuckDB without waiting

workdir=       # private directory with the FIFO, removed once DuckDB serves
duckdb_pid=    # PID of the DuckDB CLI
duckdb_status= # exit status of the DuckDB CLI once collected
serving=0      # 1 once quack_serve has succeeded

log() {
	printf '%s: %s\n' "$prog" "$*" >&2
}

config_error() {
	log "configuration error: $*"
	exit "$ex_config"
}

# printable VALUE prints VALUE with control characters replaced (log lines).
printable() {
	printf '%s' "${1//[[:cntrl:]]/?}"
}

# sql_string VALUE prints VALUE as a single-quoted SQL string literal.
sql_string() {
	local q="'"
	printf "'%s'" "${1//$q/$q$q}"
}

# positive_int VALUE succeeds for decimal integers from 1 to 999999999.
positive_int() {
	[[ $1 =~ ^[0-9]{1,9}$ ]] && ((10#$1 > 0))
}

on_signal() {
	signal_count=$((signal_count + 1))
	if ((stop_requested)); then
		force_kill=1
		log "received SIG$1 again, killing DuckDB"
	else
		stop_requested=1
		log "received SIG$1, shutting down"
	fi
}

cleanup() {
	if [[ -n $workdir ]]; then
		rm -rf -- "$workdir" || true
	fi
}

read_settings() {
	if [[ -z ${QUACK_TOKEN:-} ]]; then
		config_error "QUACK_TOKEN is not set (it holds the token that clients, normally the quackmux gateway, must present)"
	fi
	if ((${#QUACK_TOKEN} < 16)); then
		config_error "QUACK_TOKEN is too short (${#QUACK_TOKEN} characters, at least 16 are required)"
	fi

	database=${QUACKMUX_DATABASE:-:memory:}
	case $database in
	-*) config_error "QUACKMUX_DATABASE must not start with '-'" ;;
	*[[:cntrl:]]*) config_error "QUACKMUX_DATABASE must not contain control characters" ;;
	esac

	port=${QUACKMUX_PORT:-9494}
	if [[ ! $port =~ ^[0-9]{1,5}$ ]] || ((10#$port < 1 || 10#$port > 65535)); then
		config_error "QUACKMUX_PORT must be a port number from 1 to 65535, got '$(printable "$port")'"
	fi
	port=$((10#$port))

	bind=${QUACKMUX_BIND:-0.0.0.0}
	if [[ ! $bind =~ $bind_re ]]; then
		config_error "QUACKMUX_BIND must be a host name or an IP address, got '$(printable "$bind")'"
	fi
	uri="quack:$bind:$port"

	extension_dir=${QUACKMUX_EXTENSION_DIR:-/opt/duckdb/extensions}
	if [[ $extension_dir == *[[:cntrl:]]* ]]; then
		config_error "QUACKMUX_EXTENSION_DIR must not contain control characters"
	fi

	init_sql_file=${QUACKMUX_INIT_SQL_FILE:-}
	if [[ -n $init_sql_file && (! -f $init_sql_file || ! -r $init_sql_file) ]]; then
		config_error "QUACKMUX_INIT_SQL_FILE '$(printable "$init_sql_file")' is not a readable file"
	fi

	max_connections=${QUACKMUX_MAX_CONNECTIONS:-}
	if [[ -n $max_connections ]]; then
		if ! positive_int "$max_connections"; then
			config_error "QUACKMUX_MAX_CONNECTIONS must be a positive integer, got '$(printable "$max_connections")'"
		fi
		max_connections=$((10#$max_connections))
	fi

	keep_alive_timeout=${QUACKMUX_KEEP_ALIVE_TIMEOUT:-}
	if [[ -n $keep_alive_timeout ]]; then
		if ! positive_int "$keep_alive_timeout"; then
			config_error "QUACKMUX_KEEP_ALIVE_TIMEOUT must be a positive number of seconds, got '$(printable "$keep_alive_timeout")'"
		fi
		keep_alive_timeout=$((10#$keep_alive_timeout))
	fi

	stop_timeout=${QUACKMUX_STOP_TIMEOUT:-20}
	stop_timeout=${stop_timeout%s}
	if [[ ! $stop_timeout =~ ^[0-9]{1,6}$ ]]; then
		config_error "QUACKMUX_STOP_TIMEOUT must be a number of seconds such as 20 or 20s, got '$(printable "${QUACKMUX_STOP_TIMEOUT:-}")'"
	fi
	stop_timeout=$((10#$stop_timeout))

	duckdb_bin=${QUACKMUX_DUCKDB_BIN:-duckdb}
	if ! command -v "$duckdb_bin" >/dev/null 2>&1; then
		config_error "DuckDB CLI '$(printable "$duckdb_bin")' not found (QUACKMUX_DUCKDB_BIN)"
	fi
}

# is_local_file_database succeeds when QUACKMUX_DATABASE names a local
# database file (not ":memory:" and no URL or extension prefix like "md:").
is_local_file_database() {
	case $database in
	:memory:*) return 1 ;;
	esac
	[[ ! $database =~ ^[A-Za-z][A-Za-z0-9+.-]*: ]]
}

# engine_details describes the engine for log lines. The database is shown
# without its query string, which may carry credentials.
engine_details() {
	local details
	details="database $(printable "${database%%\?*}")"
	if [[ -n ${QUACKMUX_ENGINE_ID:-} ]]; then
		details+=", engine $(printable "$QUACKMUX_ENGINE_ID")"
	fi
	if [[ -n ${QUACKMUX_SHARE_LEVEL:-} ]]; then
		details+=", share level $(printable "$QUACKMUX_SHARE_LEVEL")"
	fi
	if [[ -n ${QUACKMUX_OWNER:-} ]]; then
		details+=", owner $(printable "$QUACKMUX_OWNER")"
	fi
	if [[ -n ${QUACKMUX_SUBDOMAIN:-} ]]; then
		details+=", subdomain $(printable "$QUACKMUX_SUBDOMAIN")"
	fi
	if [[ -n ${QUACKMUX_PROFILE:-} ]]; then
		details+=", profile $(printable "$QUACKMUX_PROFILE")"
	fi
	printf '%s' "$details"
}

prepare_workdir() {
	local base=${TMPDIR:-/tmp}
	if ! workdir=$(mktemp -d "$base/$prog.XXXXXXXXXX"); then
		workdir=
		config_error "cannot create a private directory in '$(printable "$base")'; the engine needs a writable /tmp (or TMPDIR)"
	fi
	chmod 0700 "$workdir"
	if [[ ! $workdir =~ $safe_path_re ]]; then
		config_error "the temporary directory '$(printable "$workdir")' contains unsupported characters; point TMPDIR to a plain path"
	fi
	mkfifo -m 0600 "$workdir/stdin"
}

# build_bootstrap sets $bootstrap: the commands that prepare DuckDB, run the
# init SQL (from the environment, then from the file) and start the server.
# After quack_serve has succeeded, DuckDB creates the marker file "started";
# with -bail it never gets there if an earlier statement fails.
build_bootstrap() {
	local nl=$'\n'
	bootstrap="SET extension_directory = $(sql_string "$extension_dir");$nl"
	bootstrap+="SET autoinstall_known_extensions = false;$nl"
	bootstrap+="SET autoload_known_extensions = true;$nl"
	bootstrap+="LOAD quack;$nl"
	# Server settings go before the init SQL so that it may override them or
	# lock the configuration (SET lock_configuration = true).
	if [[ -n $max_connections ]]; then
		bootstrap+="SET GLOBAL quack_server_max_connections = $max_connections;$nl"
	fi
	if [[ -n $keep_alive_timeout ]]; then
		bootstrap+="SET GLOBAL quack_server_keep_alive_timeout = $keep_alive_timeout;$nl"
	fi
	if [[ ${QUACKMUX_INIT_SQL:-} =~ [^[:space:]] ]]; then
		(umask 077 && printf '%s\n' "$QUACKMUX_INIT_SQL" >"$workdir/init-env.sql")
		bootstrap+=".read $workdir/init-env.sql$nl"
	fi
	if [[ -n $init_sql_file ]]; then
		if ! (umask 077 && cat -- "$init_sql_file" >"$workdir/init-file.sql"); then
			config_error "cannot read QUACKMUX_INIT_SQL_FILE '$(printable "$init_sql_file")'"
		fi
		bootstrap+=".read $workdir/init-file.sql$nl"
	fi
	# quack_serve returns a row that includes the token: discard it.
	bootstrap+=".output /dev/null$nl"
	bootstrap+="CALL quack_serve($(sql_string "$uri"), token = getenv('QUACK_TOKEN'), allow_other_hostname = true, disable_ssl = true);$nl"
	bootstrap+=".output $workdir/started$nl"
	bootstrap+=".print started$nl"
	bootstrap+=".output stdout$nl"
}

start_duckdb() {
	# Opening the FIFO read-write never blocks, and it keeps a writer around,
	# so DuckDB does not see EOF before this script closes fd 3 (or dies).
	exec 3<>"$workdir/stdin"
	(
		# The init SQL reaches DuckDB as a file; keep it out of DuckDB's
		# environment, where SQL users could read it with getenv().
		unset QUACKMUX_INIT_SQL
		exec "$duckdb_bin" -init /dev/null -bail "$database"
	) <"$workdir/stdin" 3>&- &
	duckdb_pid=$!
	# A few hundred bytes, far below the pipe capacity: the write cannot block
	# even if DuckDB has already exited.
	printf '%s' "$bootstrap" >&3
}

# wait_until_serving returns once the server runs (serving=1), DuckDB has
# exited or a stop was requested.
wait_until_serving() {
	local marker=$workdir/started
	while [[ ! -e $marker ]]; do
		if ((stop_requested)) || ! kill -0 "$duckdb_pid" 2>/dev/null; then
			break
		fi
		sleep 0.1 3>&- || true
	done
	if [[ -e $marker ]]; then
		serving=1
	fi
	return 0
}

# wait_while_serving returns when DuckDB exits by itself (setting
# duckdb_status) or when a stop was requested.
wait_while_serving() {
	local rc seen
	while ((!stop_requested)); do
		seen=$signal_count
		rc=0
		wait "$duckdb_pid" || rc=$?
		if ((signal_count == seen)); then
			# Not interrupted by a signal: DuckDB has exited.
			duckdb_status=$rc
			return 0
		fi
	done
	return 0
}

# collect_duckdb_status waits for DuckDB to exit and sets duckdb_status.
collect_duckdb_status() {
	local rc seen
	if [[ -n $duckdb_status ]]; then
		return 0
	fi
	while :; do
		seen=$signal_count
		rc=0
		wait "$duckdb_pid" || rc=$?
		if ((signal_count == seen)); then
			break
		fi
	done
	duckdb_status=$rc
}

# stop_duckdb stops the server, checkpoints a database file and ends the CLI,
# killing it when it does not exit within the stop timeout.
stop_duckdb() {
	local nl=$'\n' commands deadline reason=
	# Run every step even if one fails; the CLI still reports errors in its
	# exit status.
	commands=".bail off$nl.output /dev/null$nl"
	commands+="CALL quack_stop($(sql_string "$uri"));$nl"
	if is_local_file_database; then
		# FORCE: abort transactions that clients left open instead of failing.
		commands+="FORCE CHECKPOINT;$nl"
	fi
	commands+=".quit$nl"
	if ((!serving)) && [[ -e $workdir/started ]]; then
		serving=1 # the server came up just now
	fi
	if ((!serving)); then
		# Still starting: interrupt the running init statement. The CLI stops
		# reading its input after SIGINT; the commands below are a fallback.
		kill -INT "$duckdb_pid" 2>/dev/null || true
	fi
	printf '%s' "$commands" >&3 || true
	# EOF after the commands ends the CLI even if .quit is never reached.
	exec 3>&-

	deadline=$((SECONDS + stop_timeout))
	while kill -0 "$duckdb_pid" 2>/dev/null; do
		if ((force_kill)); then
			reason="after a second signal"
		elif ((SECONDS >= deadline)); then
			reason="after the stop timeout of ${stop_timeout}s (QUACKMUX_STOP_TIMEOUT)"
		fi
		if [[ -n $reason ]]; then
			log "killing DuckDB $reason"
			kill -KILL "$duckdb_pid" 2>/dev/null || true
			break
		fi
		sleep 0.2 || true
	done
	collect_duckdb_status
}

# failure_status STATUS maps a DuckDB exit status to the exit status of an
# engine that ended without being asked to: never 0.
failure_status() {
	if (($1 == 0)); then
		echo 1
	else
		echo "$1"
	fi
}

main() {
	trap 'on_signal TERM' TERM
	trap 'on_signal INT' INT
	trap 'on_signal HUP' HUP
	trap cleanup EXIT

	read_settings
	prepare_workdir
	build_bootstrap

	if ((stop_requested)); then
		log "stop requested before DuckDB was started"
		exit 0
	fi
	log "starting DuckDB ($(engine_details))"
	start_duckdb
	wait_until_serving

	if ((serving)); then
		log "serving on $uri over plain HTTP ($(engine_details))"
		# DuckDB has read everything up to here: drop the FIFO path, the copies
		# of the init SQL and the marker. fd 3 keeps the FIFO itself open.
		rm -rf -- "$workdir"
		workdir=
		wait_while_serving
	fi

	if ((!stop_requested)); then
		collect_duckdb_status
		if ((serving)); then
			log "DuckDB exited unexpectedly with status $duckdb_status"
		else
			log "DuckDB exited with status $duckdb_status before the Quack server was started; see the DuckDB messages above (database, extensions, init SQL)"
		fi
		exit "$(failure_status "$duckdb_status")"
	fi

	stop_duckdb
	log "DuckDB stopped (exit status $duckdb_status)"
	exit "$duckdb_status"
}

main "$@"
