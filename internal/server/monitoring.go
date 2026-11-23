package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/velocor/velocor/internal/monitoring"
	"github.com/velocor/velocor/internal/store"
)

// noScrapeGrace is how long /metrics may go unscraped (with built-in monitoring off) before
// status warns that nothing would alert anyone.
const noScrapeGrace = 15 * time.Minute

// WithMonitoring enables the built-in Prometheus/Alertmanager setting (docs/review-history.md P1-1). Without
// it, the monitoring API reports the feature as unavailable.
func WithMonitoring(m *monitoring.Manager) Option {
	return func(s *Server) { s.monitoring = m }
}

// monitoringState is what the server tracks for monitoring.
type monitoringState struct {
	lastExternalScrape atomic.Int64 // unix nanoseconds
}

// noteScrape records a /metrics scrape by anything but the built-in Prometheus.
func (s *Server) noteScrape(r *http.Request) {
	if subjectFromContext(r.Context()) == "token:"+monitoring.ScrapeTokenName {
		return
	}
	s.mon.lastExternalScrape.Store(time.Now().UnixNano())
}

// monitoringWarning says when no alert would reach anyone: built-in monitoring is off and
// nothing else has scraped /metrics recently.
func (s *Server) monitoringWarning() string {
	if st, err := s.store.GetSettings(); err == nil && st.MonitoringEnabled {
		return ""
	}
	if time.Since(s.startedAt) < noScrapeGrace {
		return ""
	}
	if last := s.mon.lastExternalScrape.Load(); last != 0 && time.Since(time.Unix(0, last)) < noScrapeGrace {
		return ""
	}
	return "No alerts reach anyone: built-in monitoring is off and nothing has scraped /metrics in the last 15 minutes. " +
		"Turn on monitoring on the System page, or point your own Prometheus at the appliance (docs/monitoring.md)."
}

// StartMonitoring applies the saved setting once velod is up (the services don't start on
// their own at boot).
func (s *Server) StartMonitoring(ctx context.Context) {
	if s.monitoring == nil {
		return
	}
	st, err := s.store.GetSettings()
	if err != nil {
		s.logger.Error("read settings for monitoring", "err", err)
		return
	}
	if !st.MonitoringEnabled {
		return
	}
	if err := s.monitoring.Apply(ctx, true, st.Alerting); err != nil {
		s.logger.Error("starting built-in monitoring failed", "err", err)
	}
}

// rotateScrapeToken revokes the built-in Prometheus's previous token and creates a new one.
func (s *Server) rotateScrapeToken() (string, error) {
	tokens, err := s.store.ListApiTokens()
	if err != nil {
		return "", err
	}
	for _, t := range tokens {
		if t.Name == monitoring.ScrapeTokenName {
			if err := s.store.DeleteApiToken(t.ID); err != nil {
				return "", err
			}
		}
	}
	_, secret, err := s.auth.CreateApiToken(monitoring.ScrapeTokenName, store.RoleReadOnly, 0)
	return secret, err
}

// reactorCPUs lists the CPUs the data plane polls on.
func (s *Server) reactorCPUs(ctx context.Context) ([]int, error) {
	if s.dp == nil {
		return nil, fmt.Errorf("no data plane")
	}
	return s.dp.PollingCPUs(ctx)
}

func (s *Server) handleGetMonitoring(w http.ResponseWriter, r *http.Request) {
	if s.monitoring == nil {
		writeJSON(w, http.StatusOK, map[string]any{"available": false})
		return
	}
	st, err := s.store.GetSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "monitoring": s.monitoring.Status(r.Context(), st.MonitoringEnabled)})
}

func (s *Server) handlePutMonitoring(w http.ResponseWriter, r *http.Request) {
	if s.monitoring == nil {
		writeError(w, fmt.Errorf("%w: built-in monitoring isn't available on this host", store.ErrInvalidArgument))
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Enabled == nil {
		writeError(w, fmt.Errorf("%w: body must be {\"enabled\": true|false}", store.ErrInvalidArgument))
		return
	}
	st, err := s.store.GetSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	st.MonitoringEnabled = *req.Enabled
	if err := s.store.SaveSettings(st); err != nil {
		writeError(w, err)
		return
	}
	// Starting both services can take a while on the first run; don't tie it to the client.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 2*time.Minute)
	defer cancel()
	if err := s.monitoring.Apply(ctx, *req.Enabled, st.Alerting); err != nil {
		s.logger.Error("applying the monitoring setting failed", "enabled", *req.Enabled, "err", err)
		writeError(w, fmt.Errorf("monitoring is now %s, but: %w", map[bool]string{true: "on", false: "off"}[*req.Enabled], err))
		return
	}
	s.logger.Info("built-in monitoring changed", "enabled", *req.Enabled, "by", subjectFromContext(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"available": true, "monitoring": s.monitoring.Status(ctx, *req.Enabled)})
}
