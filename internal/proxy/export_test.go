package proxy

// TrackedQueries returns how many sessions and queries the query time limit
// follows.
func TrackedQueries(g *Gateway) (sessions, queries int) {
	if g.limits == nil {
		return 0, 0
	}
	g.limits.mu.Lock()
	defer g.limits.mu.Unlock()
	for _, sq := range g.limits.sessions {
		queries += len(sq.queries)
	}
	return len(g.limits.sessions), queries
}
