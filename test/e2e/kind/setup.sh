#!/usr/bin/env bash
#
# Prepares the kind cluster of the quackmux end-to-end test: creates the
# cluster "quackmux" (if needed), builds the engine and gateway images, loads
# them into the cluster and deploys the dev overlay. Running it again updates
# the deployment with freshly built images.
#
#   bash test/e2e/kind/setup.sh
#
# Optional environment (see env.sh for more):
#   SKIP_BUILD=1       load the existing images instead of building them
#   KIND_NODE_IMAGE    node image for "kind create cluster --image"
#   ROLLOUT_TIMEOUT    wait for the gateway rollout (default 180s)
set -euo pipefail
# shellcheck source=env.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/env.sh"

need docker kind kubectl
render_overlay
resolve_names
resolve_images

say "overlay:  $OVERLAY_DIR"
say "gateway:  deployment/$DEPLOY in namespace $NS, image $GATEWAY_IMAGE"
say "engines:  image $ENGINE_IMAGE"

# The test relies on two properties of the overlay; warn early if they seem
# to be missing.
if ! grep -q 'mode: none' <<<"$OVERLAY_RENDERED"; then
	warn "the overlay does not seem to use auth.mode none; test.sh picks users with the token option user=..."
fi
if ! grep -q -e QUACKMUX_SESSION_KEY -e sessionKeyFile <<<"$OVERLAY_RENDERED"; then
	warn "the overlay configures no session key (QUACKMUX_SESSION_KEY or server.sessionKeyFile): sessions cannot survive a gateway restart (scenario k4)"
fi

step "kind cluster $KIND_CLUSTER"
clusters=$(kind get clusters 2>/dev/null || true)
if grep -qx -- "$KIND_CLUSTER" <<<"$clusters"; then
	say "the cluster exists"
else
	kind create cluster --name "$KIND_CLUSTER" ${KIND_NODE_IMAGE:+--image "$KIND_NODE_IMAGE"} --wait 120s
fi
kctl cluster-info >/dev/null || die "cannot reach the cluster with context $KUBE_CONTEXT"

step "images"
if [ -n "${SKIP_BUILD:-}" ]; then
	say "SKIP_BUILD is set: using the existing images"
else
	build_images "$ENGINE_IMAGE" "$GATEWAY_IMAGE"
fi
kind load docker-image --name "$KIND_CLUSTER" "$ENGINE_IMAGE" "$GATEWAY_IMAGE"

step "deploy"
existed=''
if kctl -n "$NS" get deployment "$DEPLOY" >/dev/null 2>&1; then existed=1; fi
kctl apply -k "$OVERLAY_DIR"
if [ -n "$existed" ]; then
	# Same image tags, new content: restart the gateway and drop the engines of
	# earlier runs, so that everything runs the images just loaded.
	say "restarting deployment/$DEPLOY and removing old engine pods"
	kctl -n "$NS" rollout restart "deployment/$DEPLOY" >/dev/null
	kctl -n "$NS" delete pods -l app.kubernetes.io/component=engine --ignore-not-found --wait=false >/dev/null
fi
if ! kctl -n "$NS" rollout status "deployment/$DEPLOY" --timeout="${ROLLOUT_TIMEOUT:-180s}"; then
	diagnostics
	die "deployment/$DEPLOY did not become ready"
fi
ok "deployment/$DEPLOY is ready in namespace $NS"
say "next: bash test/e2e/kind/test.sh"
