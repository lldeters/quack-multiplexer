#!/usr/bin/env bash
#
# The Helm chart deploy/helm/quackmux in the kind cluster prepared by
# setup.sh, with real DuckDB 2.0 clients (see ../README.md). It covers what
# "helm template" cannot: the chart's lookups of the cluster (generated
# secrets that upgrades keep, the refusal of a second gateway and of an
# early switch to HA) and Helm's handling of the objects it keeps.
#
#   bash test/e2e/kind/helm.sh
#
# The release "quackmux" goes into its own namespace (HELM_NAMESPACE), next
# to the dev overlay of setup.sh, which stays untouched. The scenarios run in
# this order, each on the state the previous one left:
#
#   i  helm install with the configuration of overlays/dev: the rollout
#      succeeds, a client attaches through the Service, its engine runs in
#      the release namespace
#   o  a second release in the namespace is refused, and so is a release in
#      the namespace of the dev overlay (kubectl apply -k)
#   u  helm upgrade with a changed configuration rolls the gateway out with a
#      new ConfigMap and keeps the earlier one, keeps the session key and the
#      admin token, and the attached client keeps its session (adopted)
#   g  the switch to ha.enabled is refused while the gateway runs, and after
#      scaling it to 0 while a gateway pod is still there
#   h  after the pod is gone, helm upgrade to ha.enabled (the chart's HA
#      configuration, a static user): rollout status succeeds, two ready
#      replicas, the leader alone is the endpoint of the Service; the user
#      attaches; an upgrade with a changed configuration hands over, the
#      session survives, the secrets stay, the generated admin token works
#   c  an HA upgrade with a configuration the gateway refuses stalls: the
#      new pod fails, the old ones keep serving, and an old pod that is
#      replaced comes back with its own configuration (its ConfigMap), not
#      with the refused one; helm rollback ends the stall
#   r  helm rollback to the first HA revision brings back its ConfigMap,
#      which Helm kept, and the session survives
#   d  uninstall as the chart's README says: no engine is left, the session
#      key, the admin token and the ConfigMaps stay; a new install takes the
#      secrets over with their values; the README's cleanup leaves only the
#      ConfigMap in use
#
# Optional environment (see env.sh for more):
#   HELM               helm binary, Helm 3 or 4 (default helm)
#   HELM_NAMESPACE     namespace of the release (default quackmux-helm); the
#                      script deletes it before and after the run
#   ADMIN_LOCAL_PORT   local port of the admin port-forward (default 19697)
#   ATTACH_TIMEOUT     seconds an ATTACH may take (default 180)
#   KEEP_HELM=1        keep the release and its namespace after the run
set -euo pipefail
# shellcheck source=env.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/env.sh"

HELM=${HELM:-helm}
need "$HELM" kubectl curl mkfifo awk sed grep sha256sum base64 od tr wc
# Writing to a session whose client died must fail the check, not kill the run.
trap '' PIPE

# The dev overlay names its namespace and the images that setup.sh loaded.
resolve_names
resolve_images
DEV_NS=$NS
NS=${HELM_NAMESPACE:-quackmux-helm}
DEPLOY=quackmux
CHART=$REPO_ROOT/deploy/helm/quackmux
GATEWAY_SELECTOR=app.kubernetes.io/name=quackmux,app.kubernetes.io/component=gateway
RUN_ID=${RUN_ID:-$(date +%s)}
ADMIN_LOCAL_PORT=${ADMIN_LOCAL_PORT:-19697}
ADMIN_URL=http://127.0.0.1:$ADMIN_LOCAL_PORT
ATTACH_TIMEOUT=${ATTACH_TIMEOUT:-180}
CLIENT_CMD=(kubectl --context "$KUBE_CONTEXT" -n "$NS" exec -i "$CLIENT_POD" -- /usr/local/bin/duckdb)
QUACK_URI=quack:quackmux:9494
H=("$HELM" --kube-context "$KUBE_CONTEXT" -n "$NS")

# The release with the configuration of overlays/dev (auth.mode none, so the
# user comes from the token option) and the images of setup.sh.
IMAGE_ARGS=(--set "image.repository=${GATEWAY_IMAGE%:*}" --set "image.tag=${GATEWAY_IMAGE##*:}")
DEV_ARGS=(-f "$KIND_DIR/helm-values-dev.yaml" "${IMAGE_ARGS[@]}"
	--set-file "configYaml=$REPO_ROOT/deploy/kubernetes/overlays/dev/config.yaml"
	--set "config.provider.kubernetes.namespace=$NS"
	--set "config.profiles.default.image=$ENGINE_IMAGE"
	--set "config.profiles.persistent.image=$ENGINE_IMAGE")
# ha.enabled with the chart's HA configuration and a static user.
HA_TOKEN=helm-e2e-$RUN_ID-$(od -An -N16 -tx1 /dev/urandom | tr -d ' \n')
HA_USER=e2e-helm-alice-$RUN_ID
HA_ARGS=(-f "$KIND_DIR/helm-values-ha.yaml" "${IMAGE_ARGS[@]}"
	--set "config.profiles.default.image=$ENGINE_IMAGE"
	--set "users[0].name=$HA_USER"
	--set "users[0].tokens[0].sha256=$(printf '%s' "$HA_TOKEN" | sha256sum | awk '{ print $1 }')")

E2E_WORK=$(make_workdir)
ADMIN_HEADER_FILE=''
PF_PID=''
LEADER=''
SECRETS_BEFORE=''
HA_REVISION='' HA_CONFIGMAP=''
ENGINE_I='' ENGINE_H=''

pf_stop() {
	if [ -n "$PF_PID" ]; then
		kill "$PF_PID" 2>/dev/null || true
		wait "$PF_PID" 2>/dev/null || true
		PF_PID=''
	fi
}

# pf_start [TARGET] forwards ADMIN_LOCAL_PORT to the admin port of TARGET
# (default: the gateway Deployment).
pf_start() {
	pf_stop
	(
		close_session_fds
		exec kubectl --context "$KUBE_CONTEXT" -n "$NS" port-forward "${1:-deployment/$DEPLOY}" "$ADMIN_LOCAL_PORT:9495"
	) >"$E2E_WORK/pf.log" 2>&1 &
	PF_PID=$!
	wait_for 30 "the admin API through kubectl port-forward" curl -fsS "$ADMIN_URL/readyz"
}

drop_namespace() {
	"${H[@]}" uninstall quackmux --wait >/dev/null 2>&1 || true
	kctl delete namespace "$NS" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
}

cleanup() {
	local rc=$?
	trap - EXIT
	session_kill_all
	pf_stop
	if [ -n "${KEEP_HELM:-}" ]; then
		say "KEEP_HELM is set: the release stays in namespace $NS"
	else
		drop_namespace
	fi
	rm -rf "$E2E_WORK"
	exit "$rc"
}
trap cleanup EXIT

scenario_failed_hook() {
	diagnostics
	say "helm history:"
	"${H[@]}" history quackmux >&2 || true
}

# helm_run LOG ARG... runs helm with ARG and shows its output on failure.
helm_run() {
	local log=$E2E_WORK/$1
	shift
	if ! "${H[@]}" "$@" >"$log" 2>&1; then
		bad "helm $*"
		indent "$log"
		return 1
	fi
}

# helm_refused PATTERN ARG... succeeds if helm ARG fails with PATTERN in its
# output.
helm_refused() {
	local pattern=$1 out
	shift
	if out=$("${H[@]}" "$@" 2>&1); then
		bad "helm $1 succeeded, want a refusal with '$pattern'"
		return 1
	fi
	case $out in
	*"$pattern"*) ok "refused: $(printf '%s\n' "$out" | grep -F -- "$pattern" | head -n 1 | sed 's/^Error: //' | cut -c1-200)" ;;
	*)
		bad "helm $1 failed without '$pattern':"
		printf '%s\n' "$out" | indent
		return 1
		;;
	esac
}

rollout() {
	local out
	out=$(kctl -n "$NS" rollout status "deployment/$DEPLOY" --timeout=180s) || return 1
	ok "kubectl rollout status: $(printf '%s\n' "$out" | tail -n 1)"
}

# revision prints the current revision of the release.
revision() { "${H[@]}" status quackmux -o yaml | awk '/^version:/ { print $2; exit }'; }

# secret_digest prints a digest of the data of the generated Secrets.
secret_digest() {
	local s
	for s in quackmux-session-key quackmux-admin-token; do
		kctl -n "$NS" get secret "$s" -o jsonpath='{.data}' || return 1
	done | sha256sum | awk '{ print $1 }'
}

# config_map prints the ConfigMap that the gateway's pod template mounts.
config_map() {
	kctl -n "$NS" get deployment "$DEPLOY" -o jsonpath='{.spec.template.spec.volumes[?(@.name=="config")].configMap.name}'
}

config_maps() { kctl -n "$NS" get configmap -l "app.kubernetes.io/name=quackmux,app.kubernetes.io/instance=quackmux" -o name; }

# leader_is_endpoint: two ready replicas, exactly one with the leader label,
# and it is the only ready endpoint of the Service. Sets LEADER.
leader_is_endpoint() {
	local ready leaders leader_ip ips
	ready=$(kctl -n "$NS" get pods -l "$GATEWAY_SELECTOR" \
		-o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{range .status.conditions}}{{if and (eq .type "Ready") (eq .status "True")}}x{{end}}{{end}}{{end}}{{end}}')
	[ "${#ready}" = 2 ] || return 1
	leaders=$(kctl -n "$NS" get pods -l "$GATEWAY_SELECTOR,quackmux.io/leader=true" \
		-o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.podIP}{"\n"}{end}')
	[ "$(nlines "$leaders")" = 1 ] || return 1
	leader_ip=$(printf '%s\n' "$leaders" | awk '{ print $2 }')
	ips=$(kctl -n "$NS" get endpointslices -l kubernetes.io/service-name=quackmux \
		-o go-template='{{range .items}}{{range .endpoints}}{{if .conditions.ready}}{{index .addresses 0}}{{"\n"}}{{end}}{{end}}{{end}}')
	[ "$ips" = "$leader_ip" ] || return 1
	LEADER=$(printf '%s\n' "$leaders" | awk '{ print $1 }')
}

expect_leader_endpoint() {
	if ! wait_for 90 "two ready replicas, one leader, the leader the only endpoint" leader_is_endpoint; then
		kctl -n "$NS" get pods -L quackmux.io/leader -o wide >&2 || true
		return 1
	fi
	ok "two ready replicas; leader $LEADER is the only endpoint of the Service"
}

# open_session ID TOKEN attaches a client through the Service. Right after
# helm install the Service's address may not work yet (kube-proxy has not
# programmed it): an ATTACH that cannot connect is retried.
open_session() {
	local id=$1 attempt=1
	session_start "$id" "${CLIENT_CMD[@]}"
	session_send "$id" "$(client_preamble)
CREATE SECRET (TYPE quack, TOKEN '$2', SCOPE '$QUACK_URI');
SELECT '$id-secret' AS result;" || return 1
	session_wait "$id" "$id-secret" 60 || return 1
	while :; do
		session_send "$id" "ATTACH '$QUACK_URI' AS remote (DISABLE_SSL true);
SELECT '$id-attached=' || x AS result FROM remote.query('SELECT ''ok'' AS x');
SELECT '$id-attach-$attempt' AS result;" || return 1
		session_wait "$id" "$id-attach-$attempt" "$ATTACH_TIMEOUT" '' || return 1
		if session_output "$id" | grep -qF -- "$id-attached=ok"; then
			return 0
		fi
		if [ "$attempt" -ge 5 ] || ! session_output "$id" | grep -qF -- "Could not connect to server"; then
			bad "session $id did not attach:"
			session_output "$id" | indent
			return 1
		fi
		warn "session $id: the Service did not answer yet (attempt $attempt); retrying"
		attempt=$((attempt + 1))
		sleep 2
	done
}

# close_session ID detaches and ends the client.
close_session() {
	session_send "$1" "DETACH remote;
SELECT '$1-detached' AS result;" || return 1
	session_wait "$1" "$1-detached" 60 || return 1
	session_end "$1" || true
}

# query_retry ID TAG runs a query on the attachment of session ID. A query
# during a gateway restart may fail (never run twice); it is retried twice.
query_retry() {
	local id=$1 tag=$2 attempt=1
	while :; do
		session_send "$id" "SELECT '$tag=' || x AS result FROM remote.query('SELECT ''ok'' AS x');
SELECT '$id-$tag-$attempt' AS result;" || return 1
		session_wait "$id" "$id-$tag-$attempt" 120 '' || return 1
		if session_output "$id" | grep -qF -- "$tag=ok"; then
			ok "session $id: query $tag"
			return 0
		fi
		if [ "$attempt" -ge 3 ]; then
			bad "session $id: query $tag failed three times"
			indent "$E2E_WORK/$id.log"
			return 1
		fi
		warn "session $id: query $tag failed (attempt $attempt); retrying"
		attempt=$((attempt + 1))
		sleep 3
	done
}

# engine_of USER prints the engine pod of USER.
engine_of() {
	kctl -n "$NS" get pods -l app.kubernetes.io/component=engine \
		-o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.annotations.quackmux\.io/user}{"\n"}{end}' |
		awk -v u="$1" '$2 == u { print $1; exit }'
}

# delete_engine NAME ends an engine through the admin API (port-forward).
delete_engine() {
	api DELETE "$ADMIN_URL" "/api/v1/engines/$1" "$E2E_WORK/delete.json"
	expect_eq "$HTTP_STATUS" 202 "DELETE /api/v1/engines/$1"
}

# lingering_gateway_pod NAME starts a pod with the labels of a gateway pod: a
# gateway pod that has not ended yet, as far as the chart can tell.
lingering_gateway_pod() {
	kctl -n "$NS" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: $1
  labels:
    app.kubernetes.io/name: quackmux
    app.kubernetes.io/component: gateway
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
    - name: idle
      image: $ENGINE_IMAGE
      imagePullPolicy: IfNotPresent
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
          cpu: 10m
          memory: 32Mi
        limits:
          memory: 256Mi
      volumeMounts:
        - name: tmp
          mountPath: /tmp
  volumes:
    - name: tmp
      emptyDir: {}
EOF
}

stop_gateway() {
	kctl -n "$NS" scale "deployment/$DEPLOY" --replicas=0 >/dev/null || return 1
	if ! kctl -n "$NS" wait --for=delete pod -l "$GATEWAY_SELECTOR" --timeout=120s >"$E2E_WORK/wait.log" 2>&1; then
		indent "$E2E_WORK/wait.log"
		return 1
	fi
}

step "prerequisites"
say "helm:      $("$HELM" version --short)"
say "release:   quackmux in namespace $NS (chart $CHART)"
say "images:    gateway $GATEWAY_IMAGE, engines $ENGINE_IMAGE"
drop_namespace
# As the chart's README asks: the namespace with Pod Security "restricted".
kctl create namespace "$NS" >/dev/null
kctl label namespace "$NS" pod-security.kubernetes.io/enforce=restricted \
	pod-security.kubernetes.io/warn=restricted pod-security.kubernetes.io/audit=restricted >/dev/null
ensure_client_pod
ok "namespace $NS, client pod $CLIENT_POD"

# i -----------------------------------------------------------------------------
I_USER=e2e-helm-dev-$RUN_ID
scenario_install() {
	local cm len
	helm_run install.log install quackmux "$CHART" "${DEV_ARGS[@]}" || return 1
	rollout || return 1
	cm=$(config_map)
	case $cm in
	quackmux-config-[0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f][0-9a-f]) ok "the gateway mounts ConfigMap $cm" ;;
	*)
		bad "ConfigMap '$cm': want quackmux-config-<10 hex digits>"
		return 1
		;;
	esac
	len=$(kctl -n "$NS" get secret quackmux-session-key -o jsonpath='{.data.key}' | base64 -d | wc -c | tr -d ' ')
	expect_eq "$len" 43 "the chart generated a session key of 43 characters" || return 1
	SECRETS_BEFORE=$(secret_digest) || return 1
	open_session i1 "e2e?user=$I_USER&share_level=user" || return 1
	ENGINE_I=$(engine_of "$I_USER")
	if [ -z "$ENGINE_I" ]; then
		bad "no engine pod of $I_USER in namespace $NS"
		return 1
	fi
	ok "$I_USER attached through the Service; engine $ENGINE_I runs in namespace $NS"
}

# o -----------------------------------------------------------------------------
scenario_one_gateway() {
	helm_refused "of the Helm release quackmux" install other "$CHART" "${DEV_ARGS[@]}" || return 1
	if "${H[@]}" status other >/dev/null 2>&1; then
		bad "the refused release other exists"
		return 1
	fi
	expect_eq "$(kctl -n "$NS" get deployments -o name | tr '\n' ' ')" "deployment.apps/$DEPLOY " "the namespace has the one gateway Deployment" || return 1
	if kctl -n "$DEV_NS" get deployment "$DEPLOY" >/dev/null 2>&1; then
		# A dry run against the cluster renders with lookups and changes
		# nothing.
		helm_refused "not installed with Helm" install quackmux "$CHART" --namespace "$DEV_NS" --dry-run=server \
			"${DEV_ARGS[@]}" --set "config.provider.kubernetes.namespace=$DEV_NS" || return 1
	else
		warn "no deployment/$DEPLOY in namespace $DEV_NS (setup.sh): the check against kubectl apply -k is skipped"
	fi
}

# u -----------------------------------------------------------------------------
scenario_upgrade() {
	local cm_before cm_after pods_before pods_after
	cm_before=$(config_map)
	pods_before=$(gateway_pods | tr '\n' ' ')
	say "  helm upgrade with config.limits.maxEngines=7 (pods: $pods_before)"
	helm_run upgrade.log upgrade quackmux "$CHART" "${DEV_ARGS[@]}" --set config.limits.maxEngines=7 || return 1
	rollout || return 1
	pods_after=$(gateway_pods | tr '\n' ' ')
	if [ "$pods_after" = "$pods_before" ]; then
		bad "the gateway pods did not change: $pods_after"
		return 1
	fi
	ok "the changed configuration rolled the gateway out ($pods_before-> $pods_after)"
	cm_after=$(config_map)
	if [ "$cm_after" = "$cm_before" ]; then
		bad "the changed configuration kept ConfigMap $cm_before"
		return 1
	fi
	kctl -n "$NS" get configmap "$cm_after" -o go-template='{{index .data "config.yaml"}}' | grep -q 'maxEngines: 7' || {
		bad "ConfigMap $cm_after lacks maxEngines: 7"
		return 1
	}
	ok "the gateway mounts the new ConfigMap $cm_after"
	kctl -n "$NS" get configmap "$cm_before" >/dev/null || {
		bad "Helm deleted the earlier ConfigMap $cm_before"
		return 1
	}
	ok "Helm kept the earlier ConfigMap $cm_before"
	expect_eq "$(secret_digest)" "$SECRETS_BEFORE" "helm upgrade kept the session key and the admin token" || return 1
	query_retry i1 after-upgrade || return 1
	pf_start || return 1
	api GET "$ADMIN_URL" "/api/v1/sessions?user=$I_USER" "$E2E_WORK/sessions.json"
	expect_eq "$(json_value "$E2E_WORK/sessions.json" adopted)" true "the new gateway adopted the session (same session key)" || return 1
	close_session i1 || return 1
	delete_engine "$ENGINE_I" || return 1
	pf_stop
}

# g -----------------------------------------------------------------------------
scenario_guard() {
	helm_refused "runs without leader election" upgrade quackmux "$CHART" "${HA_ARGS[@]}" || return 1
	say "  kubectl scale deployment/$DEPLOY --replicas=0; a gateway pod that has not ended yet"
	stop_gateway || return 1
	lingering_gateway_pod quackmux-e2e-lingering || return 1
	helm_refused "have not ended yet" upgrade quackmux "$CHART" "${HA_ARGS[@]}" || return 1
	kctl -n "$NS" delete pod quackmux-e2e-lingering --wait=true >/dev/null || return 1
	expect_eq "$(kctl -n "$NS" get deployment "$DEPLOY" -o jsonpath='{.spec.strategy.type} {.spec.replicas}')" "Recreate 0" \
		"the release is unchanged (Recreate, scaled to 0)"
}

# h -----------------------------------------------------------------------------
scenario_ha() {
	local pods out
	# g left the gateway at 0 replicas without pods.
	helm_run upgrade-ha.log upgrade quackmux "$CHART" "${HA_ARGS[@]}" || return 1
	ok "helm upgrade to ha.enabled after the gateway pod is gone"
	rollout || return 1
	expect_eq "$(kctl -n "$NS" get deployment "$DEPLOY" -o jsonpath='{.spec.replicas} {.spec.strategy.type} {.spec.strategy.rollingUpdate.maxSurge} {.spec.strategy.rollingUpdate.maxUnavailable}')" \
		"2 RollingUpdate 1 0" "two replicas, RollingUpdate with maxSurge 1 and maxUnavailable 0" || return 1
	expect_eq "$(kctl -n "$NS" get service quackmux -o jsonpath='{.spec.selector.quackmux\.io/leader}')" true \
		"the Service selects the leader label" || return 1
	expect_eq "$(kctl -n "$NS" get pdb quackmux -o jsonpath='{.spec.minAvailable} {.spec.unhealthyPodEvictionPolicy}')" "1 AlwaysAllow" \
		"PodDisruptionBudget minAvailable 1, AlwaysAllow" || return 1
	expect_leader_endpoint || return 1
	expect_eq "$(secret_digest)" "$SECRETS_BEFORE" "the switch to HA kept the session key and the admin token" || return 1
	HA_REVISION=$(revision)
	HA_CONFIGMAP=$(config_map)

	open_session h1 "$HA_TOKEN?share_level=user" || return 1
	ENGINE_H=$(engine_of "$HA_USER")
	if [ -z "$ENGINE_H" ]; then
		bad "no engine pod of $HA_USER"
		return 1
	fi
	ok "$HA_USER (a static token of the values users) attached; engine $ENGINE_H"

	pods=$(gateway_pods | tr '\n' ' ')
	say "  helm upgrade with config.limits.maxEngines=9 (HA rollout, pods: $pods)"
	helm_run upgrade-ha2.log upgrade quackmux "$CHART" "${HA_ARGS[@]}" --set config.limits.maxEngines=9 || return 1
	rollout || return 1
	local p
	for p in $pods; do
		wait_for 120 "old gateway pod $p to end" pod_gone "$p" || return 1
	done
	expect_leader_endpoint || return 1
	query_retry h1 after-ha-upgrade || return 1
	expect_eq "$(secret_digest)" "$SECRETS_BEFORE" "the HA upgrade kept the session key and the admin token" || return 1

	ADMIN_HEADER_FILE=$E2E_WORK/admin.header
	out=$(kctl -n "$NS" get secret quackmux-admin-token -o jsonpath='{.data.token}' | base64 -d)
	(umask 077 && printf 'Authorization: Bearer %s\n' "$out" >"$ADMIN_HEADER_FILE")
	pf_start "pod/$LEADER" || return 1
	api GET "$ADMIN_URL" "/api/v1/sessions?user=$HA_USER" "$E2E_WORK/sessions.json"
	expect_eq "$HTTP_STATUS" 200 "the leader's admin API accepts the generated admin token" || return 1
	pf_stop
}

# c -----------------------------------------------------------------------------
# template_hash POD prints the ReplicaSet (pod-template-hash) of POD.
template_hash() { kctl -n "$NS" get pod "$1" -o go-template='{{index .metadata.labels "pod-template-hash"}}'; }

# pods_of HASH prints the pods of the ReplicaSet HASH that are not
# terminating.
pods_of() {
	kctl -n "$NS" get pods -l "$GATEWAY_SELECTOR,pod-template-hash=$1" \
		-o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{.metadata.name}}{{"\n"}}{{end}}{{end}}'
}

# restarted POD succeeds once the gateway container of POD has restarted.
restarted() {
	local n
	n=$(kctl -n "$NS" get pod "$1" -o jsonpath='{.status.containerStatuses[?(@.name=="gateway")].restartCount}')
	[ "${n:-0}" -ge 1 ]
}

# refused_field POD succeeds once the log of the previous start of POD's
# gateway names the field the gateway refused.
refused_field() {
	kctl -n "$NS" logs "$1" -c gateway --previous 2>/dev/null | grep -qF 'unknown field "maxEngine"'
}

# ready_pods_of HASH COUNT succeeds once COUNT pods of the ReplicaSet HASH
# are ready.
ready_pods_of() {
	local ready
	ready=$(kctl -n "$NS" get pods -l "$GATEWAY_SELECTOR,pod-template-hash=$1" \
		-o go-template='{{range .items}}{{if not .metadata.deletionTimestamp}}{{range .status.conditions}}{{if and (eq .type "Ready") (eq .status "True")}}x{{end}}{{end}}{{end}}{{end}}')
	[ "${#ready}" = "$2" ]
}

scenario_refused_config() {
	local good hash old follower='' new='' p
	good=$(revision)
	leader_is_endpoint || {
		bad "no leader before the upgrade"
		return 1
	}
	hash=$(template_hash "$LEADER")
	old=$(pods_of "$hash" | tr '\n' ' ')
	# A typo the chart cannot see: the gateway refuses the unknown field.
	say "  helm upgrade with config.limits.maxEngine=80 (a field the gateway refuses; pods: $old)"
	helm_run upgrade-bad.log upgrade quackmux "$CHART" "${HA_ARGS[@]}" --set config.limits.maxEngines=9 --set config.limits.maxEngine=80 || return 1
	for p in $(gateway_pods); do
		[ "$(template_hash "$p")" = "$hash" ] || new=$p
	done
	if [ -z "$new" ]; then
		bad "no pod of the new ReplicaSet"
		return 1
	fi
	wait_for 120 "the new pod $new to fail" restarted "$new" || return 1
	# The log of the failed start may take a moment to become readable.
	if wait_for 60 "the log of $new to name the field maxEngine" refused_field "$new"; then
		ok "the new pod $new refuses the configuration (unknown field \"maxEngine\")"
	else
		return 1
	fi
	expect_leader_endpoint || return 1
	# A drain, an eviction or an OOM kill replaces an old pod.
	for p in $old; do
		[ "$p" = "$LEADER" ] || follower=$p
	done
	if [ -z "$follower" ]; then
		bad "no old follower among $old"
		return 1
	fi
	say "  kubectl delete pod $follower (the old follower)"
	kctl -n "$NS" delete pod "$follower" --wait=true >/dev/null || return 1
	if ! wait_for 120 "two ready pods of the old ReplicaSet $hash again" ready_pods_of "$hash" 2; then
		kctl -n "$NS" get pods -l "$GATEWAY_SELECTOR" -o wide >&2 || true
		for p in $(pods_of "$hash"); do
			kctl -n "$NS" logs "$p" --tail=5 >&2 || true
		done
		return 1
	fi
	ok "the replacement of $follower started with the configuration of its ReplicaSet and is ready"
	expect_leader_endpoint || return 1
	query_retry h1 during-stall || return 1
	say "  helm rollback quackmux $good"
	helm_run rollback-bad.log rollback quackmux "$good" --wait || return 1
	rollout || return 1
	wait_for 120 "the failed pod $new to go" pod_gone "$new" || return 1
	expect_leader_endpoint
}

# r -----------------------------------------------------------------------------
scenario_rollback() {
	[ -n "$HA_REVISION" ] || {
		bad "h recorded no revision"
		return 1
	}
	say "  helm rollback quackmux $HA_REVISION (ConfigMap $HA_CONFIGMAP, kept by Helm)"
	helm_run rollback.log rollback quackmux "$HA_REVISION" --wait || return 1
	rollout || return 1
	expect_eq "$(config_map)" "$HA_CONFIGMAP" "the gateway mounts the ConfigMap of revision $HA_REVISION again" || return 1
	expect_leader_endpoint || return 1
	query_retry h1 after-rollback || return 1
	close_session h1 || return 1
	pf_start "pod/$LEADER" || return 1
	delete_engine "$ENGINE_H" || return 1
	pf_stop
}

# d -----------------------------------------------------------------------------
scenario_uninstall() {
	local n current
	# The README: stop the gateway, delete the engines, then uninstall.
	stop_gateway || return 1
	kctl -n "$NS" delete pod -l app.kubernetes.io/component=engine,app.kubernetes.io/managed-by=quackmux --wait=true >/dev/null || return 1
	helm_run uninstall.log uninstall quackmux --wait || return 1
	expect_eq "$(kctl -n "$NS" get deployment,networkpolicy,service,pdb -o name | tr '\n' ' ')" "" \
		"helm uninstall removed the Deployment, the NetworkPolicies, the Service and the PodDisruptionBudget" || return 1
	expect_eq "$(kctl -n "$NS" get pods -l app.kubernetes.io/component=engine -o name)" "" "no engine pod is left" || return 1
	expect_eq "$(secret_digest)" "$SECRETS_BEFORE" "Helm kept the session key and the admin token" || return 1
	expect_eq "$(kctl -n "$NS" get secret quackmux-users -o name 2>/dev/null)" "secret/quackmux-users" \
		"Helm kept the users Secret" || return 1
	n=$(nlines "$(config_maps)")
	expect_ge "$n" 4 "Helm kept the ConfigMaps (dev, dev changed, HA, HA changed)" || return 1

	helm_run reinstall.log install quackmux "$CHART" "${DEV_ARGS[@]}" || return 1
	rollout || return 1
	expect_eq "$(secret_digest)" "$SECRETS_BEFORE" "a new release of the same name took the session key and the admin token over" || return 1

	# The README's cleanup: every ConfigMap but the one in use.
	current=$(config_map)
	kctl -n "$NS" delete configmap -l app.kubernetes.io/name=quackmux,app.kubernetes.io/instance=quackmux \
		--field-selector "metadata.name!=$current" >/dev/null || return 1
	expect_eq "$(config_maps)" "configmap/$current" "the README's cleanup leaves the ConfigMap in use" || return 1
	kctl -n "$NS" rollout restart "deployment/$DEPLOY" >/dev/null || return 1
	rollout || return 1
}

run_scenario i "helm install: rollout, a client attaches, the engine runs in the release namespace" scenario_install
run_scenario o "one gateway per namespace" scenario_one_gateway
run_scenario u "helm upgrade: new ConfigMap, the secrets and the session stay" scenario_upgrade
run_scenario g "the switch to ha.enabled waits until the gateway without leader election is gone" scenario_guard
run_scenario h "ha.enabled: rollout status, the leader alone is the endpoint, an upgrade hands over" scenario_ha
run_scenario c "ha.enabled: a refused configuration stalls the rollout, old pods keep theirs" scenario_refused_config
run_scenario r "helm rollback brings back a kept ConfigMap" scenario_rollback
run_scenario d "uninstall as the README says, install again" scenario_uninstall
summary "helm e2e" || exit 1
