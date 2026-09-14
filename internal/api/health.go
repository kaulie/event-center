package api

import (
	"fmt"
	"net/http"
)

// handleHealthz is a liveness probe: it only reports that the process is up.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.cfg.Version})
}

// handleReadyz is a readiness probe: it checks that the database answers.
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if err := s.store.DB().PingContext(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "database unavailable")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready", "version": s.cfg.Version})
}

// handleMetrics exposes counters plus a few live gauges in Prometheus text
// format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	events, maxSeq, pending, err := s.store.Stats(r.Context())
	if err == nil {
		s.metrics.Set("eventd_events_stored", nil, events)
		s.metrics.Set("eventd_global_seq", nil, maxSeq)
		s.metrics.Set("eventd_deliveries_pending", nil, pending)
	}
	streams, err := s.store.ListStreams(r.Context())
	if err == nil {
		for name, last := range streams {
			s.metrics.Set("eventd_stream_seq", map[string]string{"stream": name}, last)
		}
	}
	s.metrics.Render(w)
	fmt.Fprintf(w, "# TYPE eventd_build_info gauge\neventd_build_info{version=%q} 1\n", s.cfg.Version)
}
