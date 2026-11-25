package helm

import (
	"cmp"
	"fmt"
	"math"
	"strings"
	"testing"
)

// TestChartGracePeriod checks that the chart refuses a
// terminationGracePeriodSeconds that is not greater than
// server.shutdownTimeout: the kubelet would kill the gateway before it
// answers the requests it has read (shortly before the shutdown timeout),
// and a client may send a query again. The chart reads the duration like the
// gateway: every value is checked against config.Parse.
func TestChartGracePeriod(t *testing.T) {
	for _, timeout := range []string{
		``, // the default, 30s
		`"30s"`, `"1m30s"`, `"500ms"`, `"0s"`, `"2h"`,
		`44.5`, `"120"`, `"0"`, `0`,
	} {
		t.Run(cmp.Or(timeout, "default"), func(t *testing.T) {
			var set []string
			if timeout != "" {
				set = []string{"--set-json", "config.server.shutdownTimeout=" + timeout}
			}
			// The gateway's reading of the timeout, from a rendering whose
			// grace period is long enough for every case.
			objs := render(t, "quackmux", append(set, "--set", "terminationGracePeriodSeconds=86400")...)
			secs := int64(math.Floor(validateConfig(t, configText(t, objs)).Server.ShutdownTimeout.Seconds()))

			if _, err := helmTemplate(t, "quackmux", append(set, "--set", fmt.Sprintf("terminationGracePeriodSeconds=%d", secs+1))...); err != nil {
				t.Errorf("shutdownTimeout %s (%ds): the chart refuses terminationGracePeriodSeconds %d: %v", timeout, secs, secs+1, err)
			}
			_, err := helmTemplate(t, "quackmux", append(set, "--set", fmt.Sprintf("terminationGracePeriodSeconds=%d", secs))...)
			if err == nil || !strings.Contains(err.Error(), "must be greater than server.shutdownTimeout") {
				t.Errorf("shutdownTimeout %s (%ds): want a refusal of terminationGracePeriodSeconds %d, got %v", timeout, secs, secs, err)
			}
		})
	}
}
