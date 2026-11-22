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
E2E_PROJECT=${E2E_PROJECT:-quackmux-e2e}

GATEWAY_ADMIN=http://127.0.0.1:$E2E_ADMIN_PORT
IDLE_ADMIN=http://127.0.0.1:$E2E_IDLE_ADMIN_PORT
COMPOSE=(docker compose -p "$E2E_PROJECT" --env-file "$ENV_FILE" -f "$COMPOSE_DIR/docker-compose.yaml")
# A fresh DuckDB CLI container for every client. Its stdin is the SQL script.
CLIENT=("${COMPOSE[@]}" run --rm -T --no-deps client)

need docker curl mkfifo awk sed grep
docker compose version >/dev/null 2>&1 || die "docker compose v2 is required"
# Writing to a session whose client died must fail the check, not kill the run.
trap '' PIPE

E2E_WORK=$(make_workdir)
ADMIN_HEADER_FILE=$E2E_WORK/admin.header
(umask 077 && printf 'Authorization: Bearer %s\n' "$E2E_ADMIN_TOKEN" >"$ADMIN_HEADER_FILE")

cleanup() {
	local rc=$?
	trap - EXIT
	session_kill_all
	if [ "$rc" -ne 0 ]; then
		step "container logs (last 150 lines each)"
		"${COMPOSE[@]}" logs --no-color --tail=150 >&2 || true
	fi
	if [ -n "${E2E_KEEP:-}" ]; then
		say "E2E_KEEP is set: the containers keep running. Remove them with"
		say "  docker compose -p $E2E_PROJECT --env-file $ENV_FILE -f $COMPOSE_DIR/docker-compose.yaml down -v"
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

# (h) Nothing above may have crashed a request handler.
scenario_no_panics() {
	local log=$E2E_WORK/h_gateways.log
	"${COMPOSE[@]}" logs --no-color gateway gateway-idle >"$log" 2>&1 || {
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
wait_for 120 "both engines READY in gateway" engines_ready "$GATEWAY_ADMIN" 2 || exit 1
wait_for 120 "both engines READY in gateway-idle" engines_ready "$IDLE_ADMIN" 2 || exit 1
ok "gateways ready, both engines healthy"

run_scenario a "SERVER share level, temporary table, DETACH" scenario_server_share
run_scenario b "GROUP share level is denied for a non-member" scenario_group_denied
run_scenario c "GROUP share level reaches the group's engine" scenario_group_engine
run_scenario d "a wrong token is rejected" scenario_wrong_token
run_scenario e "the admin API lists an open attachment" scenario_admin_sessions
run_scenario f "the idle timeout closes a quiet session" scenario_idle_timeout
run_scenario g "CREATE TABLE AS uploads rows with SEND_DATA" scenario_send_data
run_scenario h "no panics in the gateway logs" scenario_no_panics

summary "compose e2e" || exit 1
