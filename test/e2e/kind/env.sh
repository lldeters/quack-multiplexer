# shellcheck shell=bash
#
# Settings and helpers shared by setup.sh, test.sh, ha.sh and teardown.sh (sourced,
# not executed). Every setting can be overridden through the environment:
#
#   KIND_CLUSTER     kind cluster name (default quackmux)
#   KUBE_CONTEXT     kubectl context (default kind-$KIND_CLUSTER); the scripts
#                    never touch the current context
#   OVERLAY_DIR      Kustomize overlay (default deploy/kubernetes/overlays/dev)
#   ENGINE_IMAGE     engine image (default: the duckdb-engine image the overlay
#                    names, else quackmux/duckdb-engine:2.0.0)
#   GATEWAY_IMAGE    gateway image (default: the image of the overlay's
#                    Deployment, else quackmux/quackmux:dev)
#   QMX_NAMESPACE    namespace of the gateway (default: from the overlay)
#   QMX_DEPLOYMENT   gateway Deployment (default: the overlay's Deployment)

KIND_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
E2E_ROOT=$(cd "$KIND_DIR/.." && pwd)
REPO_ROOT=$(cd "$E2E_ROOT/../.." && pwd)
# shellcheck source=../lib/common.sh
. "$E2E_ROOT/lib/common.sh"

KIND_CLUSTER=${KIND_CLUSTER:-quackmux}
KUBE_CONTEXT=${KUBE_CONTEXT:-kind-$KIND_CLUSTER}
OVERLAY_DIR=${OVERLAY_DIR:-$REPO_ROOT/deploy/kubernetes/overlays/dev}
DEFAULT_ENGINE_IMAGE=quackmux/duckdb-engine:2.0.0
DEFAULT_GATEWAY_IMAGE=quackmux/quackmux:dev

kctl() { kubectl --context "$KUBE_CONTEXT" "$@"; }

# render_overlay renders the overlay once into OVERLAY_RENDERED. It needs no
# cluster.
OVERLAY_RENDERED=''
render_overlay() {
	[ -n "$OVERLAY_RENDERED" ] && return 0
	[ -d "$OVERLAY_DIR" ] || die "overlay $OVERLAY_DIR not found (set OVERLAY_DIR)"
	OVERLAY_RENDERED=$(kubectl kustomize "$OVERLAY_DIR") || die "kubectl kustomize $OVERLAY_DIR failed"
}

# overlay_objects prints "KIND NAMESPACE NAME" for every object of the rendered
# overlay ("-" for a missing namespace). kustomize prints one field per line
# with two-space indentation, so the metadata fields are easy to find.
overlay_objects() {
	printf '%s\n' "$OVERLAY_RENDERED" | awk '
		function flush() {
			if (kind != "") print kind, (ns == "" ? "-" : ns), (name == "" ? "-" : name)
			kind = ""; ns = ""; name = ""; meta = 0
		}
		function value(s) { sub(/^[^:]*:[ \t]*/, "", s); gsub(/["\047]/, "", s); return s }
		/^---/ { flush(); next }
		/^kind:/ { kind = value($0); next }
		/^metadata:/ { meta = 1; next }
		/^[^ \t#]/ { meta = 0 }
		meta && /^  name:/ { name = value($0) }
		meta && /^  namespace:/ { ns = value($0) }
		END { flush() }'
}

# overlay_images prints "KIND IMAGE" for every image reference of the
# rendered overlay, including those inside ConfigMaps (engine profiles).
overlay_images() {
	printf '%s\n' "$OVERLAY_RENDERED" | awk '
		/^---/ { kind = ""; next }
		/^kind:/ { kind = $2; next }
		/^[ \t]*(- )?image:[ \t]/ {
			v = $0
			sub(/^[ \t]*(- )?image:[ \t]*/, "", v)
			sub(/[ \t]+#.*$/, "", v)
			gsub(/["\047]/, "", v)
			if (v != "") print (kind == "" ? "-" : kind), v
		}'
}

# resolve_names sets NS and DEPLOY (the gateway Deployment).
resolve_names() {
	local line ns name
	render_overlay
	line=$(overlay_objects | awk '$1 == "Deployment" && !found { print; found = 1 }')
	ns=$(printf '%s\n' "$line" | awk '{ print $2 }')
	name=$(printf '%s\n' "$line" | awk '{ print $3 }')
	if [ -z "$ns" ] || [ "$ns" = "-" ]; then
		# No namespace on the Deployment: take the first one of another object,
		# else the Namespace the overlay creates.
		ns=$(overlay_objects | awk '$2 != "-" && !found { print $2; found = 1 }')
		if [ -z "$ns" ]; then
			ns=$(overlay_objects | awk '$1 == "Namespace" && !found { print $3; found = 1 }')
		fi
	fi
	NS=${QMX_NAMESPACE:-${ns:-default}}
	DEPLOY=${QMX_DEPLOYMENT:-$name}
	if [ -z "$DEPLOY" ] || [ "$DEPLOY" = "-" ]; then
		die "no Deployment found in $OVERLAY_DIR (set QMX_DEPLOYMENT)"
	fi
}

# resolve_images sets ENGINE_IMAGE and GATEWAY_IMAGE.
resolve_images() {
	render_overlay
	if [ -z "${ENGINE_IMAGE:-}" ]; then
		ENGINE_IMAGE=$(overlay_images | awk '$2 ~ /duckdb-engine/ && !found { print $2; found = 1 }')
		ENGINE_IMAGE=${ENGINE_IMAGE:-$DEFAULT_ENGINE_IMAGE}
	fi
	if [ -z "${GATEWAY_IMAGE:-}" ]; then
		GATEWAY_IMAGE=$(overlay_images | awk -v e="$ENGINE_IMAGE" '$1 == "Deployment" && $2 != e && !found { print $2; found = 1 }')
		GATEWAY_IMAGE=${GATEWAY_IMAGE:-$DEFAULT_GATEWAY_IMAGE}
	fi
}

# gateway_pods prints the names of the pods of the gateway Deployment.
gateway_pods() {
	local selector
	# shellcheck disable=SC2016 # a Go template, not shell
	selector=$(kctl -n "$NS" get deployment "$DEPLOY" \
		-o go-template='{{range $k, $v := .spec.selector.matchLabels}}{{$k}}={{$v}},{{end}}') || return 1
	kctl -n "$NS" get pods -l "${selector%,}" -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
}

# pod_gone NAME succeeds once the pod NAME no longer exists.
pod_gone() {
	local out
	out=$(kctl -n "$NS" get pod "$1" --ignore-not-found -o name 2>/dev/null) && [ -z "$out" ]
}

# diagnostics prints the state of the namespace after a failure.
diagnostics() {
	step "diagnostics (namespace $NS)"
	kctl -n "$NS" get pods -o wide >&2 || true
	say "recent events:"
	kctl -n "$NS" get events --sort-by=.lastTimestamp 2>/dev/null | tail -n 25 >&2 || true
	say "gateway log (last 150 lines):"
	kctl -n "$NS" logs "deployment/$DEPLOY" --tail=150 >&2 || true
}

# Engine pods ------------------------------------------------------------------

# Engine pods of sessions; warm-pool pods (not yet claimed) do not count.
ENGINE_SELECTOR='app.kubernetes.io/component=engine,quackmux.io/warm!=true'

# engine_pods prints the names of the engine pods that are neither warm nor
# terminating, sorted.
engine_pods() {
	kctl -n "$NS" get pods -l "$ENGINE_SELECTOR" \
		-o jsonpath='{range .items[*]}{.metadata.name}{" "}{.metadata.deletionTimestamp}{"\n"}{end}' |
		awk 'NF == 1 { print $1 }' | LC_ALL=C sort
}

# new_pods BEFORE_FILE prints the engine pods that are not in BEFORE_FILE.
new_pods() { engine_pods | LC_ALL=C comm -13 "$1" -; }

# Client pod -----------------------------------------------------------------

CLIENT_POD=${CLIENT_POD:-quackmux-e2e-client}
CLIENT_UID=${CLIENT_UID:-1000}

# ensure_client_pod (re)creates the pod CLIENT_POD with the DuckDB CLI of the
# engine image; the scenarios start their clients in it with kubectl exec.
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
