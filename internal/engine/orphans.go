package engine

import "sort"

// RecoveredTargets returns the routing targets of the engines that Start
// found and that sessions of an earlier gateway process may still hold
// connections on: READY or DRAINING engines of the share levels USER, GROUP
// and SERVER. A CONNECTION engine serves one session only and ends with it;
// one whose session is not adopted ends after engine.recoveryGrace.
func (m *Manager) RecoveredTargets() []Target {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Target
	for _, e := range m.engines {
		if !e.Recovered || e.Key.ShareLevel == ShareConnection || e.Endpoint == "" ||
			(e.State != StateReady && e.State != StateDraining) {
			continue
		}
		out = append(out, e.target())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
