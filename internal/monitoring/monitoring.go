// Package monitoring runs the appliance's optional built-in Prometheus and Alertmanager
// (TODO.md P1-1): off by default; when an administrator turns it on, velod writes their
// configuration, starts their systemd units (quadlets velocor-prometheus / velocor-alertmanager,
// which have no [Install] section, so only velod starts them) and keeps them reloaded.
//
// Both listen on localhost only. Prometheus scrapes velod's /metrics with a READONLY token that
// velod creates each time it applies the configuration (the old one is revoked), and evaluates
// the shipped alert rules (deploy/monitoring).
package monitoring

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/velocor/velocor/internal/store"
)

const (
	PrometheusUnit   = "velocor-prometheus.service"
	AlertmanagerUnit = "velocor-alertmanager.service"
	// Addresses the quadlets listen on (not the defaults: 9090 is often Cockpit's).
	PrometheusAddr   = "127.0.0.1:19090"
	AlertmanagerAddr = "127.0.0.1:19093"
	// ScrapeTokenName names the API token the built-in Prometheus scrapes with.
	ScrapeTokenName = "builtin-prometheus"
	// Retention is fixed in the quadlet (15 days, at most 2 GB); reported for the UI.
	RetentionDays  = 15
	RetentionBytes = 2 << 30
	// fileGID is the containers' user (nobody), which must read the config and token.
	fileGID = 65534
)

// Runner runs a command on the host (systemctl), returning its combined output.
type Runner func(ctx context.Context, name string, args ...string) ([]byte, error)

// Manager applies the built-in monitoring setting.
type Manager struct {
	// Dir holds config/ (written here, mounted read-only into both containers), prometheus/
	// and alertmanager/ (their data). /var/lib/velocor-monitoring on the appliance.
	Dir string
	// MetricsURL is where Prometheus scrapes velod, e.g. https://127.0.0.1:8443/metrics.
	MetricsURL string
	// Instance is the instance label for the appliance's series (its hostname).
	Instance string
	// NewToken revokes the previous scrape token and returns a new READONLY one.
	NewToken func() (string, error)
	// ReactorCPUs returns the CPUs SPDK polls on, which Prometheus and Alertmanager stay off.
	ReactorCPUs func(ctx context.Context) ([]int, error)
	Run         Runner
	HTTP        *http.Client
	Logger      *slog.Logger
	// PrometheusAddr and AlertmanagerAddr override where the services are reached (tests).
	PrometheusAddr, AlertmanagerAddr string

	mu sync.Mutex
}

// ServiceStatus is one service's state.
type ServiceStatus struct {
	Active bool   `json:"active"`
	Ready  bool   `json:"ready"`
	Detail string `json:"detail,omitempty"`
}

// Status is what the API reports.
type Status struct {
	Enabled        bool          `json:"enabled"`
	Prometheus     ServiceStatus `json:"prometheus"`
	Alertmanager   ServiceStatus `json:"alertmanager"`
	RetentionDays  int           `json:"retention_days"`
	RetentionBytes int64         `json:"retention_bytes"`
}

func (m *Manager) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

func (m *Manager) promAddr() string {
	if m.PrometheusAddr != "" {
		return m.PrometheusAddr
	}
	return PrometheusAddr
}

func (m *Manager) amAddr() string {
	if m.AlertmanagerAddr != "" {
		return m.AlertmanagerAddr
	}
	return AlertmanagerAddr
}

func (m *Manager) client() *http.Client {
	if m.HTTP != nil {
		return m.HTTP
	}
	return &http.Client{Timeout: 5 * time.Second}
}

// Apply makes the services match enabled: writes the configuration and starts (or reloads)
// them, or stops them. Their data stays on disk when they are stopped.
func (m *Manager) Apply(ctx context.Context, enabled bool, alerting store.AlertingSettings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !enabled {
		var errs []error
		for _, u := range []string{PrometheusUnit, AlertmanagerUnit} {
			if out, err := m.Run(ctx, "systemctl", "stop", u); err != nil {
				errs = append(errs, fmt.Errorf("stop %s: %w (%s)", u, err, strings.TrimSpace(string(out))))
			}
		}
		return errors.Join(errs...)
	}
	if m.Instance == "" {
		// velod has its own UTS namespace: ask the host for its name.
		if out, err := m.Run(ctx, "hostname"); err == nil {
			m.Instance = strings.TrimSpace(string(out))
		}
	}
	token, err := m.NewToken()
	if err != nil {
		return fmt.Errorf("create the scrape token: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(m.Dir, "config"), 0o750); err != nil {
		return err
	}
	if err := writeShared(filepath.Join(m.Dir, "config", "scrape.token"), []byte(token)); err != nil {
		return err
	}
	if err := m.writeConfig(alerting); err != nil {
		return err
	}
	cpus := m.allowedCPUs(ctx)
	for _, u := range []string{AlertmanagerUnit, PrometheusUnit} {
		// restart, not start: picks up the new scrape token and config either way.
		if out, err := m.Run(ctx, "systemctl", "restart", u); err != nil {
			return fmt.Errorf("start %s: %w (%s)", u, err, strings.TrimSpace(string(out)))
		}
		if cpus != "" {
			// Keep off SPDK's polling cores (their reactors spin at 100%); until the next start.
			if out, err := m.Run(ctx, "systemctl", "set-property", "--runtime", u, "AllowedCPUs="+cpus); err != nil {
				m.logger().Warn("couldn't keep a monitoring service off SPDK's cores", "unit", u, "err", err, "out", strings.TrimSpace(string(out)))
			}
		}
	}
	m.logger().Info("built-in monitoring started", "prometheus", PrometheusAddr, "alertmanager", AlertmanagerAddr, "cpus", cpus)
	return nil
}

// Status reports the services' state.
func (m *Manager) Status(ctx context.Context, enabled bool) Status {
	st := Status{Enabled: enabled, RetentionDays: RetentionDays, RetentionBytes: RetentionBytes}
	st.Prometheus = m.service(ctx, PrometheusUnit, m.promAddr())
	st.Alertmanager = m.service(ctx, AlertmanagerUnit, m.amAddr())
	return st
}

func (m *Manager) service(ctx context.Context, unit, addr string) ServiceStatus {
	var s ServiceStatus
	out, err := m.Run(ctx, "systemctl", "is-active", unit)
	state := strings.TrimSpace(string(out))
	s.Active = err == nil && state == "active"
	if !s.Active {
		s.Detail = state
		return s
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/-/ready", nil)
	resp, err := m.client().Do(req)
	if err != nil {
		s.Detail = err.Error()
		return s
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	s.Ready = resp.StatusCode == http.StatusOK
	if !s.Ready {
		s.Detail = resp.Status
	}
	return s
}

// allowedCPUs lists the online CPUs SPDK doesn't poll on, in systemd's syntax ("1-3"), or ""
// to leave affinity alone (SPDK uses every CPU, or the cores can't be found).
func (m *Manager) allowedCPUs(ctx context.Context) string {
	if m.ReactorCPUs == nil {
		return ""
	}
	reactors, err := m.ReactorCPUs(ctx)
	if err != nil || len(reactors) == 0 {
		return ""
	}
	b, err := os.ReadFile("/sys/devices/system/cpu/online")
	if err != nil {
		return ""
	}
	online, err := parseCPUList(strings.TrimSpace(string(b)))
	if err != nil {
		return ""
	}
	busy := map[int]bool{}
	for _, c := range reactors {
		busy[c] = true
	}
	var free []int
	for _, c := range online {
		if !busy[c] {
			free = append(free, c)
		}
	}
	return formatCPUList(free)
}

// Reconfigure rewrites the rules and Alertmanager's configuration and reloads both services
// (no restart, same scrape token).
func (m *Manager) Reconfigure(ctx context.Context, alerting store.AlertingSettings) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.writeConfig(alerting); err != nil {
		return err
	}
	var errs []error
	for _, addr := range []string{m.promAddr(), m.amAddr()} {
		if err := m.do(ctx, http.MethodPost, addr, "/-/reload", nil, nil); err != nil {
			errs = append(errs, fmt.Errorf("reload %s: %w", addr, err))
		}
	}
	return errors.Join(errs...)
}

// writeConfig writes prometheus.yml, the rules and alertmanager.yml with its secret files.
func (m *Manager) writeConfig(alerting store.AlertingSettings) error {
	cfg := filepath.Join(m.Dir, "config")
	if err := os.MkdirAll(cfg, 0o750); err != nil {
		return err
	}
	ruleData, err := RenderRules(alerting.Rules)
	if err != nil {
		return err
	}
	am, secrets := alertmanagerConfig(alerting)
	files := map[string][]byte{
		"velocor-alerts.rules.yml": ruleData,
		"prometheus.yml":           []byte(m.prometheusConfig()),
		"alertmanager.yml":         []byte(am),
	}
	for name, data := range files {
		if err := writeShared(filepath.Join(cfg, name), data); err != nil {
			return err
		}
	}
	for _, name := range secretFiles {
		path := filepath.Join(cfg, name)
		if data, ok := secrets[name]; ok {
			if err := writeShared(path, []byte(data)); err != nil {
				return err
			}
		} else if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// writeShared writes a file the containers' user can read but not change (0640, group nobody).
func writeShared(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return err
	}
	if err := os.Chown(tmp, 0, fileGID); err != nil && !errors.Is(err, os.ErrPermission) {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func (m *Manager) prometheusConfig() string {
	scheme, host, path := "https", "127.0.0.1:8443", "/metrics"
	if u := m.MetricsURL; u != "" {
		if s, rest, ok := strings.Cut(u, "://"); ok {
			scheme = s
			host, path, _ = strings.Cut(rest, "/")
			path = "/" + path
		}
	}
	instance := m.Instance
	if instance == "" {
		instance = "velocor"
	}
	return fmt.Sprintf(`# Written by velod (built-in monitoring); changes are overwritten.
global:
  scrape_interval: 30s
  evaluation_interval: 30s
rule_files:
  - /etc/velocor-monitoring/velocor-alerts.rules.yml
alerting:
  alertmanagers:
    - static_configs:
        - targets: [%q]
scrape_configs:
  - job_name: velocor
    scheme: %s
    metrics_path: %s
    authorization:
      credentials_file: /etc/velocor-monitoring/scrape.token
    tls_config:
      # velod on the loopback interface, with whatever certificate it serves.
      insecure_skip_verify: true
    static_configs:
      - targets: [%q]
    relabel_configs:
      - target_label: instance
        replacement: %q
`, AlertmanagerAddr, scheme, path, host, instance)
}

// secretFiles are the receivers' secrets, kept out of alertmanager.yml.
var secretFiles = []string{"smtp.password", "webhook.url", "heartbeat.url"}

// alertmanagerConfig routes alerts by severity to the configured receivers, and the watchdog
// to the heartbeat URL. It returns the config and the secret files it refers to.
func alertmanagerConfig(a store.AlertingSettings) (string, map[string]string) {
	secrets := map[string]string{}
	var sb strings.Builder
	sb.WriteString("# Written by velod (built-in monitoring); changes are overwritten.\n")
	critical, warning := []string{}, []string{}
	receiver := func(min string, conf string) {
		critical = append(critical, conf)
		if min != "critical" {
			warning = append(warning, conf)
		}
	}
	if e := a.Email; e != nil && len(e.To) > 0 {
		var c strings.Builder
		fmt.Fprintf(&c, "      - to: %q\n        from: %q\n        smarthost: %q\n        require_tls: %t\n        send_resolved: true\n",
			strings.Join(e.To, ", "), e.From, net.JoinHostPort(e.SMTPHost, fmt.Sprint(e.SMTPPort)), e.RequireTLS)
		if e.Username != "" {
			fmt.Fprintf(&c, "        auth_username: %q\n", e.Username)
		}
		if e.Password != "" {
			secrets["smtp.password"] = e.Password
			c.WriteString("        auth_password_file: /etc/velocor-monitoring/smtp.password\n")
		}
		receiver(e.MinSeverity, "    email_configs:\n"+c.String())
	}
	if w := a.Webhook; w != nil && w.URL != "" {
		secrets["webhook.url"] = w.URL
		receiver(w.MinSeverity, "    webhook_configs:\n      - url_file: /etc/velocor-monitoring/webhook.url\n        send_resolved: true\n")
	}
	if a.HeartbeatURL != "" {
		secrets["heartbeat.url"] = a.HeartbeatURL
	}

	sb.WriteString("route:\n  receiver: none\n  group_by: [alertname, pool, disk, volume, export]\n  group_wait: 30s\n  group_interval: 5m\n  repeat_interval: 4h\n  routes:\n")
	if a.HeartbeatURL != "" {
		sb.WriteString("    - matchers: ['alertname=\"VelocorWatchdog\"']\n      receiver: heartbeat\n      group_wait: 0s\n      group_interval: 1m\n      repeat_interval: 5m\n")
	} else {
		sb.WriteString("    - matchers: ['alertname=\"VelocorWatchdog\"']\n      receiver: none\n")
	}
	if len(critical) > 0 {
		sb.WriteString("    - matchers: ['severity=\"critical\"']\n      receiver: critical\n")
	}
	if len(warning) > 0 {
		sb.WriteString("    - matchers: ['severity=\"warning\"']\n      receiver: warning\n")
	}
	sb.WriteString("receivers:\n  - name: none\n")
	if len(critical) > 0 {
		sb.WriteString("  - name: critical\n" + strings.Join(critical, ""))
	}
	if len(warning) > 0 {
		sb.WriteString("  - name: warning\n" + strings.Join(warning, ""))
	}
	if a.HeartbeatURL != "" {
		sb.WriteString("  - name: heartbeat\n    webhook_configs:\n      - url_file: /etc/velocor-monitoring/heartbeat.url\n        send_resolved: false\n")
	}
	return sb.String(), secrets
}

// parseCPUList parses the kernel's CPU list syntax ("0-3,8,10-11").
func parseCPUList(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		if part == "" {
			continue
		}
		lo, hi, isRange := strings.Cut(part, "-")
		var a, b int
		if _, err := fmt.Sscanf(lo, "%d", &a); err != nil {
			return nil, fmt.Errorf("bad CPU list %q", s)
		}
		b = a
		if isRange {
			if _, err := fmt.Sscanf(hi, "%d", &b); err != nil || b < a {
				return nil, fmt.Errorf("bad CPU list %q", s)
			}
		}
		for c := a; c <= b; c++ {
			out = append(out, c)
		}
	}
	return out, nil
}

// formatCPUList writes sorted CPUs in the same syntax.
func formatCPUList(cpus []int) string {
	var parts []string
	for i := 0; i < len(cpus); {
		j := i
		for j+1 < len(cpus) && cpus[j+1] == cpus[j]+1 {
			j++
		}
		if i == j {
			parts = append(parts, fmt.Sprint(cpus[i]))
		} else {
			parts = append(parts, fmt.Sprintf("%d-%d", cpus[i], cpus[j]))
		}
		i = j + 1
	}
	return strings.Join(parts, ",")
}

// LocalTarget turns velod's listen address into the address Prometheus scrapes on the
// loopback interface (":8443" → "127.0.0.1:8443").
func LocalTarget(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return "127.0.0.1:8443"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
