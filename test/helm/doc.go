// Package helm checks the Helm chart deploy/helm/quackmux against the
// Kustomize manifests in deploy/kubernetes and against the gateway's own
// configuration checks.
//
// The chart ships copies of deploy/kubernetes/base/config.yaml and
// overlays/ha/config.yaml (files/); a test without further tools fails when
// they differ. The other tests run "helm template" and "kubectl kustomize"
// and need the binary named by $QUACKMUX_HELM (Helm 3 or 4) and kubectl;
// without $QUACKMUX_HELM they are skipped, CI sets it:
//
//	QUACKMUX_HELM=helm go test -count=1 ./test/helm/
//
// The rendering with default values must equal "kubectl kustomize
// deploy/kubernetes/base", the one with ha.enabled "kubectl kustomize
// deploy/kubernetes/overlays/ha", object by object, except for the documented
// differences (normalize, deploy/helm/quackmux/README.md).
package helm
