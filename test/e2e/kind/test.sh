#!/usr/bin/env bash
#
# End-to-end scenarios against the kind cluster prepared by setup.sh: engine
# pods per share level, the loss of an engine pod, a gateway restart and
# engines with a data volume per key (persistent profile), all with real
# DuckDB 2.0 clients (see ../README.md).
#
#   bash test/e2e/kind/test.sh
#
# The clients run in the pod quackmux-e2e-client (the DuckDB CLI of the engine
# image) and reach the gateway through its Service. The admin API is reached
# through kubectl port-forward. The dev overlay uses auth.mode none, so the
# users come from the token option user=...; every run uses fresh user names.
#
# Optional environment (see env.sh for more):
#   ADMIN_TOKEN        bearer token of the admin API, if the overlay sets one
#   ADMIN_LOCAL_PORT   local port of the admin port-forward (default 19695)
#   QMX_SERVICE, QMX_SERVICE_PORT
#                      gateway Service and its Quack port (default: the
#                      Service in the namespace that exposes port 9494)
#   CLIENT_UID         user id of the client pod (default 1000, the user of the
#                      engine image)
#   ATTACH_TIMEOUT     seconds an ATTACH may take, including the start of an
#                      engine pod (default 180)
#   POD_GONE_TIMEOUT   seconds a CONNECTION engine pod may need to disappear
#                      after DETACH (default 30)
#   KEEP_CLIENT=1      keep the client pod after the run
set -euo pipefail
# shellcheck source=env.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/env.sh"

need kubectl curl mkfifo awk sed grep comm
# Writing to a session whose client died must fail the check, not kill the run.
trap '' PIPE

resolve_names
resolve_images
RUN_ID=${RUN_ID:-$(date +%s)}
ADMIN_LOCAL_PORT=${ADMIN_LOCAL_PORT:-19695}
ADMIN_URL=http://127.0.0.1:$ADMIN_LOCAL_PORT
ATTACH_TIMEOUT=${ATTACH_TIMEOUT:-180}
POD_GONE_TIMEOUT=${POD_GONE_TIMEOUT:-30}
CLIENT_CMD=(kubectl --context "$KUBE_CONTEXT" -n "$NS" exec -i "$CLIENT_POD" -- /usr/local/bin/duckdb)

E2E_WORK=$(make_workdir)
ADMIN_HEADER_FILE=''
if [ -n "${ADMIN_TOKEN:-}" ]; then
	ADMIN_HEADER_FILE=$E2E_WORK/admin.header
	(umask 077 && printf 'Authorization: Bearer %s\n' "$ADMIN_TOKEN" >"$ADMIN_HEADER_FILE")
fi
PF_PID=''

# Port-forward of the admin API --------------------------------------------------

pf_stop() {
	if [ -n "$PF_PID" ]; then
		kill "$PF_PID" 2>/dev/null || true
		wait "$PF_PID" 2>/dev/null || true
		PF_PID=''
	fi
}

# pf_start (re)starts the port-forward to a current gateway pod and waits until
# the admin API answers.
pf_start() {
	pf_stop
	(
		close_session_fds
		exec kubectl --context "$KUBE_CONTEXT" -n "$NS" port-forward "deployment/$DEPLOY" "$ADMIN_LOCAL_PORT:9495"
	) >"$E2E_WORK/port-forward.log" 2>&1 &
	PF_PID=$!
	if ! wait_for 30 "the admin API through kubectl port-forward" curl -fsS "$ADMIN_URL/readyz"; then
		indent "$E2E_WORK/port-forward.log"
		return 1
	fi
	api GET "$ADMIN_URL" /api/v1/engines "$E2E_WORK/pf-check.json"
	case $HTTP_STATUS in
	200) ;;
	401) die "the admin API wants a bearer token: set ADMIN_TOKEN" ;;
	*)
		bad "GET /api/v1/engines answered HTTP $HTTP_STATUS"
		return 1
		;;
	esac
}

# Engine pods ------------------------------------------------------------------

# terminate_engine POD ends an engine through the admin API (the engine id is
# the pod name) and waits until its pod is gone. The scenarios clean up their
# USER engines this way; the dev overlay allows only a few engines at a time.
terminate_engine() {
	api DELETE "$ADMIN_URL" "/api/v1/engines/$1" "$E2E_WORK/terminate.json"
	if [ "$HTTP_STATUS" != 202 ]; then
		bad "DELETE /api/v1/engines/$1 answered HTTP $HTTP_STATUS"
		indent "$E2E_WORK/terminate.json"
		return 1
	fi
	wait_for 90 "engine pod $1 to be deleted" pod_gone "$1" || return 1
	ok "DELETE /api/v1/engines/$1 removed the engine pod"
}

# Client sessions --------------------------------------------------------------

# secret_sql USER LEVEL [OPTIONS] prints the secret of a session; OPTIONS are
# further token options such as profile=persistent.
secret_sql() {
	printf "CREATE SECRET (TYPE quack, TOKEN 'e2e?user=%s&share_level=%s%s', SCOPE '%s');\n" "$1" "$2" "${3:+&$3}" "$QUACK_URI"
}
attach_sql() { printf "ATTACH '%s' AS remote (DISABLE_SSL true);\n" "$QUACK_URI"; }
# probe_sql TAG prints a query that runs on the engine and prints TAG=ok.
probe_sql() { printf "SELECT '%s=' || x AS result FROM remote.query('SELECT ''ok'' AS x');\n" "$1"; }

# open_client ID USER LEVEL [OPTIONS] starts a DuckDB CLI in the client pod,
# attaches the gateway as "remote" with share level LEVEL (and further token
# OPTIONS) and runs a first query on the engine (which may have to be started
# first).
open_client() {
	session_start "$1" "${CLIENT_CMD[@]}" || return 1
	session_send "$1" "$(client_preamble)
$(secret_sql "$2" "$3" "${4:-}")
$(attach_sql)
$(probe_sql "$1-attached")" || return 1
	session_wait "$1" "$1-attached=ok" "$ATTACH_TIMEOUT"
}

# close_client ID [ERRORS_EXPECTED] detaches and ends the session. The DuckDB
# CLI exits with status 1 if any of its statements failed; that is only an
# error unless ERRORS_EXPECTED is given.
close_client() {
	local rc=0
	session_send "$1" "DETACH remote;
SELECT '$1-detached' AS result;" || return 1
	session_wait "$1" "$1-detached" 60 || return 1
	session_end "$1" || rc=$?
	if [ "$rc" -ne 0 ] && [ -z "${2:-}" ]; then
		bad "the client of session $1 exited with status $rc"
		return 1
	fi
}

# Cleanup ----------------------------------------------------------------------

# cleanup_engines terminates the engines this run created (their pod names
# contain the run id: quack-user-e2e-bob-<run id>-...).
cleanup_engines() {
	local pods p
	pods=$(kctl -n "$NS" get pods -l app.kubernetes.io/component=engine \
		-o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | grep -F -- "$RUN_ID" || true)
	for p in $pods; do
		api DELETE "$ADMIN_URL" "/api/v1/engines/$p" "$E2E_WORK/delete.json"
		if [ "$HTTP_STATUS" != 202 ]; then
			kctl -n "$NS" delete pod "$p" --ignore-not-found --wait=false >/dev/null 2>&1 || true
		fi
	done
}

cleanup() {
	local rc=$?
	trap - EXIT
	session_kill_all
	if [ "$rc" -ne 0 ]; then diagnostics; fi
	key_watch_stop
	restore_gateway_config || rc=1
	cleanup_engines
	if [ -n "$MOUNT_POD" ]; then
		kctl -n "$NS" delete pod "$MOUNT_POD" --ignore-not-found --wait=true >/dev/null 2>&1 || true
	fi
	cleanup_volumes
	pf_stop
	if [ -z "${KEEP_CLIENT:-}" ]; then
		kctl -n "$NS" delete pod "$CLIENT_POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	fi
	rm -rf "$E2E_WORK"
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

scenario_failed_hook() {
	local f
	for f in "$E2E_WORK/$1"*.log; do
		if [ -f "$f" ]; then
			say "  client output ($(basename "$f")):"
			indent "$f"
		fi
	done
}

# Scenarios ----------------------------------------------------------------------

# k1: CONNECTION share level gives every ATTACH its own engine pod; the pod is
# removed when its session ends.
scenario_connection() {
	local user=e2e-alice-$RUN_ID before=$E2E_WORK/k1.before pods p1 p2
	engine_pods >"$before" || {
		bad "cannot list the engine pods"
		return 1
	}
	open_client k1_c1 "$user" connection || return 1
	pods=$(new_pods "$before")
	expect_eq "$(nlines "$pods")" 1 "the first CONNECTION session got an engine pod ($pods)" || return 1
	p1=$pods
	open_client k1_c2 "$user" connection || return 1
	pods=$(new_pods "$before")
	if ! expect_eq "$(nlines "$pods")" 2 "the second CONNECTION session got a second engine pod"; then
		printf '%s\n' "$pods" | indent
		return 1
	fi
	p2=$(printf '%s\n' "$pods" | grep -vx -- "$p1")
	ok "kubectl get pods -l app.kubernetes.io/component=engine shows $p1 and $p2"
	close_client k1_c1 || return 1
	if ! wait_for "$POD_GONE_TIMEOUT" "engine pod $p1 to disappear after DETACH" pod_gone "$p1"; then
		kctl -n "$NS" get pod "$p1" -o wide >&2 || true
		bad "the pod is still there: is the gateway's engine.checkInterval short and does the engine exit promptly on SIGTERM?"
		return 1
	fi
	ok "engine pod $p1 disappeared within ${POD_GONE_TIMEOUT}s of DETACH"
	pod_gone "$p2" && {
		bad "engine pod $p2 of the other session disappeared as well"
		return 1
	}
	ok "engine pod $p2 of the other session keeps running"
	close_client k1_c2 || return 1
}

# k2: USER share level: all sessions of a user share one engine pod (and its
# database).
scenario_user() {
	local user=e2e-bob-$RUN_ID before=$E2E_WORK/k2.before pods
	engine_pods >"$before" || {
		bad "cannot list the engine pods"
		return 1
	}
	open_client k2_u1 "$user" user || return 1
	session_send k2_u1 "CREATE TABLE remote.e2e_user_state AS SELECT 7 AS x;
SELECT 'k2_u1-created' AS result;" || return 1
	session_wait k2_u1 'k2_u1-created' 60 || return 1
	open_client k2_u2 "$user" user || return 1
	session_send k2_u2 "SELECT 'k2_u2-sees=' || CAST(x AS VARCHAR) AS result FROM remote.e2e_user_state;" || return 1
	session_wait k2_u2 'k2_u2-sees=7' 60 || return 1
	ok "the second session sees the table of the first one"
	pods=$(new_pods "$before")
	if ! expect_eq "$(nlines "$pods")" 1 "both USER sessions of $user use one engine pod ($pods)"; then
		printf '%s\n' "$pods" | indent
		return 1
	fi
	close_client k2_u1 || return 1
	close_client k2_u2 || return 1
	terminate_engine "$pods" || return 1
}

# k3: deleting the engine pod closes its sessions; the client gets a precise
# error on its next query, and DETACH + ATTACH gets it a new engine.
scenario_engine_loss() {
	local user=e2e-carol-$RUN_ID before=$E2E_WORK/k3.before log=$E2E_WORK/k3_l1.log pods p
	engine_pods >"$before" || {
		bad "cannot list the engine pods"
		return 1
	}
	open_client k3_l1 "$user" user || return 1
	pods=$(new_pods "$before")
	expect_eq "$(nlines "$pods")" 1 "the USER session got an engine pod ($pods)" || return 1
	p=$pods
	say "  deleting engine pod $p"
	kctl -n "$NS" delete pod "$p" --wait=true --timeout=120s >/dev/null || {
		bad "kubectl delete pod $p failed"
		return 1
	}
	wait_for 60 "the gateway to close the session of the deleted engine" session_count_is "$ADMIN_URL" "$user" 0 || return 1
	ok "the gateway closed the session when the pod disappeared"
	session_send k3_l1 "$(probe_sql k3_l1-after-delete)
SELECT 'k3_l1-checked' AS result;" || return 1
	session_wait k3_l1 'k3_l1-checked' 60 '' || return 1
	expect_output "$log" 'session closed' "the next query on the attachment fails with 'session closed'" || return 1
	expect_no_output "$log" 'k3_l1-after-delete=ok' "the query did not run" || return 1
	session_send k3_l1 "DETACH remote;
$(attach_sql)
$(probe_sql k3_l1-reattached)" || return 1
	session_wait k3_l1 'k3_l1-reattached=ok' "$ATTACH_TIMEOUT" '' || return 1
	ok "DETACH and ATTACH again work"
	pods=$(new_pods "$before")
	if [ "$(nlines "$pods")" != 1 ] || [ "$pods" = "$p" ]; then
		bad "expected one new engine pod instead of $p, found: $pods"
		return 1
	fi
	ok "the new session runs on the new engine pod $pods"
	close_client k3_l1 errors-expected || return 1
	terminate_engine "$pods" || return 1
}

# k4: "kubectl rollout restart" of the gateway keeps an existing attachment
# working: the new gateway process finds the engine pods again and adopts the
# session from its sealed connection id. (The dev overlay uses the Recreate
# strategy, so the gateway is briefly down; the client's heartbeats fail in
# the meantime, which the DuckDB client tolerates.)
scenario_gateway_restart() {
	local user=e2e-dave-$RUN_ID info=$E2E_WORK/k4_info.json list=$E2E_WORK/k4_sessions.json
	local log=$E2E_WORK/k4_r1.log before=$E2E_WORK/k4.before started_before started_after old_pods p engine_pod attempt=1
	engine_pods >"$before" || {
		bad "cannot list the engine pods"
		return 1
	}
	open_client k4_r1 "$user" user || return 1
	engine_pod=$(new_pods "$before")
	expect_eq "$(nlines "$engine_pod")" 1 "the USER session got an engine pod ($engine_pod)" || return 1
	expect_sessions "$ADMIN_URL" "$user" 1 "the gateway lists the session" || return 1
	api GET "$ADMIN_URL" /api/v1/info "$info"
	started_before=$(json_value "$info" startedAt)
	old_pods=$(gateway_pods) || {
		bad "cannot list the gateway pods"
		return 1
	}
	# shellcheck disable=SC2086 # one pod name per word
	say "  kubectl rollout restart deployment/$DEPLOY (pods: $(printf '%s ' $old_pods))"
	kctl -n "$NS" rollout restart "deployment/$DEPLOY" >/dev/null || return 1
	kctl -n "$NS" rollout status "deployment/$DEPLOY" --timeout=180s || return 1
	for p in $old_pods; do
		wait_for 120 "old gateway pod $p to terminate" pod_gone "$p" || return 1
	done
	pf_start || return 1
	api GET "$ADMIN_URL" /api/v1/info "$info"
	started_after=$(json_value "$info" startedAt)
	if [ -z "$started_after" ] || [ "$started_after" = "$started_before" ]; then
		bad "the admin API still reports the old gateway process (startedAt $started_after)"
		return 1
	fi
	ok "a new gateway process serves (started $started_after)"
	while :; do
		session_send k4_r1 "$(probe_sql "k4_r1-after-restart-$attempt")
SELECT 'k4_r1-attempt-$attempt' AS result;" || return 1
		session_wait k4_r1 "k4_r1-attempt-$attempt" 120 '' || return 1
		if grep -qF -- "k4_r1-after-restart-$attempt=ok" "$log"; then break; fi
		if [ "$attempt" -ge 3 ]; then
			bad "the attachment does not work after the gateway restart:"
			session_output k4_r1 | indent
			case $(cat "$log") in
			*'Invalid connection id'*) bad "the new gateway cannot open the connection id: do all gateway pods share the session key (QUACKMUX_SESSION_KEY)?" ;;
			esac
			return 1
		fi
		warn "query $attempt after the restart failed (only a request in flight during the restart may fail); retrying"
		attempt=$((attempt + 1))
		sleep 3
	done
	ok "the existing attachment keeps working after the restart"
	api GET "$ADMIN_URL" "/api/v1/sessions?user=$user" "$list"
	expect_eq "$(json_value "$list" count)" 1 "the new gateway lists the session" || return 1
	expect_eq "$(json_value "$list" adopted)" true "the new gateway adopted the session" || return 1
	expect_eq "$(json_value "$list" engineId)" "$engine_pod" "the session still runs on engine pod $engine_pod" || return 1
	if [ "$attempt" -gt 1 ]; then
		close_client k4_r1 errors-expected || return 1
	else
		close_client k4_r1 || return 1
	fi
	terminate_engine "$engine_pod" || return 1
}

# Persistent profile (profiles.persistent of the dev overlay) -----------------
#
# A USER engine of this profile mounts a PersistentVolumeClaim of its own
# (quackmux-data-...), and there is never more than one engine pod per key.
# The scenarios need a default StorageClass that supports ReadWriteOncePod
# (kind's local-path does).

PERSISTENT_OPTS=profile=persistent

# claim_field CLAIM JSONPATH prints a field of a claim.
claim_field() { kctl -n "$NS" get pvc "$1" -o jsonpath="$2" 2>/dev/null; }

claim_gone() {
	local out
	out=$(kctl -n "$NS" get pvc "$1" --ignore-not-found -o name 2>/dev/null) && [ -z "$out" ]
}

# engine_of USER prints the engine of the first open session of USER.
engine_of() {
	api GET "$ADMIN_URL" "/api/v1/sessions?user=$1" "$E2E_WORK/engine-of.json"
	json_value "$E2E_WORK/engine-of.json" engineId
}

# engine_message_has ID TEXT succeeds once the admin API shows TEXT in the
# message of engine ID.
engine_message_has() {
	api GET "$ADMIN_URL" "/api/v1/engines/$1" "$E2E_WORK/engine-message.json"
	grep -qF -- "$2" "$E2E_WORK/engine-message.json"
}

# set_gateway_config NAME points the gateway at ConfigMap NAME, waits for the
# rollout and restarts the port-forward.
set_gateway_config() {
	local idx old_pods p
	# shellcheck disable=SC2016 # a Go template, not shell
	idx=$(kctl -n "$NS" get deployment "$DEPLOY" \
		-o go-template='{{range $i, $v := .spec.template.spec.volumes}}{{if eq $v.name "config"}}{{$i}}{{end}}{{end}}') || return 1
	[ -n "$idx" ] || {
		bad "the gateway Deployment has no volume named config"
		return 1
	}
	old_pods=$(gateway_pods) || return 1
	kctl -n "$NS" patch deployment "$DEPLOY" --type json \
		-p "[{\"op\":\"replace\",\"path\":\"/spec/template/spec/volumes/$idx/configMap/name\",\"value\":\"$1\"}]" >/dev/null || return 1
	kctl -n "$NS" rollout status "deployment/$DEPLOY" --timeout=180s >/dev/null || return 1
	for p in $old_pods; do
		wait_for 120 "old gateway pod $p to terminate" pod_gone "$p" || return 1
	done
	pf_start
}

# query_with_retry ID SQL TAG runs SQL (which prints TAG=ok) in session ID,
# retrying twice: a request in flight during a gateway restart may fail.
query_with_retry() {
	local id=$1 sql=$2 tag=$3 attempt=1
	while :; do
		session_send "$id" "$sql
SELECT '$id-attempt-$attempt' AS result;" || return 1
		session_wait "$id" "$id-attempt-$attempt" 120 '' || return 1
		if grep -qF -- "$tag=ok" "$E2E_WORK/$id.log"; then return 0; fi
		if [ "$attempt" -ge 3 ]; then
			bad "session $id: '$tag' failed three times"
			return 1
		fi
		warn "query $attempt of session $id failed (a request during the restart may); retrying"
		attempt=$((attempt + 1))
		sleep 3
	done
}

# gateway_log_has SINCE TEXT... succeeds when one line of the gateway log
# since SINCE (RFC 3339) contains every TEXT.
gateway_log_has() {
	local since=$1 line t
	shift
	kctl -n "$NS" logs "deployment/$DEPLOY" --since-time="$since" >"$E2E_WORK/gateway-since.log" 2>/dev/null || return 1
	while IFS= read -r line; do
		for t in "$@"; do
			case $line in *"$t"*) ;; *) continue 2 ;; esac
		done
		return 0
	done <"$E2E_WORK/gateway-since.log"
	return 1
}

# k5: a USER engine of the persistent profile keeps its data on its own
# claim, and a key never has two engine pods. The engine is drained while
# its session stays attached: a second session of the key then waits (its
# ATTACH stays open) until the old pod is gone, and gets the next engine on
# the same claim, with the data. The data also survives the deletion of the
# engine pod.
scenario_persistent_data() {
	local user=e2e-erin-$RUN_ID p1 p2 p3 claim hash since events=$E2E_WORK/k5.events
	open_client k5_d1 "$user" user "$PERSISTENT_OPTS" || return 1
	p1=$(engine_of "$user")
	claim=$(claim_of "$p1")
	if [ -z "$p1" ] || [ -z "$claim" ]; then
		bad "the session of $user has no engine with a data volume (engine '$p1', claim '$claim')"
		return 1
	fi
	expect_eq "$(claim_field "$claim" '{.metadata.annotations.quackmux\.io/key}')" "USER/$user/default/persistent#0" \
		"engine pod $p1 mounts claim $claim of $user" || return 1
	expect_eq "$(claim_field "$claim" '{.spec.accessModes[0]} {.status.phase}')" "ReadWriteOncePod Bound" \
		"the claim is ReadWriteOncePod and bound" || return 1
	session_send k5_d1 "CREATE TABLE remote.e2e_persist AS SELECT 42 AS x;
SELECT 'k5_d1-created' AS result;" || return 1
	session_wait k5_d1 'k5_d1-created' 60 || return 1
	hash=$(key_hash_of "$p1")
	key_watch_start "$hash" "$events" || return 1

	since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
	api POST "$ADMIN_URL" "/api/v1/engines/$p1/drain" "$E2E_WORK/k5-drain.json"
	expect_eq "$HTTP_STATUS" 202 "POST /api/v1/engines/$p1/drain (its session stays attached)" || return 1
	session_start k5_d2 "${CLIENT_CMD[@]}" || return 1
	session_send k5_d2 "$(client_preamble)
$(secret_sql "$user" user "$PERSISTENT_OPTS")
$(attach_sql)
SELECT 'k5_d2-sees=' || CAST(x AS VARCHAR) AS result FROM remote.e2e_persist;" || return 1
	wait_for 60 "the gateway to log that the new session waits for $p1" \
		gateway_log_has "$since" "a new session waits for the previous engine of its key" "$p1" || return 1
	sleep 3
	case $(session_output k5_d2) in
	*k5_d2-sees=*)
		bad "the second session got an engine while $p1 still holds the data volume"
		return 1
		;;
	esac
	if pod_gone "$p1"; then
		bad "engine pod $p1 is gone although its session is still attached"
		return 1
	fi
	ok "the second session waits while the drained engine $p1 still has a session"
	close_client k5_d1 || return 1
	session_wait k5_d2 'k5_d2-sees=42' "$ATTACH_TIMEOUT" '' || return 1
	if ! pod_gone "$p1"; then
		bad "the next engine serves while the old pod $p1 still exists"
		return 1
	fi
	p2=$(engine_of "$user")
	if [ -z "$p2" ] || [ "$p2" = "$p1" ]; then
		bad "expected a new engine pod instead of $p1, found '$p2'"
		return 1
	fi
	expect_eq "$(claim_of "$p2")" "$claim" "once $p1 was gone, the next engine $p2 mounts the same claim and sees the table" || return 1

	say "  deleting engine pod $p2"
	kctl -n "$NS" delete pod "$p2" --wait=false >/dev/null || {
		bad "kubectl delete pod $p2 failed"
		return 1
	}
	wait_for 60 "the gateway to close the session of the deleted engine" session_count_is "$ADMIN_URL" "$user" 0 || return 1
	session_send k5_d2 "DETACH remote;
$(attach_sql)
SELECT 'k5_d2-again=' || CAST(x AS VARCHAR) AS result FROM remote.e2e_persist;" || return 1
	session_wait k5_d2 'k5_d2-again=42' "$ATTACH_TIMEOUT" '' || return 1
	ok "the table survived the deletion of the engine pod"
	expect_eq "$(claim_field "$claim" '{.metadata.name}')" "$claim" "the claim outlives the engine pods" || return 1
	p3=$(engine_of "$user")
	expect_eq "$(claim_of "$p3")" "$claim" "the engine pod $p3 after the deletion mounts the same claim" || return 1
	expect_one_pod_at_a_time "$events" "$hash" || return 1
	close_client k5_d2 errors-expected || return 1
	terminate_engine "$p3" || return 1
}

# k6: two users of the persistent profile get two claims, and neither sees
# the other's data; a subdomain outside allowedSubdomains gets no engine.
scenario_persistent_users() {
	local u1=e2e-frank-$RUN_ID u2=e2e-gina-$RUN_ID e1 e2 c1 c2
	open_client k6_a "$u1" user "$PERSISTENT_OPTS" || return 1
	open_client k6_b "$u2" user "$PERSISTENT_OPTS" || return 1
	e1=$(engine_of "$u1")
	e2=$(engine_of "$u2")
	c1=$(claim_of "$e1")
	c2=$(claim_of "$e2")
	if [ -z "$c1" ] || [ -z "$c2" ] || [ "$c1" = "$c2" ]; then
		bad "claims of $u1 and $u2: '$c1' and '$c2'"
		return 1
	fi
	ok "$u1 and $u2 have claims of their own ($c1, $c2)"
	expect_eq "$(claim_field "$c2" '{.metadata.annotations.quackmux\.io/key}')" "USER/$u2/default/persistent#0" \
		"claim $c2 belongs to $u2" || return 1
	session_send k6_a "CREATE TABLE remote.e2e_private AS SELECT 1 AS x;
SELECT 'k6_a-created' AS result;" || return 1
	session_wait k6_a 'k6_a-created' 60 || return 1
	session_send k6_b "SELECT count(*) FROM remote.e2e_private;
SELECT 'k6_b-checked' AS result;" || return 1
	session_wait k6_b 'k6_b-checked' 60 '' || return 1
	expect_output "$E2E_WORK/k6_b.log" 'does not exist' "$u2 does not see the table of $u1" || return 1
	session_start k6_c "${CLIENT_CMD[@]}" || return 1
	session_send k6_c "$(client_preamble)
$(secret_sql "$u2" user "$PERSISTENT_OPTS&subdomain=other")
$(attach_sql)
SELECT 'k6_c-done' AS result;" || return 1
	session_wait k6_c 'k6_c-done' 60 '' || return 1
	expect_output "$E2E_WORK/k6_c.log" 'does not allow subdomain' \
		"the persistent profile refuses a subdomain outside allowedSubdomains" || return 1
	session_end k6_c || true
	close_client k6_a || return 1
	close_client k6_b errors-expected || return 1
	terminate_engine "$e1" || return 1
	terminate_engine "$e2" || return 1
}

# k7: a profile change replaces the engine of a persistent key, and at no
# time do two engine pods of the key exist: the old engine keeps its
# sessions and takes new ones, ends once idle (after engine.recoveryGrace of
# the restarted gateway), and only then does its successor start, with the
# same data.
scenario_persistent_replace() {
	local user=e2e-hank-$RUN_ID e1 e2 claim hash orig changed grace events=$E2E_WORK/k7.events
	open_client k7_s1 "$user" user "$PERSISTENT_OPTS" || return 1
	e1=$(engine_of "$user")
	claim=$(claim_of "$e1")
	hash=$(key_hash_of "$e1")
	[ -n "$hash" ] || {
		bad "engine pod $e1 has no key-hash label"
		return 1
	}
	session_send k7_s1 "CREATE TABLE remote.e2e_generations AS SELECT 1 AS g;
SELECT 'k7_s1-created' AS result;" || return 1
	session_wait k7_s1 'k7_s1-created' 60 || return 1
	key_watch_start "$hash" "$events" || return 1

	orig=$(kctl -n "$NS" get deployment "$DEPLOY" -o jsonpath='{.spec.template.spec.volumes[?(@.name=="config")].configMap.name}')
	changed=quackmux-config-e2e-$RUN_ID
	kctl -n "$NS" get configmap "$orig" -o go-template='{{index .data "config.yaml"}}' |
		awk -v gen="$RUN_ID" '{ print } /^  persistent:$/ { print "    labels: {quackmux-e2e-generation: \"" gen "\"}" }' \
			>"$E2E_WORK/config-changed.yaml"
	grep -qF quackmux-e2e-generation "$E2E_WORK/config-changed.yaml" || {
		bad "the gateway configuration has no profile 'persistent'"
		return 1
	}
	grace=$(sed -n 's/^  recoveryGrace: *\([0-9]*\)s$/\1/p' "$E2E_WORK/config-changed.yaml")
	kctl -n "$NS" create configmap "$changed" --from-file=config.yaml="$E2E_WORK/config-changed.yaml" >/dev/null || return 1
	PERSIST_RESTORE_CONFIG=$orig
	PERSIST_CHANGED_CONFIG=$changed
	say "  changing the profile (ConfigMap $changed) and rolling out the gateway"
	set_gateway_config "$changed" || return 1

	query_with_retry k7_s1 "$(probe_sql k7_s1-after-change)" k7_s1-after-change || return 1
	wait_for 60 "engine $e1 to wait for its replacement" engine_message_has "$e1" 'replacement pending' || return 1
	ok "the outdated engine $e1 keeps serving; its replacement is pending"
	open_client k7_s2 "$user" user "$PERSISTENT_OPTS" || return 1
	session_send k7_s2 "SELECT 'k7_s2-sees=' || CAST(g AS VARCHAR) AS result FROM remote.e2e_generations;" || return 1
	session_wait k7_s2 'k7_s2-sees=1' 60 || return 1
	api GET "$ADMIN_URL" "/api/v1/engines/$e1" "$E2E_WORK/k7-engine.json"
	expect_eq "$(json_value "$E2E_WORK/k7-engine.json" sessions)" 2 "a new session joins the outdated engine $e1" || return 1

	close_client k7_s2 || return 1
	close_client k7_s1 errors-expected || return 1
	wait_for "$((${grace:-180} + 90))" "the idle outdated engine $e1 to end" pod_gone "$e1" || return 1
	ok "engine pod $e1 is gone"
	open_client k7_s3 "$user" user "$PERSISTENT_OPTS" || return 1
	e2=$(engine_of "$user")
	if [ -z "$e2" ] || [ "$e2" = "$e1" ]; then
		bad "expected a new engine instead of $e1, found '$e2'"
		return 1
	fi
	expect_eq "$(claim_of "$e2")" "$claim" "the successor $e2 mounts the same claim" || return 1
	session_send k7_s3 "SELECT 'k7_s3-sees=' || CAST(g AS VARCHAR) AS result FROM remote.e2e_generations;" || return 1
	session_wait k7_s3 'k7_s3-sees=1' 60 || return 1
	ok "the successor sees the data of its predecessor"
	expect_one_pod_at_a_time "$events" "$hash" || return 1
	close_client k7_s3 || return 1
	terminate_engine "$e2" || return 1
	restore_gateway_config || return 1
}

# restore_gateway_config undoes the configuration change of k7.
PERSIST_RESTORE_CONFIG=''
PERSIST_CHANGED_CONFIG=''
restore_gateway_config() {
	[ -n "$PERSIST_RESTORE_CONFIG" ] || return 0
	say "  restoring the gateway configuration ($PERSIST_RESTORE_CONFIG)"
	set_gateway_config "$PERSIST_RESTORE_CONFIG" || return 1
	kctl -n "$NS" delete configmap "$PERSIST_CHANGED_CONFIG" --ignore-not-found >/dev/null || true
	PERSIST_RESTORE_CONFIG=''
}

# mount_claim_pod NAME CLAIM starts a pod that is no engine (like a debug
# pod) and mounts CLAIM; cleanup deletes it if a scenario fails.
MOUNT_POD=''
mount_claim_pod() {
	MOUNT_POD=$1
	kctl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
  labels:
    app.kubernetes.io/part-of: quackmux-e2e
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  terminationGracePeriodSeconds: 1
  securityContext:
    runAsNonRoot: true
    runAsUser: $CLIENT_UID
    runAsGroup: $CLIENT_UID
    seccompProfile:
      type: RuntimeDefault
  containers:
    - name: debug
      image: $ENGINE_IMAGE
      imagePullPolicy: IfNotPresent
      command: ["/usr/local/bin/duckdb"]
      stdin: true
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      resources:
        requests:
          cpu: 10m
          memory: 32Mi
      volumeMounts:
        - name: data
          mountPath: /data
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: $2
EOF
}

# k8: the admin API lists the claim with its engine and refuses to delete it
# while the engine exists, and while a pod that is no engine mounts it (the
# gateway asks the API server, not its registry). Then it deletes it.
scenario_volume_admin() {
	local user=e2e-ivy-$RUN_ID e claim out=$E2E_WORK/k8.json debug=quackmux-e2e-debug-$RUN_ID
	open_client k8_v "$user" user "$PERSISTENT_OPTS" || return 1
	e=$(engine_of "$user")
	claim=$(claim_of "$e")
	api GET "$ADMIN_URL" "/api/v1/volumes?owner=$user" "$out"
	expect_eq "$HTTP_STATUS $(json_value "$out" name) $(json_value "$out" engine)" "200 $claim $e" \
		"GET /api/v1/volumes lists claim $claim with engine $e" || return 1
	api DELETE "$ADMIN_URL" "/api/v1/volumes/$claim" "$out"
	expect_eq "$HTTP_STATUS" 409 "DELETE /api/v1/volumes/$claim is refused while the engine exists" || return 1
	close_client k8_v || return 1
	terminate_engine "$e" || return 1
	mount_claim_pod "$debug" "$claim" || return 1
	api DELETE "$ADMIN_URL" "/api/v1/volumes/$claim" "$out"
	expect_eq "$HTTP_STATUS" 409 "DELETE /api/v1/volumes/$claim is refused while pod $debug mounts it" || return 1
	expect_output "$out" "pod $debug mounts it" "the gateway names the pod" || return 1
	kctl -n "$NS" delete pod "$debug" --wait=true >/dev/null || return 1
	MOUNT_POD=''
	api DELETE "$ADMIN_URL" "/api/v1/volumes/$claim" "$out"
	expect_eq "$HTTP_STATUS" 202 "DELETE /api/v1/volumes/$claim is accepted once no pod uses it" || return 1
	wait_for 60 "claim $claim to be deleted" claim_gone "$claim" || return 1
	ok "claim $claim is deleted"
}

# cleanup_volumes deletes the claims of this run's users (their keys contain
# the run id; a long name may be cut short); claims of engines that still stop
# are deleted once they are gone.
cleanup_volumes() {
	local c
	for c in $(kctl -n "$NS" get pvc -l app.kubernetes.io/component=engine-data \
		-o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.annotations.quackmux\.io/key}{"\n"}{end}' 2>/dev/null |
		awk -v id="$RUN_ID" 'index($2, id) { print $1 }' || true); do
		api DELETE "$ADMIN_URL" "/api/v1/volumes/$c" "$E2E_WORK/delete-volume.json"
		if [ "$HTTP_STATUS" != 202 ]; then
			kctl -n "$NS" delete pvc "$c" --ignore-not-found --wait=false >/dev/null 2>&1 || true
		fi
	done
}

# Main -----------------------------------------------------------------------------

step "prerequisites"
kctl get namespace "$NS" >/dev/null 2>&1 || die "namespace $NS not found with context $KUBE_CONTEXT: run setup.sh first"
kctl -n "$NS" rollout status "deployment/$DEPLOY" --timeout=120s >/dev/null || die "deployment/$DEPLOY in $NS is not ready"
if [ -n "${QMX_SERVICE:-}" ]; then
	SVC=$QMX_SERVICE
	SVC_PORT=${QMX_SERVICE_PORT:-9494}
else
	# The first Service that exposes the Quack port (9494 or the port "quack").
	svc_line=$(kctl -n "$NS" get services \
		-o jsonpath='{range .items[*]}{.metadata.name}{range .spec.ports[*]}{" "}{.port}{"/"}{.targetPort}{end}{"\n"}{end}' |
		awk '!found { for (i = 2; i <= NF; i++) { split($i, p, "/"); if (p[1] == "9494" || p[2] == "9494" || p[2] == "quack") { print $1, p[1]; found = 1; break } } }') || svc_line=''
	SVC='' SVC_PORT=''
	read -r SVC SVC_PORT <<<"$svc_line" || true
fi
[ -n "$SVC" ] || die "no Service with port 9494 in namespace $NS (set QMX_SERVICE and QMX_SERVICE_PORT)"
QUACK_URI=quack:$SVC:$SVC_PORT
# k5-k8 create claims without a storage class: the cluster needs a default
# one that supports ReadWriteOncePod (kind's "standard", local-path, does).
default_sc=$(kctl get storageclass -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.annotations.storageclass\.kubernetes\.io/is-default-class}{"\n"}{end}' |
	awk '$2 == "true" && !found { print $1; found = 1 }') || default_sc=''
[ -n "$default_sc" ] || die "no default StorageClass: the persistence scenarios k5-k8 need one that supports ReadWriteOncePod"
ensure_client_pod || die "the client pod $CLIENT_POD did not start (image $ENGINE_IMAGE, uid $CLIENT_UID)"
pf_start || die "cannot reach the admin API"
ok "gateway deployment/$DEPLOY in namespace $NS, clients attach $QUACK_URI, run id $RUN_ID, StorageClass $default_sc"

run_scenario k1 "CONNECTION share level: an engine pod per session, removed on DETACH" scenario_connection
run_scenario k2 "USER share level: one engine pod per user" scenario_user
run_scenario k3 "a deleted engine pod closes its sessions; ATTACH again works" scenario_engine_loss
run_scenario k4 "a gateway restart keeps existing attachments (session adoption)" scenario_gateway_restart
run_scenario k5 "persistence: a new session waits for the drained engine; the data survives" scenario_persistent_data
run_scenario k6 "persistence: two users get two claims" scenario_persistent_users
run_scenario k7 "persistence: a profile change replaces the engine without overlap" scenario_persistent_replace
run_scenario k8 "persistence: the admin API lists and deletes data volumes" scenario_volume_admin

summary "kind e2e" || exit 1
