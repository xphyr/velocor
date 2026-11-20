package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"time"

	"github.com/velocor/velocor/internal/monitoring"
	"github.com/velocor/velocor/internal/store"
)

// Alerting settings (docs/review-history.md P1-1 step 3). Secrets (SMTP password, webhook and heartbeat URLs)
// are never returned: the API reports whether each is set.

type emailView struct {
	To          []string `json:"to"`
	From        string   `json:"from"`
	SMTPHost    string   `json:"smtp_host"`
	SMTPPort    int      `json:"smtp_port"`
	Username    string   `json:"username,omitempty"`
	PasswordSet bool     `json:"password_set"`
	RequireTLS  bool     `json:"require_tls"`
	MinSeverity string   `json:"min_severity"`
}

type webhookView struct {
	URLSet      bool   `json:"url_set"`
	URLHost     string `json:"url_host,omitempty"` // only the host, to recognise it
	MinSeverity string `json:"min_severity"`
}

func severityOr(s string) string {
	if s == "" {
		return "warning"
	}
	return s
}

func alertingView(a store.AlertingSettings) map[string]any {
	out := map[string]any{"heartbeat_url_set": a.HeartbeatURL != "", "rules": a.Rules}
	if a.HeartbeatURL != "" {
		out["heartbeat_url_host"] = urlHost(a.HeartbeatURL)
	}
	if e := a.Email; e != nil {
		out["email"] = emailView{To: e.To, From: e.From, SMTPHost: e.SMTPHost, SMTPPort: e.SMTPPort, Username: e.Username,
			PasswordSet: e.Password != "", RequireTLS: e.RequireTLS, MinSeverity: severityOr(e.MinSeverity)}
	}
	if w := a.Webhook; w != nil {
		out["webhook"] = webhookView{URLSet: w.URL != "", URLHost: urlHost(w.URL), MinSeverity: severityOr(w.MinSeverity)}
	}
	return out
}

func urlHost(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}

func (s *Server) handleGetAlerting(w http.ResponseWriter, _ *http.Request) {
	st, err := s.store.GetSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerting": alertingView(st.Alerting)})
}

// handlePutAlerting changes what the body names. A receiver given as null is removed; one left
// out is kept. Inside a receiver, a secret left out (or null) keeps the stored one and ""
// clears it. "rules" replaces all rule settings.
func (s *Server) handlePutAlerting(w http.ResponseWriter, r *http.Request) {
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
		return
	}
	st, err := s.store.GetSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	a := st.Alerting
	if v, ok := raw["email"]; ok {
		if a.Email, err = mergeEmail(a.Email, v); err != nil {
			writeError(w, err)
			return
		}
	}
	if v, ok := raw["webhook"]; ok {
		if a.Webhook, err = mergeWebhook(a.Webhook, v); err != nil {
			writeError(w, err)
			return
		}
	}
	if v, ok := raw["heartbeat_url"]; ok {
		var u *string
		if err := json.Unmarshal(v, &u); err != nil {
			writeError(w, fmt.Errorf("%w: heartbeat_url must be a string", store.ErrInvalidArgument))
			return
		}
		if u != nil {
			if *u != "" {
				if err := checkURL("heartbeat_url", *u); err != nil {
					writeError(w, err)
					return
				}
			}
			a.HeartbeatURL = *u
		}
	}
	if v, ok := raw["rules"]; ok {
		var rules map[string]store.RuleSetting
		if err := json.Unmarshal(v, &rules); err != nil {
			writeError(w, fmt.Errorf("%w: rules: %v", store.ErrInvalidArgument, err))
			return
		}
		for name, rs := range rules { // drop settings that change nothing
			if !rs.Disabled && rs.For == "" && rs.Threshold == nil {
				delete(rules, name)
			}
		}
		if err := monitoring.ValidateRuleSettings(rules); err != nil {
			writeError(w, err)
			return
		}
		a.Rules = rules
	}
	st.Alerting = a
	if err := s.store.SaveSettings(st); err != nil {
		writeError(w, err)
		return
	}
	s.logger.Info("alerting settings changed", "by", subjectFromContext(r.Context()))
	if s.monitoring != nil && st.MonitoringEnabled {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
		defer cancel()
		if err := s.monitoring.Reconfigure(ctx, a); err != nil {
			s.logger.Error("applying the alerting settings failed", "err", err)
			writeError(w, fmt.Errorf("saved, but applying them failed: %w", err))
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"alerting": alertingView(a)})
}

func mergeEmail(cur *store.EmailReceiver, v json.RawMessage) (*store.EmailReceiver, error) {
	if string(v) == "null" {
		return nil, nil
	}
	var req struct {
		To          []string `json:"to"`
		From        string   `json:"from"`
		SMTPHost    string   `json:"smtp_host"`
		SMTPPort    int      `json:"smtp_port"`
		Username    string   `json:"username"`
		Password    *string  `json:"password"`
		RequireTLS  *bool    `json:"require_tls"`
		MinSeverity string   `json:"min_severity"`
	}
	if err := json.Unmarshal(v, &req); err != nil {
		return nil, fmt.Errorf("%w: email: %v", store.ErrInvalidArgument, err)
	}
	e := &store.EmailReceiver{To: req.To, From: req.From, SMTPHost: strings.TrimSpace(req.SMTPHost), SMTPPort: req.SMTPPort,
		Username: req.Username, RequireTLS: true, MinSeverity: req.MinSeverity}
	if req.RequireTLS != nil {
		e.RequireTLS = *req.RequireTLS
	}
	if cur != nil {
		e.Password = cur.Password
	}
	if req.Password != nil {
		e.Password = *req.Password
	}
	if e.SMTPPort == 0 {
		e.SMTPPort = 587
	}
	if len(e.To) == 0 {
		return nil, fmt.Errorf("%w: email: give at least one recipient", store.ErrInvalidArgument)
	}
	for _, a := range append(append([]string{}, e.To...), e.From) {
		if addr, err := mail.ParseAddress(a); err != nil || addr.Name != "" || strings.ContainsAny(a, "\"\\\n\r") {
			return nil, fmt.Errorf("%w: email: %q isn't a plain email address", store.ErrInvalidArgument, a)
		}
	}
	if e.SMTPHost == "" || strings.ContainsAny(e.SMTPHost, " \"\\/\n\r") {
		return nil, fmt.Errorf("%w: email: give the SMTP server's host name or address", store.ErrInvalidArgument)
	}
	if e.SMTPPort < 1 || e.SMTPPort > 65535 {
		return nil, fmt.Errorf("%w: email: SMTP port must be 1 to 65535", store.ErrInvalidArgument)
	}
	if strings.ContainsAny(e.Username, "\"\\\n\r") {
		return nil, fmt.Errorf("%w: email: the user name has characters that aren't allowed", store.ErrInvalidArgument)
	}
	if err := checkSeverity("email", e.MinSeverity); err != nil {
		return nil, err
	}
	return e, nil
}

func mergeWebhook(cur *store.WebhookReceiver, v json.RawMessage) (*store.WebhookReceiver, error) {
	if string(v) == "null" {
		return nil, nil
	}
	var req struct {
		URL         *string `json:"url"`
		MinSeverity string  `json:"min_severity"`
	}
	if err := json.Unmarshal(v, &req); err != nil {
		return nil, fmt.Errorf("%w: webhook: %v", store.ErrInvalidArgument, err)
	}
	wh := &store.WebhookReceiver{MinSeverity: req.MinSeverity}
	if cur != nil {
		wh.URL = cur.URL
	}
	if req.URL != nil {
		wh.URL = *req.URL
	}
	if err := checkURL("webhook url", wh.URL); err != nil {
		return nil, err
	}
	if err := checkSeverity("webhook", wh.MinSeverity); err != nil {
		return nil, err
	}
	return wh, nil
}

func checkURL(what, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || strings.ContainsAny(raw, "\n\r") {
		return fmt.Errorf("%w: %s must be an http:// or https:// URL", store.ErrInvalidArgument, what)
	}
	return nil
}

func checkSeverity(what, s string) error {
	switch s {
	case "", "warning", "critical":
		return nil
	}
	return fmt.Errorf("%w: %s: min_severity must be warning or critical", store.ErrInvalidArgument, what)
}

func (s *Server) handleListRules(w http.ResponseWriter, _ *http.Request) {
	st, err := s.store.GetSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	rules, err := monitoring.Rules(st.Alerting.Rules)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rules": rules})
}

func (s *Server) handleTestAlert(w http.ResponseWriter, r *http.Request) {
	if err := s.monitoringOn(); err != nil {
		writeMonitoringError(w, err)
		return
	}
	by := subjectFromContext(r.Context())
	if err := s.monitoring.SendTestAlert(r.Context(), by); err != nil {
		writeMonitoringError(w, err)
		return
	}
	s.logger.Info("test alert sent", "by", by)
	writeJSON(w, http.StatusOK, map[string]any{"status": "sent"})
}
