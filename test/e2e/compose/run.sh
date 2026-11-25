#!/usr/bin/env bash
#
# End-to-end test of quackmux with Docker Compose: real DuckDB 2.0 engines
# behind the gateway, real DuckDB 2.0 clients (see ../README.md).
#
#   bash test/e2e/compose/run.sh
#
# Optional environment:
#   E2E_SKIP_BUILD=1     use existing images instead of building them
#   E2E_KEEP=1           leave the containers running after the test
#   E2E_ENGINE_IMAGE     engine image (default quackmux/duckdb-engine:2.0.0)
#   E2E_GATEWAY_IMAGE    gateway image (default quackmux/quackmux:e2e)
#   E2E_EXTENSION_DIR    DuckDB extension directory in the engine image
#                        (default /opt/duckdb/extensions; empty: DuckDB default)
#   E2E_QUACK_PORT, E2E_ADMIN_PORT, E2E_IDLE_QUACK_PORT, E2E_IDLE_ADMIN_PORT
#                        host ports on 127.0.0.1 (19494, 19495, 19594, 19595)
#   E2E_TLS_QUACK_PORT, E2E_TLS_ADMIN_PORT
#                        host ports of gateway-tls (19794, 19795)
#   E2E_PROJECT          Compose project name (default quackmux-e2e); with its
#                        own name and ports a second run can go on in parallel
set -euo pipefail

COMPOSE_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
E2E_ROOT=$(cd "$COMPOSE_DIR/.." && pwd)
REPO_ROOT=$(cd "$E2E_ROOT/../.." && pwd)
# shellcheck source=../lib/common.sh
. "$E2E_ROOT/lib/common.sh"

ENV_FILE=$COMPOSE_DIR/e2e.env
# Exported, so that docker compose interpolates exactly the values of the env
# file even if the caller's environment holds others.
set -a
# shellcheck source=e2e.env
. "$ENV_FILE"
set +a

export E2E_ENGINE_IMAGE=${E2E_ENGINE_IMAGE:-quackmux/duckdb-engine:2.0.0}
export E2E_GATEWAY_IMAGE=${E2E_GATEWAY_IMAGE:-quackmux/quackmux:e2e}
export E2E_QUACK_PORT=${E2E_QUACK_PORT:-19494} E2E_ADMIN_PORT=${E2E_ADMIN_PORT:-19495}
export E2E_IDLE_QUACK_PORT=${E2E_IDLE_QUACK_PORT:-19594} E2E_IDLE_ADMIN_PORT=${E2E_IDLE_ADMIN_PORT:-19595}
export E2E_TLS_QUACK_PORT=${E2E_TLS_QUACK_PORT:-19794} E2E_TLS_ADMIN_PORT=${E2E_TLS_ADMIN_PORT:-19795}
E2E_PROJECT=${E2E_PROJECT:-quackmux-e2e}

GATEWAY_ADMIN=http://127.0.0.1:$E2E_ADMIN_PORT
IDLE_ADMIN=http://127.0.0.1:$E2E_IDLE_ADMIN_PORT
TLS_ADMIN=http://127.0.0.1:$E2E_TLS_ADMIN_PORT
# The profile "tls" adds gateway-tls (scenario l).
COMPOSE=(docker compose -p "$E2E_PROJECT" --env-file "$ENV_FILE" -f "$COMPOSE_DIR/docker-compose.yaml" --profile tls)
# A fresh DuckDB CLI container for every client. Its stdin is the SQL script.
CLIENT=("${COMPOSE[@]}" run --rm -T --no-deps client)

need docker curl mkfifo awk sed grep openssl
docker compose version >/dev/null 2>&1 || die "docker compose v2 is required"
# Writing to a session whose client died must fail the check, not kill the run.
trap '' PIPE

E2E_WORK=$(make_workdir)
ADMIN_HEADER_FILE=$E2E_WORK/admin.header
(umask 077 && printf 'Authorization: Bearer %s\n' "$E2E_ADMIN_TOKEN" >"$ADMIN_HEADER_FILE")

# Scenario l: a self-signed certificate for gateway-tls, made with the
# command of docs/tls.md (with the compose host name and localhost). The
# gateway runs as another user, so the throwaway key is readable for all.
export E2E_TLS_DIR=$E2E_WORK/tls
mkdir -m 755 "$E2E_TLS_DIR"
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 2 \
	-subj '/CN=gateway-tls' -addext 'subjectAltName=DNS:gateway-tls,DNS:localhost' \
	-keyout "$E2E_TLS_DIR/tls.key" -out "$E2E_TLS_DIR/tls.crt" 2>"$E2E_WORK/openssl.log" ||
	die "openssl could not create the test certificate: $(cat "$E2E_WORK/openssl.log")"
chmod 644 "$E2E_TLS_DIR/tls.key" "$E2E_TLS_DIR/tls.crt"

cleanup() {
	local rc=$? c
	trap - EXIT
	session_kill_all
	# The clients of scenario l in the network namespace of gateway-tls are
	# not part of the Compose project.
	for c in $(docker ps -aq --filter "name=^$E2E_PROJECT-l-" 2>/dev/null); do
		docker rm -f "$c" >/dev/null 2>&1 || true
	done
	if [ "$rc" -ne 0 ]; then
		step "container logs (last 150 lines each)"
		"${COMPOSE[@]}" logs --no-color --tail=150 >&2 || true
	fi
	if [ -n "${E2E_KEEP:-}" ]; then
		say "E2E_KEEP is set: the containers keep running. Remove them with"
		say "  docker compose -p $E2E_PROJECT --env-file $ENV_FILE -f $COMPOSE_DIR/docker-compose.yaml --profile tls down -v"
	else
		"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || warn "docker compose down failed"
	fi
	rm -rf "$E2E_WORK"
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# scenario_failed_hook prints the client output of a failed scenario.
scenario_failed_hook() {
	local f
	for f in "$E2E_WORK/$1".log "$E2E_WORK/$1"_*.log; do
		if [ -f "$f" ]; then
			say "  client output ($(basename "$f")):"
			indent "$f"
		fi
	done
}

# client NAME runs the SQL script on stdin in a new client container with
# -bail. The output goes to $E2E_WORK/NAME.log, the exit status to CLIENT_RC.
client() {
	CLIENT_RC=0
	"${CLIENT[@]}" -bail >"$E2E_WORK/$1.log" 2>&1 || CLIENT_RC=$?
}

# attach_sql TOKEN GATEWAY prints the SQL that attaches GATEWAY (a compose
# service) as "remote", authenticated with TOKEN (credential?options).
attach_sql() {
	printf "CREATE SECRET (TYPE quack, TOKEN '%s', SCOPE 'quack:%s:9494');\n" "$1" "$2"
	printf "ATTACH 'quack:%s:9494' AS remote (DISABLE_SSL true);\n" "$2"
}

# engines_ready BASE_URL N succeeds when the gateway reports N READY engines.
engines_ready() {
	local out=$E2E_WORK/engines.json
	api GET "$1" /api/v1/engines "$out"
	[ "$HTTP_STATUS" = 200 ] && [ "$(json_count "$out" state '"READY"')" -ge "$2" ]
}

# (a) SERVER share level: alice reaches engine-shared. A temporary table lives
# on her engine connection, so it is only visible if every request of the
# session reaches the same backend connection. DETACH ends the session.
scenario_server_share() {
	local log=$E2E_WORK/a.log
	client a <<SQL
$(client_preamble)
$(attach_sql "${E2E_ALICE_TOKEN}?share_level=server" gateway)
SELECT 'whoami=' || engine AS result FROM remote.whoami;
FROM remote.query('CREATE TEMP TABLE e2e_scratch AS SELECT 21 AS half');
SELECT 'temp=' || CAST(half * 2 AS VARCHAR) AS result FROM remote.query('SELECT half FROM e2e_scratch');
DETACH remote;
SELECT 'detach=ok' AS result;
SQL
	expect_status "$CLIENT_RC" 0 "the client ran all statements" || return 1
	expect_output "$log" 'whoami=shared' "share_level=server is routed to engine-shared" || return 1
	expect_output "$log" 'temp=42' "the temporary table is visible to the next query of the session" || return 1
	expect_output "$log" 'detach=ok' "DETACH succeeded" || return 1
	# DETACH sends DISCONNECT, which the gateway answers itself after closing
	# the session.
	wait_for 15 "the gateway to close alice's session after DETACH" session_count_is "$GATEWAY_ADMIN" alice 0 || {
		expect_sessions "$GATEWAY_ADMIN" alice 0 "DETACH closed the session in the gateway"
		return 1
	}
	ok "DETACH closed the session in the gateway"
}

# (b) GROUP share level for a group the user is not a member of.
scenario_group_denied() {
	local log=$E2E_WORK/b.log
	client b <<SQL
$(client_preamble)
$(attach_sql "${E2E_BOB_TOKEN}?share_level=group&group=analytics" gateway)
SELECT 'attached=unexpected' AS result;
SQL
	expect_status "$CLIENT_RC" nonzero "the client failed" || return 1
	expect_output "$log" 'not a member' "ATTACH is denied: bob is not a member of analytics" || return 1
	expect_no_output "$log" 'attached=unexpected' "no statement ran after the denied ATTACH" || return 1
	expect_sessions "$GATEWAY_ADMIN" bob 0 "the gateway opened no session" || return 1
}

# (c) GROUP share level routes a member to the engine of the group.
scenario_group_engine() {
	local log=$E2E_WORK/c.log
	client c <<SQL
$(client_preamble)
$(attach_sql "${E2E_ALICE_TOKEN}?share_level=group&group=analytics" gateway)
SELECT 'whoami=' || engine AS result FROM remote.whoami;
DETACH remote;
SQL
	expect_status "$CLIENT_RC" 0 "the client ran all statements" || return 1
	expect_output "$log" 'whoami=analytics' "share_level=group&group=analytics is routed to engine-analytics" || return 1
	expect_no_output "$log" 'whoami=shared' "the session did not reach engine-shared" || return 1
}

# (d) An unknown token is rejected with the stock server's error text.
scenario_wrong_token() {
	local log=$E2E_WORK/d.log before after
	before=$(metric_sum "$GATEWAY_ADMIN" quackmux_auth_failures_total)
	client d <<SQL
$(client_preamble)
$(attach_sql "${E2E_WRONG_TOKEN}?share_level=server" gateway)
SELECT 'attached=unexpected' AS result;
SQL
	expect_status "$CLIENT_RC" nonzero "the client failed" || return 1
	expect_output "$log" 'Authentication failed' "ATTACH fails with 'Authentication failed'" || return 1
	expect_no_output "$log" 'attached=unexpected' "no statement ran after the failed ATTACH" || return 1
	after=$(metric_sum "$GATEWAY_ADMIN" quackmux_auth_failures_total)
	expect_ge "$after" $((before + 1)) "quackmux_auth_failures_total counted the failure ($before -> $after)" || return 1
}

# (e) While a client keeps its attachment open, the admin API lists the
# session; without the bearer token the API refuses to answer.
scenario_admin_sessions() {
	local list=$E2E_WORK/e_list.json detail=$E2E_WORK/e_detail.json id status rc=0
	session_start e_hold "${CLIENT[@]}" || return 1
	session_send e_hold "$(client_preamble)
$(attach_sql "${E2E_BOB_TOKEN}?share_level=server" gateway)
SELECT 'hold=' || engine AS result FROM remote.whoami;" || return 1
	session_wait e_hold 'hold=shared' 120 || return 1
	ok "bob attached and keeps the attachment open"

	api GET "$GATEWAY_ADMIN" "/api/v1/sessions?user=bob" "$list"
	expect_eq "$HTTP_STATUS" 200 "GET /api/v1/sessions?user=bob answers 200" || return 1
	if ! expect_eq "$(json_value "$list" count)" 1 "the open attachment is listed as one session"; then
		indent "$list"
		return 1
	fi
	expect_eq "$(json_value "$list" state)" OPEN "the session is OPEN" || return 1
	expect_eq "$(json_value "$list" shareLevel)" SERVER "the session has share level SERVER" || return 1
	id=$(json_value "$list" id)
	api GET "$GATEWAY_ADMIN" "/api/v1/sessions/$id" "$detail"
	expect_eq "$HTTP_STATUS" 200 "GET /api/v1/sessions/$id answers 200" || return 1
	expect_ge "$(json_count "$detail" state '"READY"')" 1 "the session's engine is READY" || return 1
	status=$(curl -s -o /dev/null -w '%{http_code}' "$GATEWAY_ADMIN/api/v1/sessions") || status=000
	expect_eq "$status" 401 "the admin API rejects requests without the bearer token" || return 1

	session_send e_hold "DETACH remote;
SELECT 'released=ok' AS result;" || return 1
	session_wait e_hold 'released=ok' 60 || return 1
	session_end e_hold || rc=$?
	expect_status "$rc" 0 "the client exited cleanly" || return 1
	wait_for 15 "the session of bob to disappear after DETACH" session_count_is "$GATEWAY_ADMIN" bob 0 || {
		expect_sessions "$GATEWAY_ADMIN" bob 0 "the session is gone after DETACH"
		return 1
	}
	ok "the session is gone after DETACH"
}

# (f) gateway-idle closes sessions that ran no query for 15s (heartbeats do
# not count); the next query gets the reason.
scenario_idle_timeout() {
	local log=$E2E_WORK/f_idle.log t0 before after
	before=$(metric_sum "$IDLE_ADMIN" quackmux_sessions_closed_total 'reason="idle timeout"')
	session_start f_idle "${CLIENT[@]}" || return 1
	session_send f_idle "$(client_preamble)
$(attach_sql "${E2E_ALICE_TOKEN}?share_level=server" gateway-idle)
SELECT 'before=' || engine AS result FROM remote.whoami;" || return 1
	session_wait f_idle 'before=shared' 120 || return 1
	t0=$(now)
	expect_sessions "$IDLE_ADMIN" alice 1 "gateway-idle lists the session" || return 1
	say "  waiting 20s without a query (session.idleTimeout is 15s)"
	sleep_until $((t0 + 20))
	expect_sessions "$IDLE_ADMIN" alice 0 "the reaper closed the quiet session" || return 1
	session_send f_idle "SELECT 'after=' || engine AS result FROM remote.whoami;
SELECT 'f-done' AS result;" || return 1
	session_wait f_idle 'f-done' 60 '' || return 1
	expect_output "$log" 'idle timeout' "the next query fails with the reason 'idle timeout'" || return 1
	expect_no_output "$log" 'after=shared' "the query did not run on the engine" || return 1
	after=$(metric_sum "$IDLE_ADMIN" quackmux_sessions_closed_total 'reason="idle timeout"')
	expect_ge "$after" $((before + 1)) "quackmux_sessions_closed_total{reason=\"idle timeout\"} counted it ($before -> $after)" || return 1
	session_send f_idle "DETACH remote;" || true
	# The exit status is 1: a statement failed (as expected).
	session_end f_idle || true
}

# (g) CREATE TABLE AS on the attached database streams the rows from the
# client to the engine with SEND_DATA requests through the gateway.
scenario_send_data() {
	local log=$E2E_WORK/g.log before after
	before=$(metric_sum "$GATEWAY_ADMIN" quackmux_requests_total 'type="SEND_DATA_REQUEST"' 'outcome="ok"')
	client g <<SQL
$(client_preamble)
$(attach_sql "${E2E_ALICE_TOKEN}?share_level=server" gateway)
CREATE TABLE remote.e2e_range AS SELECT range FROM range(100000);
SELECT 'range_count=' || CAST(count(*) AS VARCHAR) AS result FROM remote.e2e_range;
CREATE TABLE e2e_local AS SELECT range AS id FROM range(100000);
CREATE TABLE remote.e2e_upload AS SELECT id FROM e2e_local;
SELECT 'upload=' || CAST(count(*) AS VARCHAR) || '/' || CAST(sum(id) AS VARCHAR) AS result FROM remote.e2e_upload;
DETACH remote;
SQL
	expect_status "$CLIENT_RC" 0 "the client ran all statements" || return 1
	expect_output "$log" 'range_count=100000' "CREATE TABLE remote.t AS SELECT range FROM range(100000) stored 100000 rows" || return 1
	expect_output "$log" 'upload=100000/4999950000' "the rows of a local table arrived complete (count and sum)" || return 1
	after=$(metric_sum "$GATEWAY_ADMIN" quackmux_requests_total 'type="SEND_DATA_REQUEST"' 'outcome="ok"')
	expect_ge "$after" $((before + 1)) "SEND_DATA_REQUEST messages went through the gateway ($before -> $after)" || return 1
}

# (h) bob's queries have a time limit of 5s (overrides in gateway.yaml). A
# query below it is not touched. A long query is cancelled on the engine in
# each phase that the limit can hit: while the engine computes (its PREPARE
# waits), while the client fetches the rows (its read-ahead FETCHes wait) and
# while the client uploads rows (SEND_DATA). Each time bob gets the gateway's
# error at the limit, and his session runs the next query.
scenario_query_timeout() {
	local phase=$E2E_WORK/h_phase.log msg before after fetches uploads t0 elapsed
	msg='ran longer than its time limit of 5s (overrides[0].queryTimeout) and was cancelled'
	before=$(metric_sum "$GATEWAY_ADMIN" quackmux_query_timeouts_total 'result="cancelled"')
	fetches=$(metric_sum "$GATEWAY_ADMIN" quackmux_requests_total 'type="FETCH_REQUEST"' 'outcome="query_timeout"')
	uploads=$(metric_sum "$GATEWAY_ADMIN" quackmux_requests_total 'type="SEND_DATA_REQUEST"' 'outcome="query_timeout"')
	session_start h_limit "${CLIENT[@]}" || return 1
	session_send h_limit "$(client_preamble)
$(attach_sql "${E2E_BOB_TOKEN}?share_level=server" gateway)
CREATE TABLE h_one AS SELECT 0::BIGINT AS k;
SELECT 'short=' || CAST(count(*) AS VARCHAR) AS result FROM remote.query('SELECT range FROM range(100000)');" || return 1
	session_wait h_limit 'short=100000' 120 || return 1
	ok "a query below the limit returns its result"

	# The engine counts 10^12 rows, far longer than the limit. (A count
	# outside remote.query would make the client fetch the rows.)
	t0=$(now)
	session_send h_limit "SELECT 'long=' || CAST(n AS VARCHAR) AS result FROM remote.query('SELECT count(*) AS n FROM range(1000000000000)');
SELECT 'after_engine=' || engine AS result FROM remote.whoami;" || return 1
	session_wait h_limit 'after_engine=shared' 120 '' || return 1
	elapsed=$(($(now) - t0))
	session_output h_limit >"$phase"
	expect_output "$phase" "$msg" "while the engine computes: the query fails with the gateway's error" || return 1
	expect_no_output "$phase" 'long=1000000000000' "the query returned no result" || return 1
	expect_ge 30 "$elapsed" "the error came at the limit, not at the end of the query, and the session runs the next query (after ${elapsed}s)" || return 1

	# The join with a local table makes the client fetch the 5 * 10^9 rows.
	t0=$(now)
	session_send h_limit "SELECT 'joined=' || CAST(count(*) AS VARCHAR) AS result FROM remote.query('SELECT range AS i FROM range(5000000000)') r JOIN h_one ON r.i % 2 = h_one.k;
SELECT 'after_fetch=' || engine AS result FROM remote.whoami;" || return 1
	session_wait h_limit 'after_fetch=shared' 120 '' || return 1
	elapsed=$(($(now) - t0))
	session_output h_limit >"$phase"
	expect_output "$phase" "$msg" "while the client fetches: the query fails with the gateway's error" || return 1
	expect_no_output "$phase" 'joined=2500000000' "the query returned no result" || return 1
	expect_ge 30 "$elapsed" "the session runs the next query (after ${elapsed}s)" || return 1
	after=$(metric_sum "$GATEWAY_ADMIN" quackmux_requests_total 'type="FETCH_REQUEST"' 'outcome="query_timeout"')
	expect_ge "$after" $((fetches + 1)) "the gateway answered the waiting FETCHes ($fetches -> $after)" || return 1

	# The client uploads the rows of a local query with SEND_DATA.
	t0=$(now)
	session_send h_limit "CREATE TABLE remote.h_upload AS SELECT r.range AS i FROM range(10000000000) r, h_one;
SELECT 'upload_tables=' || CAST(count(*) AS VARCHAR) AS result FROM remote.query('SELECT * FROM duckdb_tables() WHERE table_name = ''h_upload''');" || return 1
	session_wait h_limit 'upload_tables=' 120 '' || return 1
	elapsed=$(($(now) - t0))
	session_output h_limit >"$phase"
	expect_output "$phase" "$msg" "while the client uploads: CREATE TABLE AS fails with the gateway's error" || return 1
	expect_output "$phase" 'upload_tables=0' "it left no table (the client rolled the transaction back)" || return 1
	expect_ge 30 "$elapsed" "the session runs the next query (after ${elapsed}s)" || return 1
	after=$(metric_sum "$GATEWAY_ADMIN" quackmux_requests_total 'type="SEND_DATA_REQUEST"' 'outcome="query_timeout"')
	expect_ge "$after" $((uploads + 1)) "the gateway answered the upload ($uploads -> $after)" || return 1

	after=$(metric_sum "$GATEWAY_ADMIN" quackmux_query_timeouts_total 'result="cancelled"')
	expect_ge "$after" $((before + 3)) "quackmux_query_timeouts_total{result=\"cancelled\"} counted all three ($before -> $after)" || return 1
	session_send h_limit "DETACH remote;" || true
	# The exit status is 1: statements failed (as expected).
	session_end h_limit || true
}

# (j) engine-analytics is hardened like a shared engine (the authorization
# macro of the image, as quackmux_authorize_listing, and the post-start
# settings). Through the gateway a session attaches (catalog queries),
# queries, uploads and keeps a temporary table as usual, and FROM _ only
# shows the harmless last result of the engine's CLI. Then the cases of the
# macro run through the gateway (quackmux-authorize-test of the client
# image, images/engine/authorize-cases.tsv): what the macro and DuckDB must
# refuse, also in other spellings, the client's ordinary traffic and the
# listing of the gateway. The engine token appears nowhere, and the engine
# keeps serving.
scenario_hardened_engine() {
	local log=$E2E_WORK/j.log cases=$E2E_WORK/j_cases.log
	client j <<SQL
$(client_preamble)
$(attach_sql "${E2E_ALICE_TOKEN}?share_level=group&group=analytics" gateway)
SELECT 'whoami=' || engine AS result FROM remote.whoami;
SHOW ALL TABLES;
CREATE TABLE e2e_local AS SELECT range AS id FROM range(1000);
CREATE TABLE remote.e2e_hardened AS SELECT id FROM e2e_local;
SELECT 'upload=' || CAST(count(*) AS VARCHAR) AS result FROM remote.e2e_hardened;
DROP TABLE remote.e2e_hardened;
FROM remote.query('CREATE TEMP TABLE e2e_scratch AS SELECT 21 AS half');
SELECT 'temp=' || CAST(half * 2 AS VARCHAR) AS result FROM remote.query('SELECT half FROM e2e_scratch');
SELECT 'last=' || quackmux_engine AS result FROM remote.query('FROM _');
DETACH remote;
SQL
	expect_status "$CLIENT_RC" 0 "the client ran all statements" || return 1
	expect_output "$log" 'whoami=analytics' "ATTACH (catalog queries) and a query pass the authorization macro" || return 1
	expect_output "$log" 'upload=1000' "an upload with SEND_DATA passes the macro" || return 1
	expect_output "$log" 'temp=42' "a temporary table works on the hardened engine" || return 1
	expect_output "$log" 'last=started' "FROM _ shows the harmless last result of the engine's CLI" || return 1

	# The tokens reach the container through its environment, not through
	# a command line.
	CLIENT_RC=0
	QUACK_TOKEN="${E2E_ALICE_TOKEN}?share_level=group&group=analytics" QUACKMUX_TEST_SECRET=$E2E_ENGINE_ANALYTICS_TOKEN \
		QUACKMUX_EXTENSION_DIR=$E2E_EXTENSION_DIR QUACKMUX_TEST_LISTING=1 "${COMPOSE[@]}" run --rm -T --no-deps \
		-e QUACK_TOKEN -e QUACKMUX_TEST_SECRET -e QUACKMUX_EXTENSION_DIR -e QUACKMUX_TEST_LISTING \
		--entrypoint /usr/local/bin/quackmux-authorize-test client gateway:9494 >"$cases" 2>&1 || CLIENT_RC=$?
	if ! expect_status "$CLIENT_RC" 0 "the cases of the authorization macro pass through the gateway: $(tail -n 1 "$cases")"; then
		indent "$cases"
		return 1
	fi
	expect_no_output "$cases" "$E2E_ENGINE_ANALYTICS_TOKEN" "the engine token appears nowhere" || return 1

	client j_after <<SQL
$(client_preamble)
$(attach_sql "${E2E_ALICE_TOKEN}?share_level=group&group=analytics" gateway)
SELECT 'still=' || engine AS result FROM remote.whoami;
DETACH remote;
SQL
	expect_output "$E2E_WORK/j_after.log" 'still=analytics' "the engine keeps serving" || return 1
}

# list_connections SESSION TAG FILE lists the connections of the engine of
# SESSION, an attached client, into FILE (sorted): the query the gateway uses
# after a restart, which the macro of a hardened engine lets through as
# quackmux_authorize_listing.
list_connections() {
	session_send "$1" "SELECT 'conn=' || connection_id AS result FROM remote.query('SELECT connection_id FROM quack_active_connections()');
SELECT 'listed=$2' AS result;" || return 1
	session_wait "$1" "listed=$2" 60 || return 1
	session_output "$1" | sed -n 's/^conn=//p' | sort >"$3"
}

# new_connection BEFORE AFTER prints the one connection in AFTER that is not
# in BEFORE (listings of list_connections).
new_connection() {
	local new
	new=$(comm -13 "$1" "$2")
	if [ "$(nlines "$new")" != 1 ]; then
		bad "expected exactly one new connection, got: ${new:-none}"
		return 1
	fi
	printf '%s\n' "$new"
}

# orphan_client NAME ROLE LIST BEFORE AFTER ATTACH_SQL [RUN...] starts the
# client k_NAME_ROLE (with RUN instead of a fresh client container), attaches
# it with ATTACH_SQL, creates a temporary table on its engine connection and
# sets ROLE_NAME (upper case) to that connection: the one that the listing of
# session LIST shows in AFTER but not in BEFORE.
orphan_client() {
	local name=$1 role=$2 list=$3 before=$4 after=$5 attach=$6 id session=k_$1_$2
	shift 6
	if [ $# -eq 0 ]; then
		set -- "${CLIENT[@]}"
	fi
	session_start "$session" "$@" || return 1
	session_send "$session" "$(client_preamble)
$attach
FROM remote.query('CREATE TEMP TABLE e2e_orphan_scratch AS SELECT 21 AS half');
SELECT '$role=' || engine AS result FROM remote.whoami;" || return 1
	session_wait "$session" "$role=$name" 120 || return 1
	list_connections "$list" "$name-$role" "$after" || return 1
	id=$(new_connection "$before" "$after") || return 1
	eval "$(printf '%s' "$role" | tr '[:lower:]' '[:upper:]')_$name=\$id"
}

# orphan_setup NAME OPTIONS ENGINE TOKEN attaches three clients to the
# compose service ENGINE, each with a temporary table on its engine
# connection: one of alice with OPTIONS through the gateway that lives
# (k_NAME_live), one that attaches to the engine directly with its TOKEN, a
# connection the gateway does not know (k_NAME_foreign), and one of alice
# through the gateway that will die (k_NAME_dead, in the container
# $E2E_PROJECT-k-NAME).
orphan_setup() {
	local name=$1 opts=$2 engine=$3 token=$4 list=k_${1}_list w=$E2E_WORK/k_$1
	session_start "$list" "${CLIENT[@]}" || return 1
	session_send "$list" "$(client_preamble)
$(attach_sql "${E2E_ALICE_TOKEN}?$opts" gateway)" || return 1
	list_connections "$list" "${name}0" "${w}_0.ids" || return 1
	orphan_client "$name" live "$list" "${w}_0.ids" "${w}_1.ids" "$(attach_sql "${E2E_ALICE_TOKEN}?$opts" gateway)" || return 1
	orphan_client "$name" foreign "$list" "${w}_1.ids" "${w}_2.ids" "$(attach_sql "$token" "$engine")" || return 1
	orphan_client "$name" dead "$list" "${w}_2.ids" "${w}_3.ids" "$(attach_sql "${E2E_ALICE_TOKEN}?$opts" gateway)" \
		"${COMPOSE[@]}" run --rm -T --no-deps --name "$E2E_PROJECT-k-$name" client || return 1
	session_send "$list" "DETACH remote;" || return 1
	session_end "$list" || return 1
	ok "$name: a living and a doomed client of alice and a client of the engine itself are attached ($opts)"
}

# orphan_id LIVE|FOREIGN|DEAD NAME prints a connection that orphan_setup
# NAME found.
orphan_id() { eval "printf '%s' \"\${${1}_$2:-}\""; }

# orphans_closed N succeeds once the gateway has closed N orphaned
# connections since it started.
orphans_closed() { [ "$(metric_sum "$GATEWAY_ADMIN" quackmux_orphaned_connections_closed_total)" -ge "$1" ]; }

# temp_table_alive NAME ROLE TAG checks that the client k_NAME_ROLE of
# orphan_setup still reaches its temporary table, that is its engine
# connection.
temp_table_alive() {
	local session=k_$1_$2
	session_send "$session" "SELECT 'temp=' || CAST(half * 2 AS VARCHAR) AS result FROM remote.query('SELECT half FROM e2e_orphan_scratch');
SELECT 'checked=$3' AS result;" || return 1
	session_wait "$session" "checked=$3" 60 || return 1
	case $(session_output "$session") in
	*temp=42*) ok "$1: the $2 client still has its connection and its temporary table ($3)" ;;
	*)
		bad "$1: the $2 client lost its temporary table ($3):"
		session_output "$session" | indent
		return 1
		;;
	esac
}

# (k) Orphaned engine connections: on engine-shared and on the hardened
# engine-analytics, a client dies while the gateway is stopped. After the
# restart, once the engine lease of its connection has run out (the gateway
# waits 75s with gateway.yaml: backendHeartbeatTimeout 60s plus
# heartbeatGrace), the gateway's heartbeat makes the engine remove the
# connection. A client that lived through the restart keeps its connection,
# its temporary table and its queries, and so does a client attached to the
# engine directly, whose connection the gateway does not know.
scenario_orphans() {
	local name list=$E2E_WORK/k_final.ids role id
	orphan_setup shared "share_level=server" engine-shared "$E2E_ENGINE_SHARED_TOKEN" || return 1
	orphan_setup analytics "share_level=group&group=analytics" engine-analytics "$E2E_ENGINE_ANALYTICS_TOKEN" || return 1

	"${COMPOSE[@]}" stop gateway >/dev/null 2>&1 || {
		bad "docker compose stop gateway failed"
		return 1
	}
	for name in shared analytics; do
		docker kill "$E2E_PROJECT-k-$name" >/dev/null 2>&1 || {
			bad "cannot kill the client container $E2E_PROJECT-k-$name"
			return 1
		}
		session_end "k_${name}_dead" 30 || true # killed
	done
	ok "the gateway is stopped, and the doomed clients were killed without DETACH"
	"${COMPOSE[@]}" start gateway >/dev/null 2>&1 || {
		bad "docker compose start gateway failed"
		return 1
	}
	wait_for 90 "gateway /readyz after the restart" curl -fsS "$GATEWAY_ADMIN/readyz" || return 1
	wait_for 60 "the living clients to be adopted" session_count_is "$GATEWAY_ADMIN" alice 2 || {
		expect_sessions "$GATEWAY_ADMIN" alice 2 "the living clients are adopted"
		return 1
	}
	ok "the gateway is back; the living clients are adopted"
	for name in shared analytics; do
		temp_table_alive "$name" live "after the restart" || return 1
	done

	wait_for 180 "the gateway to close both orphaned connections" orphans_closed 2 || return 1
	expect_ge "$(metric_sum "$GATEWAY_ADMIN" quackmux_orphan_sweeps_total 'result="ok"')" 2 \
		"quackmux_orphan_sweeps_total{result=\"ok\"} counted both engines" || return 1
	for name in shared analytics; do
		session_start "k_${name}_check" "${CLIENT[@]}" || return 1
		session_send "k_${name}_check" "$(client_preamble)
$(attach_sql "${E2E_ALICE_TOKEN}?$(orphan_opts "$name")" gateway)" || return 1
		list_connections "k_${name}_check" "${name}4" "$list" || return 1
		session_send "k_${name}_check" "DETACH remote;" || return 1
		session_end "k_${name}_check" || return 1
		if grep -qx "$(orphan_id DEAD "$name")" "$list"; then
			bad "$name: the engine still lists the connection of the dead client"
			indent "$list"
			return 1
		fi
		ok "$name: the engine no longer lists the connection of the dead client"
		for role in live foreign; do
			id=$(orphan_id "$(printf '%s' "$role" | tr '[:lower:]' '[:upper:]')" "$name")
			if ! grep -qx "$id" "$list"; then
				bad "$name: the engine no longer lists the connection of the $role client"
				indent "$list"
				return 1
			fi
			temp_table_alive "$name" "$role" "after the probes" || return 1
			session_send "k_${name}_$role" "DETACH remote;" || return 1
			session_end "k_${name}_$role" || return 1
		done
	done
	say "  what the gateway logged about the sweep:"
	"${COMPOSE[@]}" logs --no-color gateway 2>/dev/null |
		grep -E 'closing the connections on recovered engines|closed orphaned connections|no orphaned connections' | indent 2>&1 || true
}

# orphan_opts NAME prints the token options of the clients of alice in
# scenario k (shared: engine-shared, analytics: the hardened
# engine-analytics).
orphan_opts() {
	case $1 in
	shared) echo "share_level=server" ;;
	analytics) echo "share_level=group&group=analytics" ;;
	esac
}

# tls_client NAME runs the SQL script on stdin like client, with the
# certificate of gateway-tls at /certs/tls.crt.
tls_client() {
	CLIENT_RC=0
	"${COMPOSE[@]}" run --rm -T --no-deps -v "$E2E_TLS_DIR:/certs:ro" client -bail >"$E2E_WORK/$1.log" 2>&1 ||
		CLIENT_RC=$?
}

# local_client NAME runs the SQL script on stdin like tls_client, but in the
# network namespace of gateway-tls: there localhost is the gateway, as for a
# client behind "kubectl port-forward".
local_client() {
	local cid
	CLIENT_RC=0
	cid=$("${COMPOSE[@]}" ps -q gateway-tls 2>/dev/null) || cid=''
	if [ -z "$cid" ]; then
		echo "gateway-tls is not running" >"$E2E_WORK/$1.log"
		CLIENT_RC=1
		return
	fi
	docker run --rm -i --name "$E2E_PROJECT-l-$1" --network "container:$cid" --read-only --tmpfs /tmp \
		--cap-drop ALL --security-opt no-new-privileges=true -e HOME=/tmp -v "$E2E_TLS_DIR:/certs:ro" \
		--entrypoint /usr/local/bin/duckdb "$E2E_ENGINE_IMAGE" -bail >"$E2E_WORK/$1.log" 2>&1 || CLIENT_RC=$?
}

# tls_secret TOKEN HOST [OPTIONS] prints a quack secret for HOST:9494.
tls_secret() {
	printf "CREATE SECRET (TYPE quack, TOKEN '%s', SCOPE 'quack:%s:9494'%s);\n" "$1" "$2" "${3:+, $3}"
}

# expect_refused NAME TEXT DESCRIPTION: the client NAME failed with TEXT and
# never got to its last statement.
expect_refused() {
	local log=$E2E_WORK/$1.log
	expect_status "$CLIENT_RC" nonzero "$1: the client failed" || return 1
	expect_output "$log" "$2" "$3" || return 1
	expect_no_output "$log" 'attached=unexpected' "$1: no statement ran after the failed ATTACH" || return 1
}

# (l) TLS (docs/tls.md): gateway-tls serves the Quack port with server.tls
# and a self-signed certificate. A client that pins it (ssl_fingerprint)
# gets in but sends no EXTRA_HTTP_HEADERS, so its session lands on the
# defaults; a client that trusts it as its CA (SET GLOBAL ca_cert_file)
# sends them. Both fetch a result of many batches, which the client fetches
# ahead over several HTTP connections. A wrong pin, the whole openssl line as
# the pin, a host that is not in the certificate and clients that speak HTTP
# (DISABLE_SSL true, localhost without a pin) are refused with the messages
# of docs/tls.md. A pinned client gives up after the global http_timeout.
scenario_tls() {
	local fp line headers many before after cid host
	fp=$(openssl x509 -in "$E2E_TLS_DIR/tls.crt" -noout -fingerprint -sha256 | cut -d= -f2)
	line=$(openssl x509 -in "$E2E_TLS_DIR/tls.crt" -noout -fingerprint -sha256)
	headers="EXTRA_HTTP_HEADERS MAP {'X-Quackmux-Share-Level': 'group', 'X-Quackmux-Group': 'analytics'}"
	many="SELECT 'rows=' || CAST(count(*) AS VARCHAR) || '/' || CAST(count(DISTINCT h) AS VARCHAR) AS result
FROM remote.query('SELECT i, md5(CAST(i AS VARCHAR)) AS h FROM range(1000000) t(i)');"

	before=$(metric_sum "$TLS_ADMIN" quackmux_requests_total 'type="FETCH_REQUEST"')
	tls_client l_pinned <<SQL
$(client_preamble)
$(tls_secret "$E2E_ALICE_TOKEN" gateway-tls "SSL_FINGERPRINT '$fp', $headers")
ATTACH 'quack:gateway-tls:9494' AS remote;
SELECT 'whoami=' || engine AS result FROM remote.whoami;
$many
DETACH remote;
SQL
	expect_status "$CLIENT_RC" 0 "the client with the pin in its secret ran all statements" || return 1
	expect_output "$E2E_WORK/l_pinned.log" 'whoami=shared' \
		"the pinned client sent no EXTRA_HTTP_HEADERS: its session has the defaults (engine-shared)" || return 1
	expect_output "$E2E_WORK/l_pinned.log" 'rows=1000000/1000000' "it fetched a result of many batches" || return 1
	after=$(metric_sum "$TLS_ADMIN" quackmux_requests_total 'type="FETCH_REQUEST"')
	expect_ge "$after" $((before + 2)) "the result needed several FETCH requests ($before -> $after)" || return 1

	before=$after
	tls_client l_ca <<SQL
$(client_preamble)
SET GLOBAL ca_cert_file = '/certs/tls.crt';
$(tls_secret "$E2E_ALICE_TOKEN" gateway-tls "$headers")
ATTACH 'quack:gateway-tls:9494' AS remote;
SELECT 'whoami=' || engine AS result FROM remote.whoami;
$many
DETACH remote;
SQL
	expect_status "$CLIENT_RC" 0 "the client with SET GLOBAL ca_cert_file ran all statements" || return 1
	expect_output "$E2E_WORK/l_ca.log" 'whoami=analytics' \
		"it verified the certificate and sent EXTRA_HTTP_HEADERS (engine-analytics)" || return 1
	expect_output "$E2E_WORK/l_ca.log" 'rows=1000000/1000000' "it fetched a result of many batches" || return 1
	after=$(metric_sum "$TLS_ADMIN" quackmux_requests_total 'type="FETCH_REQUEST"')
	expect_ge "$after" $((before + 2)) "the result needed several FETCH requests ($before -> $after)" || return 1

	tls_client l_wrong_pin <<SQL
$(client_preamble)
$(tls_secret "$E2E_ALICE_TOKEN" gateway-tls "SSL_FINGERPRINT '$(printf '%064d' 0)'")
ATTACH 'quack:gateway-tls:9494' AS remote;
SELECT 'attached=unexpected' AS result;
SQL
	expect_refused l_wrong_pin 'does not match the pinned ssl_fingerprint' "a wrong pin is refused" || return 1

	tls_client l_line <<SQL
$(client_preamble)
$(tls_secret "$E2E_ALICE_TOKEN" gateway-tls "SSL_FINGERPRINT '$line'")
ATTACH 'quack:gateway-tls:9494' AS remote;
SELECT 'attached=unexpected' AS result;
SQL
	expect_refused l_line 'Invalid SSL fingerprint' "the whole openssl output line is no valid pin" || return 1

	# The container name reaches the same gateway, but is not in the
	# certificate.
	cid=$("${COMPOSE[@]}" ps -q gateway-tls 2>/dev/null) || cid=''
	host=$(docker inspect --format '{{.Name}}' "$cid" 2>/dev/null) || host=''
	host=${host#/}
	if [ -z "$host" ]; then
		bad "cannot find the container of gateway-tls"
		return 1
	fi
	tls_client l_ca_host <<SQL
$(client_preamble)
SET GLOBAL ca_cert_file = '/certs/tls.crt';
$(tls_secret "$E2E_ALICE_TOKEN" "$host")
ATTACH 'quack:$host:9494' AS remote;
SELECT 'attached=unexpected' AS result;
SQL
	expect_refused l_ca_host 'SSL peer certificate or SSH remote key was not OK' \
		"the CA check refuses a host name that is not in the certificate ($host)" || return 1

	tls_client l_http <<SQL
$(client_preamble)
$(tls_secret "$E2E_ALICE_TOKEN" gateway-tls)
ATTACH 'quack:gateway-tls:9494' AS remote (DISABLE_SSL true);
SELECT 'attached=unexpected' AS result;
SQL
	expect_refused l_http 'Bad Request' "a client with DISABLE_SSL true gets 'Bad Request'" || return 1

	local_client l_localhost <<SQL
$(client_preamble)
$(tls_secret "$E2E_ALICE_TOKEN" localhost)
ATTACH 'quack:localhost:9494' AS remote;
SELECT 'attached=unexpected' AS result;
SQL
	expect_refused l_localhost 'Bad Request' "localhost without a pin speaks HTTP and gets 'Bad Request' (port-forward)" || return 1

	local_client l_localhost_pinned <<SQL
$(client_preamble)
$(tls_secret "$E2E_ALICE_TOKEN" localhost "SSL_FINGERPRINT '$fp'")
ATTACH 'quack:localhost:9494' AS remote;
SELECT 'whoami=' || engine AS result FROM remote.whoami;
DETACH remote;
SQL
	expect_status "$CLIENT_RC" 0 "localhost with a pin: the client ran all statements" || return 1
	expect_output "$E2E_WORK/l_localhost_pinned.log" 'whoami=shared' "a pin makes the client use HTTPS for localhost" || return 1

	local_client l_localhost_ca <<SQL
$(client_preamble)
SET GLOBAL ca_cert_file = '/certs/tls.crt';
$(tls_secret "$E2E_ALICE_TOKEN" localhost)
ATTACH 'quack:localhost:9494' AS remote (DISABLE_SSL false);
SELECT 'whoami=' || engine AS result FROM remote.whoami;
DETACH remote;
SQL
	expect_status "$CLIENT_RC" 0 "localhost with DISABLE_SSL false: the client ran all statements" || return 1
	expect_output "$E2E_WORK/l_localhost_ca.log" 'whoami=shared' \
		"DISABLE_SSL false and DNS:localhost in the certificate let the CA check pass for localhost" || return 1

	# bob's queries have a time limit of 5s; the engine would count for much
	# longer. With a global http_timeout of 1s the pinned client gives up
	# first; the session's value does not reach its HTTP client.
	tls_client l_timeout <<SQL
$(client_preamble)
SET GLOBAL http_timeout = 1;
$(tls_secret "$E2E_BOB_TOKEN" gateway-tls "SSL_FINGERPRINT '$fp'")
ATTACH 'quack:gateway-tls:9494' AS remote;
SELECT 'long=' || CAST(n AS VARCHAR) AS result FROM remote.query('SELECT count(*) AS n FROM range(1000000000000)');
SQL
	expect_status "$CLIENT_RC" nonzero "with SET GLOBAL http_timeout = 1 the long query failed" || return 1
	expect_output "$E2E_WORK/l_timeout.log" 'Failed to read connection' \
		"the pinned client gave up after the global http_timeout" || return 1
	expect_no_output "$E2E_WORK/l_timeout.log" 'time limit of 5s' "it did not wait for the gateway's time limit" || return 1

	tls_client l_timeout_session <<SQL
$(client_preamble)
SET http_timeout = 1;
$(tls_secret "$E2E_BOB_TOKEN" gateway-tls "SSL_FINGERPRINT '$fp'")
ATTACH 'quack:gateway-tls:9494' AS remote;
SELECT 'long=' || CAST(n AS VARCHAR) AS result FROM remote.query('SELECT count(*) AS n FROM range(1000000000000)');
SQL
	expect_output "$E2E_WORK/l_timeout_session.log" 'time limit of 5s' \
		"with SET http_timeout = 1 (session) it waited until the gateway's time limit" || return 1
}

# (i) Nothing above may have crashed a request handler.
scenario_no_panics() {
	local log=$E2E_WORK/i_gateways.log
	"${COMPOSE[@]}" logs --no-color gateway gateway-idle gateway-tls >"$log" 2>&1 || {
		bad "docker compose logs failed"
		return 1
	}
	expect_no_output "$log" 'panic' "the gateway logs contain no panic" || return 1
}

step "images"
if [ -n "${E2E_SKIP_BUILD:-}" ]; then
	say "E2E_SKIP_BUILD is set: using $E2E_ENGINE_IMAGE and $E2E_GATEWAY_IMAGE"
else
	build_images "$E2E_ENGINE_IMAGE" "$E2E_GATEWAY_IMAGE"
fi

step "start engines and gateways"
"${COMPOSE[@]}" down -v --remove-orphans >/dev/null 2>&1 || true
"${COMPOSE[@]}" up -d
wait_for 90 "gateway /readyz ($GATEWAY_ADMIN)" curl -fsS "$GATEWAY_ADMIN/readyz" || exit 1
wait_for 90 "gateway-idle /readyz ($IDLE_ADMIN)" curl -fsS "$IDLE_ADMIN/readyz" || exit 1
wait_for 90 "gateway-tls /readyz ($TLS_ADMIN)" curl -fsS "$TLS_ADMIN/readyz" || exit 1
wait_for 120 "both engines READY in gateway" engines_ready "$GATEWAY_ADMIN" 2 || exit 1
wait_for 120 "both engines READY in gateway-idle" engines_ready "$IDLE_ADMIN" 2 || exit 1
wait_for 120 "both engines READY in gateway-tls" engines_ready "$TLS_ADMIN" 2 || exit 1
ok "gateways ready, both engines healthy"

run_scenario a "SERVER share level, temporary table, DETACH" scenario_server_share
run_scenario b "GROUP share level is denied for a non-member" scenario_group_denied
run_scenario c "GROUP share level reaches the group's engine" scenario_group_engine
run_scenario d "a wrong token is rejected" scenario_wrong_token
run_scenario e "the admin API lists an open attachment" scenario_admin_sessions
run_scenario f "the idle timeout closes a quiet session" scenario_idle_timeout
run_scenario g "CREATE TABLE AS uploads rows with SEND_DATA" scenario_send_data
run_scenario h "the query time limit cancels long queries in every phase" scenario_query_timeout
run_scenario j "a hardened engine serves, but not quack_stop, the token or files" scenario_hardened_engine
run_scenario k "a restart closes the connections of clients that died meanwhile" scenario_orphans
run_scenario l "TLS: pinned and CA-verifying clients, and the ones that are refused" scenario_tls
run_scenario i "no panics in the gateway logs" scenario_no_panics

summary "compose e2e" || exit 1
