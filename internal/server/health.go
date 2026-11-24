package server

import (
	"context"
	"net/http"
	"time"
)

// Health endpoints (docs/review-history.md P2-6), unauthenticated and without detail: what failed is in the
// authenticated status and velod's log, not something to hand anyone who can reach the port.
//
//   GET /healthz  velod is serving requests (liveness: the quadlet's HealthCmd uses it).
//   GET /readyz   velod can do its job: the database and the data plane answer (readiness).

func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if !s.ready(r.Context()) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
		return
	}
	_, _ = w.Write([]byte("ok\n"))
}

func (s *Server) ready(ctx context.Context) bool {
	if _, err := s.store.GetSystemConfig(); err != nil {
		s.logger.Warn("readiness: the database doesn't answer", "err", err)
		return false
	}
	if s.dp == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err := s.dp.Version(ctx); err != nil {
		return false
	}
	return true
}
