#!/usr/bin/env bash
#
# End-to-end scenarios against the kind cluster prepared by setup.sh: engine
# pods per share level, the loss of an engine pod and a gateway restart, all
# with real DuckDB 2.0 clients (see ../README.md).
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
CLIENT_POD=${CLIENT_POD:-quackmux-e2e-client}
CLIENT_UID=${CLIENT_UID:-1000}
ADMIN_LOCAL_PORT=${ADMIN_LOCAL_PORT:-19695}
ADMIN_URL=http://127.0.0.1:$ADMIN_LOCAL_PORT
ATTACH_TIMEOUT=${ATTACH_TIMEOUT:-180}
POD_GONE_TIMEOUT=${POD_GONE_TIMEOUT:-30}
# Engine pods of sessions; warm-pool pods (not yet claimed) do not count.
ENGINE_SELECTOR='app.kubernetes.io/component=engine,quackmux.io/warm!=true'
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

# engine_pods prints the names of the engine pods that are neither warm nor
# terminating, sorted.
engine_pods() {
	kctl -n "$NS" get pods -l "$ENGINE_SELECTOR" \
		-o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.deletionTimestamp}{"\n"}{end}' |
		awk 'NF == 1 { print $1 }' | LC_ALL=C sort
}

# new_pods BEFORE_FILE prints the engine pods that are not in BEFORE_FILE.
new_pods() { engine_pods | LC_ALL=C comm -13 "$1" -; }

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

secret_sql() {
	printf "CREATE SECRET (TYPE quack, TOKEN 'e2e?user=%s&share_level=%s', SCOPE '%s');\n" "$1" "$2" "$QUACK_URI"
}
attach_sql() { printf "ATTACH '%s' AS remote (DISABLE_SSL true);\n" "$QUACK_URI"; }
# probe_sql TAG prints a query that runs on the engine and prints TAG=ok.
probe_sql() { printf "SELECT '%s=' || x AS result FROM remote.query('SELECT ''ok'' AS x');\n" "$1"; }

# open_client ID USER LEVEL starts a DuckDB CLI in the client pod, attaches the
# gateway as "remote" with share level LEVEL and runs a first query on the
# engine (which may have to be started first).
open_client() {
	session_start "$1" "${CLIENT_CMD[@]}" || return 1
	session_send "$1" "$(client_preamble)
$(secret_sql "$2" "$3")
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

ensure_client_pod() {
	kctl -n "$NS" delete pod "$CLIENT_POD" --ignore-not-found --wait=true >/dev/null
	kctl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $CLIENT_POD
  labels:
    app.kubernetes.io/name: quackmux-e2e-client
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
    - name: duckdb
      image: $ENGINE_IMAGE
      imagePullPolicy: IfNotPresent
      # A DuckDB CLI that waits on its (open) stdin keeps the pod running;
      # the test starts further CLIs in it with kubectl exec.
      command: ["/usr/local/bin/duckdb"]
      stdin: true
      env:
        - name: HOME
          value: /tmp
      securityContext:
        allowPrivilegeEscalation: false
        readOnlyRootFilesystem: true
        capabilities:
          drop: ["ALL"]
      resources:
        requests:
          cpu: 50m
          memory: 128Mi
        limits:
          memory: 1Gi
      volumeMounts:
        - name: tmp
          mountPath: /tmp
  volumes:
    - name: tmp
      emptyDir: {}
EOF
	kctl -n "$NS" wait --for=condition=Ready "pod/$CLIENT_POD" --timeout=120s >/dev/null
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
	cleanup_engines
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
ensure_client_pod || die "the client pod $CLIENT_POD did not start (image $ENGINE_IMAGE, uid $CLIENT_UID)"
pf_start || die "cannot reach the admin API"
ok "gateway deployment/$DEPLOY in namespace $NS, clients attach $QUACK_URI, run id $RUN_ID"

run_scenario k1 "CONNECTION share level: an engine pod per session, removed on DETACH" scenario_connection
run_scenario k2 "USER share level: one engine pod per user" scenario_user
run_scenario k3 "a deleted engine pod closes its sessions; ATTACH again works" scenario_engine_loss
run_scenario k4 "a gateway restart keeps existing attachments (session adoption)" scenario_gateway_restart

summary "kind e2e" || exit 1
