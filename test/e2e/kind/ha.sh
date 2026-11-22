#!/usr/bin/env bash
#
# High availability in the kind cluster prepared by setup.sh: the gateway runs
# as two replicas with leader election (deploy/kubernetes/overlays/ha), and
# real DuckDB 2.0 clients keep their attachments through failovers (see
# ../README.md).
#
#   bash test/e2e/kind/ha.sh
#
# The script does what deploy/kubernetes/README.md asks of an operator: it
# generates fresh secrets into an overlay on top of overlays/ha that uses the
# locally built gateway image (test/e2e/kind/.work/ha-overlay), stops the
# running gateway and applies the overlay. The scenarios:
#
#   h1  both replicas are ready; the leader alone carries the label
#       quackmux.io/leader and is the only endpoint of the Service; a client
#       that connects to the follower directly gets a Quack error
#   h2  deleting the leader hands over within seconds; attachments survive
#   h3  a frozen leader (SIGSTOP) loses the lease and its label to the
#       follower; when it resumes it steps down without closing the sessions
#       it still knows and comes back as a ready follower
#   h4  "kubectl rollout restart" completes on its own ("kubectl rollout
#       status" succeeds) with exactly one handover, to a new pod
#   h5  the PodDisruptionBudget lets the leader be evicted while the follower
#       is ready (it hands over), but never the last ready replica
#
# Two clients of one user stay attached through all scenarios: one with the
# default heartbeat timeout (60 s) and one with HEARTBEAT_TIMEOUT 5, whose
# session lease (5 s + session.heartbeatGrace 15 s) is shorter than h3 keeps
# the leader frozen. Afterwards the cluster runs the dev overlay again, so
# that test.sh works as before. h3 freezes the gateway process on its kind
# node (docker exec).
#
# Optional environment (see env.sh for more):
#   HA_OVERLAY         the overlay under test, relative to the generated one
#                      (default ../../../../../deploy/kubernetes/overlays/ha)
#   HA_LEASE           name of the lease (default quackmux-default)
#   HA_FREEZE_SECONDS  how long h3 freezes the leader (default 45: longer than
#                      the session lease of the second client, shorter than
#                      the liveness probe of the gateway tolerates, about 60 s)
#   HA_HANDOVER_SECONDS  how long h2 may take from deleting the leader until
#                      another replica leads (default 12: the old leader
#                      releases the lease; an expiring one takes 15 s or more)
#   HA_GAP_SECONDS     how long the Service may lack a ready endpoint during
#                      an orderly handover (h2, h4, h5; default 8: a follower
#                      tries every ha.retryPeriod plus jitter, up to 4.4 s)
#   ADMIN_LOCAL_PORT   local port of the admin port-forward (default 19696)
#   ATTACH_TIMEOUT     seconds an ATTACH may take (default 180)
#   KEEP_HA=1          keep the HA deployment and the generated overlay (with
#                      its secrets) after the run
set -euo pipefail
# shellcheck source=env.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/env.sh"

need kubectl kind curl docker mkfifo awk sed grep od tr
# Writing to a session whose client died must fail the check, not kill the run.
trap '' PIPE

# The dev overlay names the namespace, the Deployment and the images that
# setup.sh loaded into the cluster; the HA overlay uses the same names.
DEV_OVERLAY_DIR=$OVERLAY_DIR
resolve_names
resolve_images

HA_WORK=$KIND_DIR/.work
HA_DIR=$HA_WORK/ha-overlay
HA_OVERLAY=${HA_OVERLAY:-../../../../../deploy/kubernetes/overlays/ha}
HA_LEASE=${HA_LEASE:-quackmux-default}
HA_FREEZE_SECONDS=${HA_FREEZE_SECONDS:-45}
HA_HANDOVER_SECONDS=${HA_HANDOVER_SECONDS:-12}
HA_GAP_SECONDS=${HA_GAP_SECONDS:-8}
LEADER_LABEL=quackmux.io/leader
RUN_ID=${RUN_ID:-$(date +%s)}
HA_USER=e2e-ha-$RUN_ID
ADMIN_LOCAL_PORT=${ADMIN_LOCAL_PORT:-19696}
ADMIN_URL=http://127.0.0.1:$ADMIN_LOCAL_PORT
ATTACH_TIMEOUT=${ATTACH_TIMEOUT:-180}
CLIENT_CMD=(kubectl --context "$KUBE_CONTEXT" -n "$NS" exec -i "$CLIENT_POD" -- /usr/local/bin/duckdb)

E2E_WORK=$(make_workdir)
ADMIN_HEADER_FILE=''
PF_PID=''
LEADER='' FOLLOWER='' NEW_LEADER=''
ENGINE_POD=''
SWITCHED=''
FROZEN_NODE='' FROZEN_PID=''
CLIENTS_OPEN=''
GAP_PID='' GAP=''

# Generated overlay ----------------------------------------------------------------

# random_token prints 64 random hex digits (32 bytes, like "quackmux token").
random_token() { od -An -N32 -tx1 /dev/urandom | tr -d ' \n'; }

sha256_hex() {
	if command -v sha256sum >/dev/null 2>&1; then
		printf '%s' "$1" | sha256sum
	elif command -v shasum >/dev/null 2>&1; then
		printf '%s' "$1" | shasum -a 256
	else
		printf '%s' "$1" | openssl dgst -sha256 -r
	fi | awk '{ print $1 }'
}

# make_overlay writes HA_DIR: HA_OVERLAY with the gateway image of the dev
# overlay and freshly generated secrets (deploy/kubernetes/README.md, "Echte
# Secrets erzeugen").
make_overlay() {
	local name=${GATEWAY_IMAGE%:*} tag=${GATEWAY_IMAGE##*:}
	case $GATEWAY_IMAGE in *@*) die "GATEWAY_IMAGE with a digest is not supported: $GATEWAY_IMAGE" ;; esac
	case $tag in */* | "$GATEWAY_IMAGE") name=$GATEWAY_IMAGE tag=latest ;; esac
	CLIENT_TOKEN=$(random_token)
	ADMIN_TOKEN=$(random_token)
	rm -rf "$HA_DIR"
	(
		umask 077
		mkdir -p "$HA_DIR/secrets"
		random_token >"$HA_DIR/secrets/session-key"
		printf '%s\n' "$ADMIN_TOKEN" >"$HA_DIR/secrets/admin-token"
		cat >"$HA_DIR/secrets/users.yaml" <<EOF
users:
  - name: $HA_USER
    tokens:
      - sha256: "$(sha256_hex "$CLIENT_TOKEN")"
        description: test/e2e/kind/ha.sh
EOF
		cat >"$HA_DIR/kustomization.yaml" <<EOF
# Generated by test/e2e/kind/ha.sh. The secrets are throwaway values.
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - $HA_OVERLAY
images:
  - name: ghcr.io/lldeters/quackmux
    newName: $name
    newTag: $tag
secretGenerator:
  - name: quackmux-session-key
    behavior: replace
    files: [key=secrets/session-key]
  - name: quackmux-admin-token
    behavior: replace
    files: [token=secrets/admin-token]
  - name: quackmux-users
    behavior: replace
    files: [users.yaml=secrets/users.yaml]
EOF
	)
	ADMIN_HEADER_FILE=$E2E_WORK/admin.header
	(umask 077 && printf 'Authorization: Bearer %s\n' "$ADMIN_TOKEN" >"$ADMIN_HEADER_FILE")
}

# Gateway pods and the lease -------------------------------------------------------

gateway_selector() {
	local s
	# shellcheck disable=SC2016 # a Go template, not shell
	s=$(kctl -n "$NS" get deployment "$DEPLOY" \
		-o go-template='{{range $k, $v := .spec.selector.matchLabels}}{{$k}}={{$v}},{{end}}') || return 1
	printf '%s' "${s%,}"
}

# gateway_status prints "NAME READY LEADER" for every gateway pod that is
# running and not being deleted: READY is True or False, LEADER the value of
# the label quackmux.io/leader ("-" without it).
gateway_status() {
	local selector
	selector=$(gateway_selector) || return 1
	# shellcheck disable=SC2016 # a Go template, not shell
	kctl -n "$NS" get pods -l "$selector" -o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{if eq .status.phase "Running"}}{{.metadata.name}} {{$r := "False"}}{{range .status.conditions}}{{if eq .type "Ready"}}{{$r = .status}}{{end}}{{end}}{{$r}} {{or (index .metadata.labels "quackmux.io/leader") "-"}}{{"\n"}}{{end}}{{end}}{{end}}'
}

# label_of POD prints the value of the leader label of POD ("" without it).
label_of() { kctl -n "$NS" get pod "$1" -o go-template='{{index .metadata.labels "quackmux.io/leader"}}' 2>/dev/null | sed 's/<no value>//'; }

# deletion_cost POD prints the pod-deletion-cost annotation of POD ("" without
# it).
deletion_cost() {
	kctl -n "$NS" get pod "$1" -o go-template='{{index .metadata.annotations "controller.kubernetes.io/pod-deletion-cost"}}' 2>/dev/null |
		sed 's/<no value>//'
}

# unlabelled POD succeeds when POD exists and carries no leader label.
unlabelled() {
	kctl -n "$NS" get pod "$1" >/dev/null 2>&1 || return 1
	[ -z "$(label_of "$1")" ]
}

pod_ip() { kctl -n "$NS" get pod "$1" -o jsonpath='{.status.podIP}'; }

# service_endpoints prints the ready addresses of the Service the clients use.
service_endpoints() {
	kctl -n "$NS" get endpointslices -l "kubernetes.io/service-name=$SVC" \
		-o go-template='{{range .items}}{{range .endpoints}}{{if .conditions.ready}}{{index .addresses 0}}{{"\n"}}{{end}}{{end}}{{end}}'
}

# endpoints_are POD succeeds when the Service has exactly one ready endpoint,
# POD.
endpoints_are() {
	local ip
	ip=$(pod_ip "$1") || return 1
	[ -n "$ip" ] && [ "$(service_endpoints)" = "$ip" ]
}

lease_transitions() { kctl -n "$NS" get lease "$HA_LEASE" -o jsonpath='{.spec.leaseTransitions}' 2>/dev/null; }

lease_holder() { kctl -n "$NS" get lease "$HA_LEASE" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null; }

# one_leader succeeds when exactly two gateway pods run, both are ready,
# exactly one of them carries the leader label, that one holds the lease, and
# it is the only endpoint of the Service. It sets LEADER and FOLLOWER.
one_leader() {
	local st labelled holder
	st=$(gateway_status) || return 1
	[ "$(nlines "$st")" = 2 ] || return 1
	[ "$(printf '%s\n' "$st" | awk '$2 == "True"' | grep -c .)" = 2 ] || return 1
	labelled=$(printf '%s\n' "$st" | awk '$3 == "true" { print $1 }')
	[ "$(nlines "$labelled")" = 1 ] || return 1
	holder=$(lease_holder) || return 1
	[ "$holder" = "$labelled" ] || return 1
	endpoints_are "$labelled" || return 1
	LEADER=$labelled
	FOLLOWER=$(printf '%s\n' "$st" | awk '$3 != "true" { print $1 }')
}

# leader_is POD succeeds once POD holds the lease, is ready, carries the
# leader label and is the only endpoint of the Service.
leader_is() {
	[ "$(lease_holder)" = "$1" ] || return 1
	gateway_status | awk -v p="$1" '$1 == p && $2 == "True" && $3 == "true" { f = 1 } END { exit !f }' || return 1
	endpoints_are "$1"
}

# leader_other_than POD succeeds once a pod other than POD leads (leader_is).
# It sets NEW_LEADER.
leader_other_than() {
	local holder
	holder=$(lease_holder) || return 1
	[ -n "$holder" ] && [ "$holder" != "$1" ] || return 1
	leader_is "$holder" || return 1
	NEW_LEADER=$holder
}

no_gateway_pods() { [ -z "$(gateway_pods 2>/dev/null)" ]; }

# deployment_counts prints "REPLICAS UPDATED AVAILABLE" of the Deployment.
deployment_counts() {
	kctl -n "$NS" get deployment "$DEPLOY" \
		-o jsonpath='{.status.replicas} {.status.updatedReplicas} {.status.availableReplicas}'
}

# rollout_done succeeds when the Deployment runs two available pods of its
# newest template, one of them the leader (UP-TO-DATE 2, AVAILABLE 2).
rollout_done() { one_leader && [ "$(deployment_counts)" = "2 2 2" ]; }

# template_hash POD prints the pod-template-hash (the ReplicaSet) of POD.
template_hash() { kctl -n "$NS" get pod "$1" -o go-template='{{index .metadata.labels "pod-template-hash"}}'; }

restart_count() {
	kctl -n "$NS" get pod "$1" -o jsonpath='{.status.containerStatuses[?(@.name=="gateway")].restartCount}'
}

# restarted_follower POD COUNT succeeds once the gateway container of POD has
# restarted more than COUNT times and runs as a ready follower again (ready,
# without the leader label).
restarted_follower() {
	local n
	n=$(restart_count "$1") || return 1
	[ -n "$n" ] && [ "$n" -gt "$2" ] || return 1
	gateway_status | awk -v p="$1" '$1 == p && $2 == "True" && $3 == "-" { f = 1 } END { exit !f }'
}

# node_pid POD NODE prints the process id of the gateway container of POD in the
# PID namespace of its kind node.
node_pid() {
	local cid
	cid=$(kctl -n "$NS" get pod "$1" -o jsonpath='{.status.containerStatuses[?(@.name=="gateway")].containerID}') || return 1
	[ -n "$cid" ] || return 1
	docker exec "$2" crictl inspect --output go-template --template '{{.info.pid}}' "${cid#*://}"
}

# evict POD asks the API server to evict POD (what kubectl drain does).
evict() {
	kctl create --raw "/api/v1/namespaces/$NS/pods/$1/eviction" -f - <<EOF
{"apiVersion": "policy/v1", "kind": "Eviction", "metadata": {"name": "$1", "namespace": "$NS"}}
EOF
}

# Admin API ------------------------------------------------------------------------

pf_stop() {
	if [ -n "$PF_PID" ]; then
		kill "$PF_PID" 2>/dev/null || true
		wait "$PF_PID" 2>/dev/null || true
		PF_PID=''
	fi
}

# pf_start POD (re)starts the port-forward to the admin port of POD.
pf_start() {
	pf_stop
	(
		close_session_fds
		exec kubectl --context "$KUBE_CONTEXT" -n "$NS" port-forward "pod/$1" "$ADMIN_LOCAL_PORT:9495"
	) >"$E2E_WORK/port-forward.log" 2>&1 &
	PF_PID=$!
	if ! wait_for 30 "the admin API of $1 through kubectl port-forward" curl -fsS "$ADMIN_URL/healthz"; then
		indent "$E2E_WORK/port-forward.log"
		return 1
	fi
}

# is_leader_metric prints the value of quackmux_is_leader of the pod behind the
# port-forward.
is_leader_metric() {
	curl -fsS "$ADMIN_URL/metrics" 2>/dev/null | awk '$1 == "quackmux_is_leader" { print $2 }'
}

# leader_label_metric prints quackmux_leader_label of the pod behind the
# port-forward.
leader_label_metric() {
	curl -fsS "$ADMIN_URL/metrics" 2>/dev/null | awk '$1 == "quackmux_leader_label" { print $2 }'
}

# Service endpoints during a handover -----------------------------------------------

# ms prints the time in milliseconds (seconds * 1000 where date lacks %N).
ms() {
	local t
	t=$(date +%s%3N)
	case $t in *N) echo $(($(date +%s) * 1000)) ;; *) echo "$t" ;; esac
}

# gap_watch_start NAME samples the number of ready endpoints of the Service
# about five times a second until gap_watch_stop. A sample that fails (an
# API server hiccup) is skipped, not counted as a gap.
gap_watch_start() {
	local out=$E2E_WORK/gap-$1.txt eps
	gap_watch_kill
	: >"$out"
	(
		close_session_fds
		while :; do
			if eps=$(service_endpoints 2>/dev/null); then
				printf '%s %s\n' "$(ms)" "$(nlines "$eps")"
			fi
			sleep 0.2
		done
	) >>"$out" 2>/dev/null &
	GAP_PID=$!
}

gap_watch_kill() {
	if [ -n "$GAP_PID" ]; then
		kill "$GAP_PID" 2>/dev/null || true
		wait "$GAP_PID" 2>/dev/null || true
		GAP_PID=''
	fi
}

# gap_watch_stop NAME stops the sampling and sets GAP to the longest time in
# seconds (one decimal) in which the Service had no ready endpoint, from the
# first sample without one to the next sample with one (or the last sample).
gap_watch_stop() {
	gap_watch_kill
	GAP=$(awk '$2 == 0 && !s { s = $1 } $2 != 0 && s { if ($1 - s > m) m = $1 - s; s = 0 } { last = $1 }
		END { if (s && last - s > m) m = last - s; printf "%.1f\n", m / 1000 }' "$E2E_WORK/gap-$1.txt")
}

# expect_gap NAME DESCRIPTION: the Service lacked a ready endpoint for at most
# HA_GAP_SECONDS.
expect_gap() {
	gap_watch_stop "$1"
	if awk -v g="$GAP" -v max="$HA_GAP_SECONDS" 'BEGIN { exit !(g <= max) }'; then
		ok "$2: the Service had no ready endpoint for ${GAP}s (at most ${HA_GAP_SECONDS}s)"
		return 0
	fi
	bad "$2: the Service had no ready endpoint for ${GAP}s (at most ${HA_GAP_SECONDS}s)"
	return 1
}

# log_events FILE prints "TIME MESSAGE" for the leadership and shutdown events
# in a gateway log (JSON lines).
log_events() {
	grep -E '"msg":"(shutting down|became leader|stopped leading|stopped campaigning|lost leadership|gateway ready|labelled this pod as the leader|the leader label is absent from this pod)"' "$1" |
		sed -n 's/.*"time":"\([^"]*\)".*"msg":"\([^"]*\)".*/\1 \2/p'
}

# expect_before FILE FIRST SECOND DESCRIPTION: the first line of FILE that
# contains FIRST comes before the first one that contains SECOND.
expect_before() {
	local a b
	a=$(grep -nF -- "$2" "$1" | head -n 1 | cut -d: -f1)
	b=$(grep -nF -- "$3" "$1" | head -n 1 | cut -d: -f1)
	if [ -n "$a" ] && [ -n "$b" ] && [ "$a" -lt "$b" ]; then
		ok "$4"
		return 0
	fi
	bad "$4: '$2' (line ${a:-none}) does not come before '$3' (line ${b:-none})"
	return 1
}

# Clients ----------------------------------------------------------------------------

# open_client ID [HEARTBEAT_TIMEOUT] starts a DuckDB CLI in the client pod and
# attaches the gateway as "remote" with share level USER.
open_client() {
	session_start "$1" "${CLIENT_CMD[@]}" || return 1
	CLIENTS_OPEN="$CLIENTS_OPEN $1"
	session_send "$1" "$(client_preamble)
CREATE SECRET (TYPE quack, TOKEN '$CLIENT_TOKEN?share_level=user', SCOPE '$QUACK_URI');
ATTACH '$QUACK_URI' AS remote (DISABLE_SSL true${2:+, HEARTBEAT_TIMEOUT $2});
SELECT '$1-attached=' || x AS result FROM remote.query('SELECT ''ok'' AS x');" || return 1
	session_wait "$1" "$1-attached=ok" "$ATTACH_TIMEOUT"
}

# expect_attached ID TAG: client ID still reaches the table ha_t on its engine
# through its attachment. The query goes through remote.query(): the client
# caches the catalog of an attachment, and c2 attached before c1 created the
# table. A request in flight during a failover may fail (the client then gets
# an error, never a second execution); such a query is repeated up to twice,
# with a warning.
expect_attached() {
	local id=$1 tag=$2 attempt=1 log=$E2E_WORK/$1.log
	while :; do
		session_send "$id" "SELECT '$tag-$attempt=' || x AS result FROM remote.query('SELECT x FROM ha_t');
SELECT '$tag-done-$attempt' AS result;" || return 1
		session_wait "$id" "$tag-done-$attempt" 120 '' || return 1
		if grep -qF -- "$tag-$attempt=42" "$log"; then break; fi
		if [ "$attempt" -ge 3 ]; then
			bad "client $id cannot use its attachment any more:"
			session_output "$id" | indent
			return 1
		fi
		warn "query $attempt of client $id failed, retrying:"
		session_output "$id" | indent
		eval "ERRORS_$id=1"
		attempt=$((attempt + 1))
		sleep 3
	done
	ok "client $id still uses its attachment ($tag)"
}

expect_both_attached() {
	expect_attached c1 "$1-c1" || return 1
	expect_attached c2 "$1-c2"
}

# expect_adopted DESCRIPTION: the gateway behind the port-forward knows both
# sessions of the clients, adopted, on ENGINE_POD.
expect_adopted() {
	local list=$E2E_WORK/sessions.json
	api GET "$ADMIN_URL" "/api/v1/sessions?user=$HA_USER" "$list"
	if [ "$HTTP_STATUS" != 200 ]; then
		bad "$1: GET /api/v1/sessions answered HTTP $HTTP_STATUS"
		indent "$list"
		return 1
	fi
	expect_eq "$(json_value "$list" count)" 2 "$1: both sessions are open" || {
		indent "$list"
		return 1
	}
	expect_eq "$(json_count "$list" adopted true)" 2 "$1: both sessions are adopted" || return 1
	expect_eq "$(json_count "$list" engineId "\"$ENGINE_POD\"")" 2 "$1: both run on engine pod $ENGINE_POD" || return 1
}

close_clients() {
	local id rc
	for id in $CLIENTS_OPEN; do
		rc=0
		session_send "$id" "DETACH remote;
SELECT '$id-detached' AS result;" && session_wait "$id" "$id-detached" 60 '' || rc=1
		session_end "$id" || [ -n "$(eval "printf '%s' \"\${ERRORS_$id:-}\"")" ] || rc=1
		[ "$rc" = 0 ] || warn "client $id did not end cleanly"
	done
	CLIENTS_OPEN=''
}

# expect_follower_refuses POD: a client that connects to POD directly, not
# through the Service, gets the Quack error of a follower (HTTP 200,
# ERROR_RESPONSE); nothing runs there.
expect_follower_refuses() {
	local ip log=$E2E_WORK/cf.log
	ip=$(pod_ip "$1") || return 1
	session_start cf "${CLIENT_CMD[@]}" || return 1
	session_send cf "$(client_preamble)
CREATE SECRET (TYPE quack, TOKEN '$CLIENT_TOKEN?share_level=user', SCOPE 'quack:$ip:9494');
ATTACH 'quack:$ip:9494' AS direct (DISABLE_SSL true);
SELECT 'cf-done' AS result;" || return 1
	session_wait cf cf-done 60 '' || return 1
	session_end cf || true
	expect_output "$log" "gateway is not ready: not the leader" "a client that connects to the follower $1 directly gets 'not the leader'"
}

# Switching overlays -------------------------------------------------------------

switch_to_ha() {
	kctl -n "$NS" scale "deployment/$DEPLOY" --replicas=0 >/dev/null || return 1
	wait_for 120 "the running gateway to stop" no_gateway_pods || return 1
	SWITCHED=1
	if ! kctl apply -k "$HA_DIR" >"$E2E_WORK/apply.log" 2>&1; then
		indent "$E2E_WORK/apply.log"
		return 1
	fi
}

restore_dev() {
	step "back to the dev overlay"
	kctl -n "$NS" scale "deployment/$DEPLOY" --replicas=0 >/dev/null || return 1
	wait_for 120 "the HA gateway pods to stop" no_gateway_pods || return 1
	kctl -n "$NS" delete poddisruptionbudget "$DEPLOY" --ignore-not-found >/dev/null || return 1
	kctl apply -k "$DEV_OVERLAY_DIR" >/dev/null || return 1
	kctl -n "$NS" rollout status "deployment/$DEPLOY" --timeout=180s >/dev/null || return 1
	# kubectl apply drops what only the HA overlay applied, such as the
	# leader label in the selector of the Service: the gateway of the dev
	# overlay does not label its pod.
	if [ -n "$(kctl -n "$NS" get service "$SVC" -o go-template="{{index .spec.selector \"$LEADER_LABEL\"}}" 2>/dev/null |
		sed 's/<no value>//')" ]; then
		bad "service/$SVC still selects $LEADER_LABEL"
		return 1
	fi
	ok "deployment/$DEPLOY runs the dev overlay again"
}

# Cleanup ----------------------------------------------------------------------------

ha_diagnostics() {
	local p
	step "diagnostics (namespace $NS)"
	kctl -n "$NS" get pods -o wide >&2 || true
	kctl -n "$NS" get lease "$HA_LEASE" -o yaml 2>/dev/null | grep -E 'holderIdentity|renewTime|leaseTransitions' >&2 || true
	say "recent events:"
	kctl -n "$NS" get events --sort-by=.lastTimestamp 2>/dev/null | tail -n 25 >&2 || true
	for p in $(gateway_pods 2>/dev/null); do
		say "log of $p (last 60 lines, and of its previous container if any):"
		kctl -n "$NS" logs "$p" --previous --tail=60 >&2 2>/dev/null || true
		kctl -n "$NS" logs "$p" --tail=60 >&2 || true
	done
}

cleanup() {
	local rc=$?
	trap - EXIT
	gap_watch_kill
	if [ -n "$FROZEN_PID" ]; then
		docker exec "$FROZEN_NODE" kill -CONT "$FROZEN_PID" 2>/dev/null || true
	fi
	if [ "$rc" -ne 0 ]; then ha_diagnostics; fi
	session_kill_all
	if [ -n "$ENGINE_POD" ]; then
		kctl -n "$NS" delete pod "$ENGINE_POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	fi
	pf_stop
	kctl -n "$NS" delete pod "$CLIENT_POD" --ignore-not-found --wait=false >/dev/null 2>&1 || true
	if [ -n "$SWITCHED" ] && [ -z "${KEEP_HA:-}" ]; then
		restore_dev || rc=1
	fi
	if [ -z "${KEEP_HA:-}" ]; then rm -rf "$HA_WORK"; fi
	rm -rf "$E2E_WORK"
	exit "$rc"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

scenario_failed_hook() {
	local f
	for f in "$E2E_WORK"/c*.log; do
		if [ -f "$f" ]; then
			say "  client output ($(basename "$f")):"
			indent "$f"
		fi
	done
}

# Scenarios ----------------------------------------------------------------------------

# h1: two replicas, both ready; the leader alone carries the label and is the
# endpoint of the Service (deploy/kubernetes/README.md, "Hochverfügbarkeit").
scenario_election() {
	local readyz=$E2E_WORK/h1-readyz.txt
	if ! kctl -n "$NS" rollout status "deployment/$DEPLOY" --timeout=180s >/dev/null; then
		bad "kubectl rollout status does not report success"
		return 1
	fi
	ok "kubectl rollout status reports success"
	wait_for 60 "two ready replicas, one of them the labelled leader" one_leader || return 1
	ok "two ready replicas: $LEADER holds lease $HA_LEASE, carries $LEADER_LABEL and is the only endpoint of service/$SVC; $FOLLOWER follows"
	expect_eq "$(deployment_counts)" "2 2 2" "the Deployment shows UP-TO-DATE 2, AVAILABLE 2" || return 1
	expect_eq "$(deletion_cost "$LEADER")" 100 "the leader's pod has the deletion cost 100" || return 1
	expect_eq "$(deletion_cost "$FOLLOWER")" "" "the follower's pod has no deletion cost" || return 1
	pf_start "$FOLLOWER" || return 1
	api GET "$ADMIN_URL" /readyz "$readyz"
	expect_eq "$HTTP_STATUS" 200 "the follower passes its readiness probe" || return 1
	expect_eq "$(is_leader_metric)" 0 "the follower reports quackmux_is_leader 0" || return 1
	expect_eq "$(leader_label_metric)" 0 "the follower reports quackmux_leader_label 0" || return 1
	expect_follower_refuses "$FOLLOWER" || return 1
	expect_ge "$(metric_sum "$ADMIN_URL" quackmux_requests_total 'outcome="not_leader"')" 1 \
		"the follower counts the request as outcome not_leader" || return 1
	pf_start "$LEADER" || return 1
	expect_eq "$(is_leader_metric)" 1 "the leader reports quackmux_is_leader 1" || return 1
	expect_eq "$(leader_label_metric)" 1 "the leader reports quackmux_leader_label 1" || return 1
}

# h2: deleting the leader pod (SIGTERM) releases the lease; the waiting
# follower takes over within seconds (the pod that replaces the deleted one
# campaigns only after 3 x ha.retryPeriod) and adopts the sessions of both
# clients.
scenario_delete_leader() {
	local before=$E2E_WORK/h2.before old follower start took logpid transitions
	engine_pods >"$before" || return 1
	one_leader || {
		bad "no single leader"
		return 1
	}
	pf_start "$LEADER" || return 1
	open_client c1 || return 1
	open_client c2 5 || return 1
	session_send c1 "CREATE OR REPLACE TABLE remote.ha_t AS SELECT 42 AS x;
SELECT 'c1-created' AS result;" || return 1
	session_wait c1 c1-created 60 || return 1
	ENGINE_POD=$(new_pods "$before")
	expect_eq "$(nlines "$ENGINE_POD")" 1 "both USER sessions got one engine pod ($ENGINE_POD)" || return 1
	expect_both_attached h2-before || return 1
	expect_sessions "$ADMIN_URL" "$HA_USER" 2 "the leader lists both sessions" || return 1
	old=$LEADER follower=$FOLLOWER transitions=$(lease_transitions)
	(
		close_session_fds
		exec kubectl --context "$KUBE_CONTEXT" -n "$NS" logs -f "pod/$old" --since=5s
	) >"$E2E_WORK/h2-old.log" 2>&1 &
	logpid=$!
	sleep 2
	gap_watch_start h2
	say "  kubectl delete pod $old (the leader) at $(date -u +%H:%M:%S)"
	start=$(now)
	kctl -n "$NS" delete pod "$old" --wait=false >/dev/null || return 1
	wait_for 60 "another replica to take over" leader_other_than "$old" || return 1
	took=$(($(now) - start))
	expect_gap h2 "deleting the leader" || return 1
	kill "$logpid" 2>/dev/null || true
	wait "$logpid" 2>/dev/null || true
	kctl -n "$NS" logs "$NEW_LEADER" --since=2m >"$E2E_WORK/h2-new.log" 2>&1 || true
	say "  old leader: $(log_events "$E2E_WORK/h2-old.log" | tr '\n' ';')"
	say "  new leader: $(log_events "$E2E_WORK/h2-new.log" | tr '\n' ';')"
	if [ "$took" -gt "$HA_HANDOVER_SECONDS" ]; then
		bad "$NEW_LEADER leads only ${took}s after the delete (at most ${HA_HANDOVER_SECONDS}s): the old leader did not release the lease"
		return 1
	fi
	ok "$NEW_LEADER leads ${took}s after the delete: the old leader released the lease"
	expect_before "$E2E_WORK/h2-old.log" '"msg":"the leader label is absent from this pod"' '"msg":"stopped leading"' \
		"the old leader removed its label before it released the lease" || return 1
	expect_eq "$NEW_LEADER" "$follower" "the waiting follower took over, not the pod that replaces the deleted one" || return 1
	expect_eq "$(lease_transitions)" $((transitions + 1)) "the lease changed hands once" || return 1
	wait_for 120 "two replicas with one leader again" one_leader || return 1
	ok "the Deployment replaced $old; $LEADER leads, $FOLLOWER follows"
	expect_both_attached h2-after || return 1
	pf_start "$LEADER" || return 1
	expect_adopted "the new leader $LEADER" || return 1
}

# h3: a leader that freezes (SIGSTOP, like a stalled node or VM) cannot renew
# its lease; the follower takes over once the lease expires. When the old
# leader resumes, its sessions look expired to it (no heartbeat for longer
# than the session lease of client c2): it must step down without closing
# them, since the new leader serves them. No query runs during the freeze: a
# client may still hold a keep-alive connection to the frozen pod, on which a
# request would wait for the resume.
scenario_frozen_leader() {
	local frozen takeover node pid start restarts log=$E2E_WORK/h3-resumed.log
	one_leader || {
		bad "no single leader"
		return 1
	}
	frozen=$LEADER takeover=$FOLLOWER
	node=$(kctl -n "$NS" get pod "$frozen" -o jsonpath='{.spec.nodeName}') || return 1
	if ! pid=$(node_pid "$frozen" "$node"); then
		bad "cannot find the gateway process of $frozen on the kind node $node (docker exec $node crictl inspect)"
		return 1
	fi
	restarts=$(restart_count "$frozen")
	expect_both_attached h3-before || return 1
	say "  kill -STOP $pid on node $node (gateway $frozen) for ${HA_FREEZE_SECONDS}s"
	start=$(now)
	docker exec "$node" kill -STOP "$pid" || return 1
	FROZEN_NODE=$node FROZEN_PID=$pid
	wait_for 90 "$FOLLOWER to take over the lease" leader_is "$FOLLOWER" || return 1
	ok "$FOLLOWER leads $(($(now) - start))s after the freeze (ha.leaseDuration 15s) and is the only endpoint of service/$SVC"
	wait_for 15 "the new leader to remove $LEADER_LABEL from the frozen $frozen" unlabelled "$frozen" || return 1
	ok "the new leader removed $LEADER_LABEL from the frozen pod $frozen"
	pf_start "$FOLLOWER" || return 1
	say "  sessions the new leader has adopted during the freeze: $(session_count "$ADMIN_URL" "$HA_USER" || echo '?')"
	sleep_until $((start + HA_FREEZE_SECONDS))
	say "  kill -CONT $pid: $frozen resumes after $(($(now) - start))s"
	docker exec "$node" kill -CONT "$pid" || return 1
	FROZEN_NODE='' FROZEN_PID=''
	wait_for 60 "$frozen to step down and restart as a ready follower" restarted_follower "$frozen" "$restarts" || return 1
	kctl -n "$NS" logs "$frozen" --previous >"$log" 2>&1 || true
	expect_output "$log" "lost leadership" "the resumed gateway noticed that it lost the lease" || return 1
	if grep -F '"msg":"session closed"' "$log" | grep -qF "$HA_USER"; then
		bad "the resumed gateway closed sessions that the new leader serves:"
		grep -F '"msg":"session closed"' "$log" | indent
		return 1
	fi
	ok "the resumed gateway closed no session"
	# Give damage, if any, time to show: a DISCONNECT from the old leader would
	# end the connections on the engine.
	sleep 5
	expect_both_attached h3-after || return 1
	wait_for 60 "two replicas with one leader" one_leader || return 1
	expect_eq "$LEADER" "$takeover" "$takeover still leads: the resumed gateway left the lease alone" || return 1
	pf_start "$LEADER" || return 1
	expect_adopted "the leader $LEADER after the resume" || return 1
}

# h4: a rollout needs no help: it starts a new pod, removes the old follower
# (the leader's pod has a higher deletion cost), starts the second new pod and
# only then removes the old leader, which hands over to a new pod. The lease
# changes hands exactly once (deploy/kubernetes/README.md, "Rollout").
scenario_rollout() {
	local old_leader old_follower old_hash transitions start
	one_leader || {
		bad "no single leader"
		return 1
	}
	old_leader=$LEADER old_follower=$FOLLOWER old_hash=$(template_hash "$LEADER") transitions=$(lease_transitions)
	gap_watch_start h4
	say "  kubectl rollout restart deployment/$DEPLOY"
	start=$(now)
	kctl -n "$NS" rollout restart "deployment/$DEPLOY" >/dev/null || return 1
	if ! kctl -n "$NS" rollout status "deployment/$DEPLOY" --timeout=300s >/dev/null; then
		bad "kubectl rollout status does not report success"
		return 1
	fi
	ok "kubectl rollout status reports success after $(($(now) - start))s, without a manual failover"
	wait_for 60 "two ready replicas of the new template, one of them the leader" rollout_done || return 1
	expect_gap h4 "the rollout" || return 1
	# The old leader drains and keeps answering for a few seconds after it
	# left the Service (kubectl rollout status does not wait for that).
	wait_for 60 "the old leader $old_leader to go" pod_gone "$old_leader" || return 1
	wait_for 60 "the old follower $old_follower to go" pod_gone "$old_follower" || return 1
	if [ "$(template_hash "$LEADER")" = "$old_hash" ]; then
		bad "the leader $LEADER still runs the old template"
		return 1
	fi
	ok "$LEADER (new template) leads, $FOLLOWER follows"
	expect_eq "$(lease_transitions)" $((transitions + 1)) "the lease changed hands exactly once" || return 1
	expect_both_attached h4-after || return 1
	pf_start "$LEADER" || return 1
	expect_adopted "the leader $LEADER after the rollout" || return 1
}

# h5: kubectl drain respects the PodDisruptionBudget (minAvailable 1). Both
# replicas are ready, so the leader may be evicted: it hands over to the
# follower like a deleted pod (h2). Right after that the follower is the only
# ready replica and may not be evicted, until the replacement pod is ready.
scenario_pdb() {
	local out=$E2E_WORK/h5-evict.txt rc old_leader old_follower
	one_leader || {
		bad "no single leader"
		return 1
	}
	old_leader=$LEADER old_follower=$FOLLOWER
	gap_watch_start h5
	rc=0
	evict "$old_leader" >"$out" 2>&1 || rc=$?
	expect_status "$rc" 0 "the leader $old_leader may be evicted while the follower is ready" || {
		indent "$out"
		return 1
	}
	rc=0
	evict "$old_follower" >"$out" 2>&1 || rc=$?
	expect_status "$rc" nonzero "the eviction of $old_follower, now the last ready replica, is refused" || return 1
	expect_output "$out" "disruption budget" "the PodDisruptionBudget refused it" || {
		indent "$out"
		return 1
	}
	wait_for 60 "$old_follower to take over" leader_is "$old_follower" || return 1
	expect_gap h5 "evicting the leader" || return 1
	wait_for 120 "a ready replacement next to the new leader" one_leader || return 1
	ok "$LEADER leads, the replacement $FOLLOWER is ready"
	rc=0
	evict "$FOLLOWER" >"$out" 2>&1 || rc=$?
	expect_status "$rc" 0 "the follower $FOLLOWER may be evicted" || {
		indent "$out"
		return 1
	}
	wait_for 120 "a new follower" one_leader || return 1
	expect_both_attached h5-after || return 1
	pf_start "$LEADER" || return 1
	expect_adopted "the leader $LEADER after the evictions" || return 1
}

# Main -------------------------------------------------------------------------------

step "prerequisites"
kctl get namespace "$NS" >/dev/null 2>&1 || die "namespace $NS not found with context $KUBE_CONTEXT: run setup.sh first"
kctl -n "$NS" get deployment "$DEPLOY" >/dev/null 2>&1 || die "deployment/$DEPLOY not found in $NS: run setup.sh first"
make_overlay
OVERLAY_DIR=$HA_DIR OVERLAY_RENDERED=''
render_overlay
case $OVERLAY_RENDERED in
*"kind: PodDisruptionBudget"*) ;;
*) die "$HA_OVERLAY does not look like overlays/ha (no PodDisruptionBudget)" ;;
esac
# The HA configuration names the published engine image; the cluster runs the
# locally built one (setup.sh) under that name (imagePullPolicy IfNotPresent).
HA_ENGINE_IMAGE=$(printf '%s\n' "$OVERLAY_RENDERED" | awk '$1 == "image:" && $2 ~ /duckdb-engine/ { print $2; exit }')
if [ -n "$HA_ENGINE_IMAGE" ] && [ "$HA_ENGINE_IMAGE" != "$ENGINE_IMAGE" ]; then
	docker image inspect "$ENGINE_IMAGE" >/dev/null 2>&1 || die "engine image $ENGINE_IMAGE not found locally: run setup.sh first"
	docker tag "$ENGINE_IMAGE" "$HA_ENGINE_IMAGE"
	kind load docker-image --name "$KIND_CLUSTER" "$HA_ENGINE_IMAGE" >/dev/null || die "kind load docker-image $HA_ENGINE_IMAGE failed"
	ok "the cluster runs the local $ENGINE_IMAGE as $HA_ENGINE_IMAGE (the engine image of the HA configuration)"
fi
SVC=${QMX_SERVICE:-quackmux}
SVC_PORT=${QMX_SERVICE_PORT:-9494}
QUACK_URI=quack:$SVC:$SVC_PORT
ok "overlay $HA_DIR: $HA_OVERLAY with generated secrets and $GATEWAY_IMAGE, user $HA_USER"

step "switch from the running gateway to the HA overlay"
switch_to_ha || die "cannot switch to the HA overlay"
ok "scaled deployment/$DEPLOY to 0 and applied the HA overlay (deploy/kubernetes/README.md)"
ensure_client_pod || die "the client pod $CLIENT_POD did not start (image $ENGINE_IMAGE)"

run_scenario h1 "two ready replicas: the labelled leader is the only endpoint" scenario_election
run_scenario h2 "deleting the leader hands over within seconds; attachments survive" scenario_delete_leader
if [ -n "$ENGINE_POD" ]; then
	run_scenario h3 "a frozen leader loses the lease and steps down when it resumes" scenario_frozen_leader
	run_scenario h4 "a rollout completes on its own with one handover" scenario_rollout
	run_scenario h5 "the PodDisruptionBudget lets the leader go while the follower is ready" scenario_pdb
else
	bad "h3-h5 need the attachments of h2"
	SCENARIOS_FAILED="$SCENARIOS_FAILED h3 h4 h5"
fi
close_clients

summary "kind HA e2e" || exit 1
