package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/velocor/velocor/internal/monitoring"
	"github.com/velocor/velocor/internal/store"
)

// History graphs (docs/review-history.md P1-1 step 4) come from the built-in Prometheus. The UI asks for a
// graph by name; velod builds the query, so browsers never send PromQL.

type graphLine struct {
	name  string
	query string // %[1]s is the selector, %[2]s the rate() window
}

type graphDef struct {
	scope string // "pool", "volume" or "" (the whole appliance)
	unit  string // ratio, bytes, iops, bytes_per_sec, seconds
	lines []graphLine
}

var graphs = map[string]graphDef{
	"pool-space": {scope: "pool", unit: "ratio", lines: []graphLine{
		{"Allocated", `velocor_pool_used_ratio{%[1]s}`},
	}},
	"pool-iops": {scope: "pool", unit: "iops", lines: []graphLine{
		{"Read", `sum(rate(velocor_pool_read_ops_total{%[1]s}[%[2]s]))`},
		{"Write", `sum(rate(velocor_pool_write_ops_total{%[1]s}[%[2]s]))`},
	}},
	"pool-throughput": {scope: "pool", unit: "bytes_per_sec", lines: []graphLine{
		{"Read", `sum(rate(velocor_pool_read_bytes_total{%[1]s}[%[2]s]))`},
		{"Write", `sum(rate(velocor_pool_written_bytes_total{%[1]s}[%[2]s]))`},
	}},
	"pool-latency": {scope: "pool", unit: "seconds", lines: []graphLine{
		{"Read", `sum(rate(velocor_pool_read_seconds_total{%[1]s}[%[2]s])) / sum(rate(velocor_pool_read_ops_total{%[1]s}[%[2]s]))`},
		{"Write", `sum(rate(velocor_pool_write_seconds_total{%[1]s}[%[2]s])) / sum(rate(velocor_pool_write_ops_total{%[1]s}[%[2]s]))`},
	}},
	"volume-space": {scope: "volume", unit: "bytes", lines: []graphLine{
		{"Allocated", `velocor_volume_allocated_bytes{%[1]s}`},
		{"Size", `velocor_volume_size_bytes{%[1]s}`},
	}},
	"volume-iops": {scope: "volume", unit: "iops", lines: []graphLine{
		{"Read", `sum(rate(velocor_volume_read_ops_total{%[1]s}[%[2]s]))`},
		{"Write", `sum(rate(velocor_volume_write_ops_total{%[1]s}[%[2]s]))`},
	}},
	"volume-throughput": {scope: "volume", unit: "bytes_per_sec", lines: []graphLine{
		{"Read", `sum(rate(velocor_volume_read_bytes_total{%[1]s}[%[2]s]))`},
		{"Write", `sum(rate(velocor_volume_written_bytes_total{%[1]s}[%[2]s]))`},
	}},
	"volume-latency": {scope: "volume", unit: "seconds", lines: []graphLine{
		{"Read", `sum(rate(velocor_volume_read_seconds_total{%[1]s}[%[2]s])) / sum(rate(velocor_volume_read_ops_total{%[1]s}[%[2]s]))`},
		{"Write", `sum(rate(velocor_volume_write_seconds_total{%[1]s}[%[2]s])) / sum(rate(velocor_volume_write_ops_total{%[1]s}[%[2]s]))`},
	}},
	"appliance-iops": {unit: "iops", lines: []graphLine{
		{"Read", `sum(rate(velocor_pool_read_ops_total{%[1]s}[%[2]s]))`},
		{"Write", `sum(rate(velocor_pool_write_ops_total{%[1]s}[%[2]s]))`},
	}},
	"appliance-throughput": {unit: "bytes_per_sec", lines: []graphLine{
		{"Read", `sum(rate(velocor_pool_read_bytes_total{%[1]s}[%[2]s]))`},
		{"Write", `sum(rate(velocor_pool_written_bytes_total{%[1]s}[%[2]s]))`},
	}},
}

var graphRanges = map[string]time.Duration{
	"1h": time.Hour, "6h": 6 * time.Hour, "24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "15d": 15 * 24 * time.Hour,
}

// promString quotes s as a PromQL string literal.
func promString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

// graphQuery builds a line's query for a selector and range step.
func graphQuery(l graphLine, selector string, step time.Duration) string {
	window := 4 * step
	if window < 2*time.Minute {
		window = 2 * time.Minute // at least four 30 s scrapes
	}
	return fmt.Sprintf(l.query, selector, fmt.Sprintf("%ds", int(window.Seconds())))
}

func (s *Server) monitoringOn() error {
	if s.monitoring == nil {
		return fmt.Errorf("%w: built-in monitoring isn't available on this host", store.ErrInvalidArgument)
	}
	st, err := s.store.GetSettings()
	if err != nil {
		return err
	}
	if !st.MonitoringEnabled {
		return errMonitoringOff
	}
	return nil
}

var errMonitoringOff = errors.New("built-in monitoring is off; turn it on on the System page")

// writeMonitoringError reports monitoring that is off as 409 and a service that doesn't
// answer (still starting, or failed) as 503.
func writeMonitoringError(w http.ResponseWriter, err error) {
	if errors.Is(err, errMonitoringOff) {
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error()})
		return
	}
	if errors.Is(err, monitoring.ErrUnavailable) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": err.Error()})
		return
	}
	writeError(w, err)
}

func (s *Server) handleGraph(w http.ResponseWriter, r *http.Request) {
	if err := s.monitoringOn(); err != nil {
		writeMonitoringError(w, err)
		return
	}
	def, ok := graphs[r.PathValue("name")]
	if !ok {
		writeError(w, fmt.Errorf("%w: no graph %q", store.ErrNotFound, r.PathValue("name")))
		return
	}
	rng, ok := graphRanges[r.URL.Query().Get("range")]
	if r.URL.Query().Get("range") == "" {
		rng, ok = 6*time.Hour, true
	}
	if !ok {
		writeError(w, fmt.Errorf("%w: range must be 1h, 6h, 24h, 7d or 15d", store.ErrInvalidArgument))
		return
	}
	selector := ""
	switch def.scope {
	case "pool":
		name := r.URL.Query().Get("pool")
		if name == "" {
			writeError(w, fmt.Errorf("%w: pool is required", store.ErrInvalidArgument))
			return
		}
		selector = "pool=" + promString(name)
	case "volume":
		name := r.URL.Query().Get("volume")
		if name == "" {
			writeError(w, fmt.Errorf("%w: volume is required", store.ErrInvalidArgument))
			return
		}
		selector = "volume=" + promString(name)
		if pool := r.URL.Query().Get("pool"); pool != "" {
			selector += ",pool=" + promString(pool)
		}
	}
	end := time.Now()
	step := rng / 240
	if step < 30*time.Second {
		step = 30 * time.Second
	}
	type line struct {
		Name   string             `json:"name"`
		Points []monitoring.Point `json:"points"`
	}
	out, errs := make([]line, len(def.lines)), make([]error, len(def.lines))
	var wg sync.WaitGroup
	for i, l := range def.lines {
		wg.Go(func() {
			series, err := s.monitoring.QueryRange(r.Context(), graphQuery(l, selector, step), end.Add(-rng), end, step)
			pts := []monitoring.Point{}
			if len(series) > 0 {
				pts = series[0].Points
			}
			out[i], errs[i] = line{Name: l.name, Points: pts}, err
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			writeMonitoringError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"unit": def.unit, "step_seconds": int(step.Seconds()), "lines": out})
}

func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	if err := s.monitoringOn(); err != nil {
		writeMonitoringError(w, err)
		return
	}
	alerts, err := s.monitoring.Alerts(r.Context())
	if err != nil {
		writeMonitoringError(w, err)
		return
	}
	silences, err := s.monitoring.Silences(r.Context())
	if err != nil {
		writeMonitoringError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerts": alerts, "silences": silences})
}

func (s *Server) handleCreateSilence(w http.ResponseWriter, r *http.Request) {
	if err := s.monitoringOn(); err != nil {
		writeMonitoringError(w, err)
		return
	}
	var req struct {
		Matchers      []monitoring.Matcher `json:"matchers"`
		DurationHours float64              `json:"duration_hours"`
		Comment       string               `json:"comment"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	if len(req.Matchers) == 0 || req.DurationHours <= 0 || req.DurationHours > 24*30 {
		writeError(w, fmt.Errorf("%w: give matchers and duration_hours (up to 720)", store.ErrInvalidArgument))
		return
	}
	for _, m := range req.Matchers {
		if m.Name == "" || m.Value == "" {
			writeError(w, fmt.Errorf("%w: every matcher needs a name and a value", store.ErrInvalidArgument))
			return
		}
	}
	by := subjectFromContext(r.Context())
	id, err := s.monitoring.CreateSilence(r.Context(), req.Matchers, time.Duration(req.DurationHours*float64(time.Hour)), by, req.Comment)
	if err != nil {
		writeMonitoringError(w, err)
		return
	}
	s.logger.Info("alerts silenced", "silence", id, "matchers", req.Matchers, "hours", req.DurationHours, "by", by)
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleExpireSilence(w http.ResponseWriter, r *http.Request) {
	if err := s.monitoringOn(); err != nil {
		writeMonitoringError(w, err)
		return
	}
	if err := s.monitoring.ExpireSilence(r.Context(), r.PathValue("id")); err != nil {
		writeMonitoringError(w, err)
		return
	}
	s.logger.Info("silence ended", "silence", r.PathValue("id"), "by", subjectFromContext(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"status": "expired"})
}
