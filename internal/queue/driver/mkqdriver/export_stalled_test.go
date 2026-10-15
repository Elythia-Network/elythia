package mkqdriver

// StalledCheckers reports how many Workers of qname run the stalled-job
// scan, in the roster and in quarantine. Test-only.
func (s *Server) StalledCheckers(qname string) (roster, quarantine int) {
	s.mu.Lock()
	pool, ok := s.pools[qname]
	s.mu.Unlock()
	if !ok {
		return 0, 0
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	for _, h := range pool.workers {
		if h.stalledChecker {
			roster++
		}
	}
	for _, h := range pool.quarantine {
		if h.stalledChecker {
			quarantine++
		}
	}
	return roster, quarantine
}
