// Package license_test checks the license files of the repository: LICENSE
// is the unchanged Apache License 2.0, the copies that the engine image and
// the Helm chart ship match LICENSE and NOTICE, and both images declare
// their licenses.
package license_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// apache20 is the SHA-256 of the Apache License 2.0 as the ASF publishes it
// (LICENSE-2.0.txt), which is also the LICENSE of k8s.io/client-go.
const apache20 = "cfc7749b96f63bd31c3c42b5c471bf756814053e847c10f3eb003417bc523d30"

func read(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestLicenseIsTheApacheLicense20(t *testing.T) {
	sum := sha256.Sum256(read(t, "LICENSE"))
	if got := hex.EncodeToString(sum[:]); got != apache20 {
		t.Fatalf("LICENSE has SHA-256 %s, want %s: keep the text of the Apache License 2.0 unchanged "+
			"(the copyright holder belongs in NOTICE)", got, apache20)
	}
}

func TestShippedCopiesMatch(t *testing.T) {
	for copy, orig := range map[string]string{
		"images/engine/licenses/LICENSE": "LICENSE",
		"images/engine/licenses/NOTICE":  "NOTICE",
		"deploy/helm/quackmux/LICENSE":   "LICENSE",
	} {
		if !bytes.Equal(read(t, copy), read(t, orig)) {
			t.Errorf("%s differs from %s; copy it again: cp %s %s", copy, orig, orig, copy)
		}
	}
}

func TestNoticeNamesTheCopyrightHolder(t *testing.T) {
	lines := strings.Split(string(read(t, "NOTICE")), "\n")
	if len(lines) < 2 || lines[0] != "quackmux" || !strings.HasPrefix(lines[1], "Copyright ") {
		t.Fatalf("NOTICE must start with the product name and its copyright line, got %q", lines[:min(2, len(lines))])
	}
}

func TestImagesDeclareTheirLicenses(t *testing.T) {
	for file, want := range map[string][]string{
		"Dockerfile": {`org.opencontainers.image.licenses="Apache-2.0"`, "COPY --from=build /out/licenses /licenses",
			"scripts/third-party-licenses.sh"},
		"images/engine/Dockerfile": {`org.opencontainers.image.licenses="Apache-2.0 AND MIT"`, "COPY licenses/ /licenses/"},
	} {
		s := string(read(t, file))
		for _, w := range want {
			if !strings.Contains(s, w) {
				t.Errorf("%s does not contain %s", file, w)
			}
		}
	}
}
