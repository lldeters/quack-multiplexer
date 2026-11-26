package proxy_test

import (
	"testing"

	"quackmux/internal/wire"
)

// Message types a Quack server does not accept from clients are counted
// under one label value: anyone can send them before authenticating, and a
// label value per type would grow the metric with every type tried.
func TestUnsupportedTypesShareOneLabel(t *testing.T) {
	h := newHarness(t, "", nil)
	for typ := 0; typ < 256; typ++ {
		mt := wire.MessageType(typ)
		if mt.IsClientRequest() {
			continue
		}
		mustError(t, h.post(wire.EncodeEmpty(wire.Header{Type: mt}), nil), "Unsupported message type")
	}
	mfs, err := h.met.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]float64{}
	for _, mf := range mfs {
		if mf.GetName() != "quackmux_requests_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			var typ, outcome string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "type":
					typ = l.GetValue()
				case "outcome":
					outcome = l.GetValue()
				}
			}
			if outcome == "unsupported" {
				types[typ] += m.GetCounter().GetValue()
			}
		}
	}
	if len(types) != 1 || types["INVALID"] < 240 {
		t.Fatalf("unsupported requests by type label: %v, want all under INVALID", types)
	}
}
