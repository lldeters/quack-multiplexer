#!/usr/bin/env bash
#
# Deletes the kind cluster of the quackmux end-to-end test (and with it every
# gateway and engine pod). The local Docker images are kept.
#
#   bash test/e2e/kind/teardown.sh
set -euo pipefail
# shellcheck source=env.sh
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/env.sh"

need kind
clusters=$(kind get clusters 2>/dev/null || true)
if grep -qx -- "$KIND_CLUSTER" <<<"$clusters"; then
	kind delete cluster --name "$KIND_CLUSTER"
	ok "deleted the kind cluster $KIND_CLUSTER"
else
	say "the kind cluster $KIND_CLUSTER does not exist"
fi
