package helm

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"
)

// Paths relative to this package.
var (
	repoRoot = filepath.Join("..", "..")
	chartDir = filepath.Join(repoRoot, "deploy", "helm", "quackmux")
)

// releaseName is the release of every rendering: with it the chart's object
// names are those of deploy/kubernetes.
const releaseName = "quackmux"

// kubeVersion is the oldest supported Kubernetes version (Chart.yaml
// kubeVersion); Helm 3 would assume 1.20 without it.
const kubeVersion = "1.30.0"

// object is one Kubernetes object of a rendered manifest stream.
type object = map[string]any

// helmBinary returns the helm binary that $QUACKMUX_HELM names and skips the
// test without it.
func helmBinary(t *testing.T) string {
	t.Helper()
	name := os.Getenv("QUACKMUX_HELM")
	if name == "" {
		t.Skip("set QUACKMUX_HELM to a helm binary (Helm 3 or 4) to check the chart")
	}
	path, err := exec.LookPath(name)
	if err != nil {
		t.Fatalf("QUACKMUX_HELM=%s: %v", name, err)
	}
	return path
}

// helmTemplate runs "helm template" for the release quackmux in namespace ns
// with further arguments (--set, --set-file, -f).
func helmTemplate(t *testing.T, ns string, args ...string) ([]byte, error) {
	t.Helper()
	all := append([]string{"template", releaseName, chartDir, "--namespace", ns, "--kube-version", kubeVersion}, args...)
	cmd := exec.Command(helmBinary(t), all...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("helm %s: %v\n%s", strings.Join(all, " "), err, stderr.String())
	}
	return out, nil
}

// render renders the chart and decodes its objects.
func render(t *testing.T, ns string, args ...string) []object {
	t.Helper()
	out, err := helmTemplate(t, ns, args...)
	if err != nil {
		t.Fatal(err)
	}
	return parseObjects(t, out)
}

// kustomize renders a Kustomize directory relative to the repository root.
func kustomize(t *testing.T, dir string) []object {
	t.Helper()
	kubectl, err := exec.LookPath("kubectl")
	if err != nil {
		t.Fatalf("kubectl kustomize %s: %v", dir, err)
	}
	cmd := exec.Command(kubectl, "kustomize", filepath.Join(repoRoot, dir))
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("kubectl kustomize %s: %v\n%s", dir, err, stderr.String())
	}
	return parseObjects(t, out)
}

var docSeparator = regexp.MustCompile(`(?m)^---[ \t]*$`)

// parseObjects decodes a stream of YAML documents; empty documents (only
// comments) are skipped.
func parseObjects(t *testing.T, stream []byte) []object {
	t.Helper()
	var objs []object
	for i, doc := range docSeparator.Split(string(stream), -1) {
		var o object
		if err := yaml.Unmarshal([]byte(doc), &o); err != nil {
			t.Fatalf("document %d: %v\n%s", i, err, doc)
		}
		if len(o) > 0 {
			objs = append(objs, o)
		}
	}
	return objs
}

// get returns the value at path (map keys), or nil.
func get(v any, path ...string) any {
	for _, p := range path {
		m, ok := v.(map[string]any)
		if !ok {
			return nil
		}
		v = m[p]
	}
	return v
}

func str(v any, path ...string) string {
	s, _ := get(v, path...).(string)
	return s
}

func mapAt(v any, path ...string) map[string]any {
	m, _ := get(v, path...).(map[string]any)
	return m
}

func listAt(v any, path ...string) []any {
	l, _ := get(v, path...).([]any)
	return l
}

// objectKey identifies an object by kind, namespace and name.
func objectKey(o object) string {
	return fmt.Sprintf("%s %s/%s", str(o, "kind"), str(o, "metadata", "namespace"), str(o, "metadata", "name"))
}

// find returns the object of kind and name, or nil.
func find(objs []object, kind, name string) object {
	for _, o := range objs {
		if str(o, "kind") == kind && str(o, "metadata", "name") == name {
			return o
		}
	}
	return nil
}

func mustFind(t *testing.T, objs []object, kind, name string) object {
	t.Helper()
	o := find(objs, kind, name)
	if o == nil {
		t.Fatalf("the rendering has no %s %s", kind, name)
	}
	return o
}

// The documented differences between the chart and deploy/kubernetes
// (deploy/helm/quackmux/README.md, "Unterschiede zu Kustomize"); everything
// else must be equal:
//
//  1. The chart's objects carry Helm's labels in their metadata (helmLabels),
//     and the Secrets that Helm keeps the annotation helmAnnotations.
//  2. Its pod template carries the annotation checksum/config: the chart
//     rolls the gateway out on a changed configuration this way.
//  3. Kustomize appends a hash of the content to the names of the ConfigMap
//     and the Secrets of the session key and the admin token; the chart's
//     names have none.
//  4. Secrets: only their keys are compared. Kustomize ships public
//     placeholders, the chart generates the session key and the admin token
//     and builds the users file from its values.
//  5. Kustomize creates the Namespace (with the Pod Security labels); a Helm
//     release goes into an existing one.

// helmLabels and helmAnnotations are the metadata of difference 1.
var (
	helmLabels      = []string{"helm.sh/chart", "app.kubernetes.io/instance", "app.kubernetes.io/version", "app.kubernetes.io/managed-by"}
	helmAnnotations = []string{"helm.sh/resource-policy"}
)

// checksumAnnotation is the annotation of difference 2.
const checksumAnnotation = "checksum/config"

// kustomizeHashSuffix matches a name with the hash of difference 3.
var kustomizeHashSuffix = regexp.MustCompile(`^(.+)-[a-z0-9]{10}$`)

// normalizeHelm removes differences 1, 2 and 4 from the chart's objects.
func normalizeHelm(objs []object) []object {
	for _, o := range objs {
		deleteKeys(mapAt(o, "metadata"), "labels", helmLabels...)
		deleteKeys(mapAt(o, "metadata"), "annotations", helmAnnotations...)
		deleteKeys(mapAt(o, "spec", "template", "metadata"), "annotations", checksumAnnotation)
		redactSecret(o)
	}
	return objs
}

// normalizeKustomize removes differences 3, 4 and 5 from the objects of
// kubectl kustomize.
func normalizeKustomize(objs []object) []object {
	renamed := map[string]string{}
	var out []object
	for _, o := range objs {
		switch str(o, "kind") {
		case "Namespace":
			continue
		case "ConfigMap", "Secret":
			name := str(o, "metadata", "name")
			if m := kustomizeHashSuffix.FindStringSubmatch(name); m != nil {
				renamed[name] = m[1]
			}
		}
		out = append(out, o)
	}
	for _, o := range out {
		rename(o, renamed)
		redactSecret(o)
	}
	return out
}

// deleteKeys deletes keys from the map m[field], and m[field] once it is
// empty.
func deleteKeys(m map[string]any, field string, keys ...string) {
	inner, _ := m[field].(map[string]any)
	if inner == nil {
		return
	}
	for _, k := range keys {
		delete(inner, k)
	}
	if len(inner) == 0 {
		delete(m, field)
	}
}

// rename replaces, in place, every string value that is a key of names (the
// hashed names and the references to them).
func rename(v any, names map[string]string) {
	switch v := v.(type) {
	case map[string]any:
		for k, e := range v {
			if s, ok := e.(string); ok {
				if n, ok := names[s]; ok {
					v[k] = n
				}
				continue
			}
			rename(e, names)
		}
	case []any:
		for i, e := range v {
			if s, ok := e.(string); ok {
				if n, ok := names[s]; ok {
					v[i] = n
				}
				continue
			}
			rename(e, names)
		}
	}
}

// redactSecret keeps only the keys of a Secret's data and stringData.
func redactSecret(o object) {
	if str(o, "kind") != "Secret" {
		return
	}
	keys := map[string]any{}
	for _, field := range []string{"data", "stringData"} {
		for k := range mapAt(o, field) {
			keys[k] = "(compared by key only)"
		}
		delete(o, field)
	}
	if len(keys) > 0 {
		o["data"] = keys
	}
}

// canonical renders an object as YAML with sorted keys.
func canonical(t *testing.T, o object) string {
	t.Helper()
	b, err := yaml.Marshal(o)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// diffObjects lists every object that exists on one side only or differs,
// the latter with a diff of the canonical YAML.
func diffObjects(t *testing.T, wantName string, want []object, gotName string, got []object) []string {
	t.Helper()
	wantBy := byKey(t, want)
	gotBy := byKey(t, got)
	var diffs []string
	for _, k := range sortedKeys(wantBy) {
		g, ok := gotBy[k]
		if !ok {
			diffs = append(diffs, fmt.Sprintf("%s: in %s, missing in %s", k, wantName, gotName))
			continue
		}
		if w, g := canonical(t, wantBy[k]), canonical(t, g); w != g {
			diffs = append(diffs, fmt.Sprintf("%s differs (-%s +%s):\n%s", k, wantName, gotName, lineDiff(w, g)))
		}
	}
	for _, k := range sortedKeys(gotBy) {
		if _, ok := wantBy[k]; !ok {
			diffs = append(diffs, fmt.Sprintf("%s: in %s, missing in %s", k, gotName, wantName))
		}
	}
	return diffs
}

func byKey(t *testing.T, objs []object) map[string]object {
	t.Helper()
	m := map[string]object{}
	for _, o := range objs {
		k := objectKey(o)
		if _, dup := m[k]; dup {
			t.Fatalf("%s appears twice", k)
		}
		m[k] = o
	}
	return m
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// lineDiff shows the lines of a and b that differ ("-" only in a, "+" only
// in b) with two lines of context.
func lineDiff(a, b string) string {
	x := strings.Split(strings.TrimSuffix(a, "\n"), "\n")
	y := strings.Split(strings.TrimSuffix(b, "\n"), "\n")
	// lcs[i][j] is the length of the longest common subsequence of x[i:]
	// and y[j:].
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	type line struct {
		op   string
		text string
	}
	var lines []line
	for i, j := 0, 0; i < len(x) || j < len(y); {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			lines = append(lines, line{"  ", x[i]})
			i++
			j++
		case i < len(x) && (j == len(y) || lcs[i+1][j] >= lcs[i][j+1]):
			lines = append(lines, line{"- ", x[i]})
			i++
		default:
			lines = append(lines, line{"+ ", y[j]})
			j++
		}
	}
	const context = 2
	show := make([]bool, len(lines))
	for i, l := range lines {
		if l.op == "  " {
			continue
		}
		for k := max(0, i-context); k <= min(len(lines)-1, i+context); k++ {
			show[k] = true
		}
	}
	var sb strings.Builder
	gap := false
	for i, l := range lines {
		if !show[i] {
			gap = true
			continue
		}
		if gap && sb.Len() > 0 {
			sb.WriteString("  ...\n")
		}
		gap = false
		sb.WriteString(l.op + l.text + "\n")
	}
	return sb.String()
}

// TestDiffObjects checks the normalization without tools: the documented
// differences vanish, every other one is reported.
func TestDiffObjects(t *testing.T) {
	const kustomized = `apiVersion: v1
kind: Namespace
metadata:
  name: quackmux
---
apiVersion: v1
kind: ConfigMap
metadata:
  name: quackmux-config-5mfk8g45c4
  namespace: quackmux
  labels: {app.kubernetes.io/name: quackmux, app.kubernetes.io/part-of: quackmux}
data:
  config.yaml: "a: 1\n"
---
apiVersion: v1
kind: Secret
metadata:
  name: quackmux-session-key-hm5gc7b6f7
  namespace: quackmux
type: Opaque
data:
  key: UExBQ0VIT0xERVI=
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: quackmux
  namespace: quackmux
spec:
  template:
    metadata:
      annotations: {prometheus.io/scrape: "true"}
    spec:
      volumes:
        - {name: config, configMap: {name: quackmux-config-5mfk8g45c4}}
        - {name: session-key, secret: {secretName: quackmux-session-key-hm5gc7b6f7, defaultMode: 288}}
`
	const helmed = `---
# Source: quackmux/templates/configmap.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: quackmux-config
  namespace: quackmux
  labels:
    app.kubernetes.io/name: quackmux
    app.kubernetes.io/part-of: quackmux
    app.kubernetes.io/instance: quackmux
    app.kubernetes.io/version: "0.1.0"
    app.kubernetes.io/managed-by: Helm
    helm.sh/chart: quackmux-0.1.0
data:
  config.yaml: |
    a: 1
---
apiVersion: v1
kind: Secret
metadata:
  name: quackmux-session-key
  namespace: quackmux
  labels:
    helm.sh/chart: quackmux-0.1.0
  annotations:
    helm.sh/resource-policy: keep
type: Opaque
data:
  key: "R0VORVJBVEVE"
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: quackmux
  namespace: quackmux
spec:
  template:
    metadata:
      annotations:
        checksum/config: 0123
        prometheus.io/scrape: "true"
    spec:
      volumes:
        - name: config
          configMap:
            name: quackmux-config
        - name: session-key
          secret:
            secretName: quackmux-session-key
            defaultMode: 0440
`
	diff := func(helm string) []string {
		want := normalizeKustomize(parseObjects(t, []byte(kustomized)))
		got := normalizeHelm(parseObjects(t, []byte(helm)))
		return diffObjects(t, "kustomize", want, "helm", got)
	}
	if d := diff(helmed); len(d) > 0 {
		t.Fatalf("documented differences were reported:\n%s", strings.Join(d, "\n"))
	}
	for _, tc := range []struct{ name, from, to, want string }{
		{"configuration", "    a: 1\n", "    a: 2\n", "ConfigMap quackmux/quackmux-config differs"},
		{"secret key", "  key: \"R0VORVJBVEVE\"", "  token: \"R0VORVJBVEVE\"", "Secret quackmux/quackmux-session-key differs"},
		{"label", "    helm.sh/chart: quackmux-0.1.0\n  annotations", "    app.kubernetes.io/part-of: other\n  annotations", "Secret quackmux/quackmux-session-key differs"},
		{"other annotation", "    helm.sh/resource-policy: keep", "    example.com/policy: keep", "Secret quackmux/quackmux-session-key differs"},
		{"reference", "secretName: quackmux-session-key\n", "secretName: other\n", "Deployment quackmux/quackmux differs"},
		{"annotation", "        prometheus.io/scrape: \"true\"", "        prometheus.io/scrape: \"false\"", "Deployment quackmux/quackmux differs"},
		{"object only in helm", "kind: ConfigMap", "kind: Job", "ConfigMap quackmux/quackmux-config: in kustomize, missing in helm"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := strings.Replace(helmed, tc.from, tc.to, 1)
			if changed == helmed {
				t.Fatalf("%q is not in the test input", tc.from)
			}
			d := strings.Join(diff(changed), "\n")
			if !strings.Contains(d, tc.want) {
				t.Fatalf("want a difference %q, got:\n%s", tc.want, d)
			}
		})
	}
}
