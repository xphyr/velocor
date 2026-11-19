package monitoring

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/store"
)

type fakeHost struct{ calls []string }

func (f *fakeHost) run(_ context.Context, name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	if len(args) > 0 && args[0] == "is-active" {
		return []byte("inactive\n"), os.ErrNotExist
	}
	return nil, nil
}

func TestApplyWritesConfigAndStartsServices(t *testing.T) {
	dir := t.TempDir()
	host := &fakeHost{}
	tokens := 0
	m := &Manager{Dir: dir, MetricsURL: "https://127.0.0.1:8443/metrics", Instance: "velocor-1", Run: host.run,
		NewToken: func() (string, error) { tokens++; return "velo_pat_secret", nil }}
	if err := m.Apply(context.Background(), true, store.AlertingSettings{}); err != nil {
		t.Fatal(err)
	}
	if tokens != 1 {
		t.Fatalf("NewToken called %d times", tokens)
	}
	want := []string{"systemctl restart " + AlertmanagerUnit, "systemctl restart " + PrometheusUnit}
	if !slices.Equal(host.calls, want) {
		t.Fatalf("calls %v, want %v", host.calls, want)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, "config", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	if read("scrape.token") != "velo_pat_secret" {
		t.Fatal("scrape token not written")
	}
	if !strings.Contains(read("velocor-alerts.rules.yml"), "alert: VelocorPoolDegraded") {
		t.Fatal("rules not written")
	}
	prom := read("prometheus.yml")
	for _, want := range []string{"job_name: velocor", `targets: ["127.0.0.1:8443"]`, "scheme: https", "metrics_path: /metrics",
		`replacement: "velocor-1"`, `targets: ["` + AlertmanagerAddr + `"]`, "credentials_file: /etc/velocor-monitoring/scrape.token"} {
		if !strings.Contains(prom, want) {
			t.Errorf("prometheus.yml lacks %q:\n%s", want, prom)
		}
	}
	if !strings.Contains(read("alertmanager.yml"), "receiver: none") {
		t.Error("alertmanager.yml")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "config", "scrape.token")); fi.Mode().Perm() != 0o640 {
		t.Errorf("token mode %v, want 0640", fi.Mode().Perm())
	}

	host.calls = nil
	if err := m.Apply(context.Background(), false, store.AlertingSettings{}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"systemctl stop " + PrometheusUnit, "systemctl stop " + AlertmanagerUnit}; !slices.Equal(host.calls, want) {
		t.Fatalf("disable: %v", host.calls)
	}
	st := m.Status(context.Background(), false)
	if st.Prometheus.Active || st.Prometheus.Detail != "inactive" || st.RetentionDays != 15 {
		t.Fatalf("status %+v", st)
	}
}

func TestCPULists(t *testing.T) {
	got, err := parseCPUList("0-3,8,10-11")
	if err != nil || !slices.Equal(got, []int{0, 1, 2, 3, 8, 10, 11}) {
		t.Fatalf("parse: %v %v", got, err)
	}
	if s := formatCPUList([]int{1, 2, 3, 8, 10, 11}); s != "1-3,8,10-11" {
		t.Fatalf("format: %s", s)
	}
	if _, err := parseCPUList("3-1"); err == nil {
		t.Fatal("a reversed range must fail")
	}
	for in, want := range map[string]string{":8443": "127.0.0.1:8443", "0.0.0.0:9443": "127.0.0.1:9443", "10.0.0.5:8443": "10.0.0.5:8443"} {
		if got := LocalTarget(in); got != want {
			t.Errorf("LocalTarget(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderRulesAppliesSettings(t *testing.T) {
	hot, wear := 60.0, 0.8
	settings := map[string]store.RuleSetting{
		"VelocorPoolSpaceWarning": {Disabled: true},
		"VelocorPoolDegraded":     {For: "15m"},
		"VelocorDiskHot":          {Threshold: &hot, For: "0s"},
		"VelocorDiskWornOut":      {Threshold: &wear},
	}
	if err := ValidateRuleSettings(settings); err != nil {
		t.Fatal(err)
	}
	out, err := RenderRules(settings)
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	if dump := os.Getenv("VELOCOR_RULES_DUMP"); dump != "" {
		_ = os.WriteFile(dump, out, 0o644)
	}
	for _, want := range []string{"velocor_disk_temperature_celsius > 60", "velocor_disk_wear_ratio >= 0.8", "alert: VelocorWatchdog"} {
		if !strings.Contains(text, want) {
			t.Errorf("rendered rules lack %q", want)
		}
	}
	if strings.Contains(text, "VelocorPoolSpaceWarning") {
		t.Error("a disabled rule is still there")
	}
	i := strings.Index(text, "alert: VelocorPoolDegraded")
	if i < 0 || !strings.Contains(text[i:i+400], "for: 15m") {
		t.Errorf("delay not applied:\n%s", text[i:i+400])
	}
	for _, bad := range []map[string]store.RuleSetting{
		{"Nope": {}},
		{"VelocorPoolDegraded": {For: "90s"}},
		{"VelocorPoolDegraded": {Threshold: &hot}},
		{"VelocorDiskHot": {Threshold: func() *float64 { v := 200.0; return &v }()}},
	} {
		if ValidateRuleSettings(bad) == nil {
			t.Errorf("accepted %v", bad)
		}
	}
	rules, _ := Rules(settings)
	if len(rules) != 24 {
		t.Errorf("%d rules listed, want 24 (the watchdog isn't tunable)", len(rules))
	}
}

func TestAlertmanagerConfigReceivers(t *testing.T) {
	conf, secrets := alertmanagerConfig(store.AlertingSettings{
		Email:        &store.EmailReceiver{To: []string{"ops@example.com", "oncall@example.com"}, From: "velocor@example.com", SMTPHost: "smtp.example.com", SMTPPort: 587, Username: "velocor", Password: "s3cret", RequireTLS: true},
		Webhook:      &store.WebhookReceiver{URL: "https://hooks.example.com/T0/abc", MinSeverity: "critical"},
		HeartbeatURL: "https://hc.example.com/ping/xyz",
	})
	if dump := os.Getenv("VELOCOR_AM_DUMP"); dump != "" {
		_ = os.WriteFile(dump, []byte(conf), 0o644)
	}
	if strings.Contains(conf, "s3cret") || strings.Contains(conf, "hooks.example.com") || strings.Contains(conf, "hc.example.com") {
		t.Fatalf("secrets in alertmanager.yml:\n%s", conf)
	}
	if secrets["smtp.password"] != "s3cret" || secrets["webhook.url"] == "" || secrets["heartbeat.url"] == "" {
		t.Fatalf("secrets %v", secrets)
	}
	crit := conf[strings.Index(conf, "  - name: critical"):]
	if !strings.Contains(crit, "email_configs") || !strings.Contains(crit, "webhook_configs") {
		t.Fatalf("critical receiver should have email and webhook:\n%s", conf)
	}
	w := conf[strings.Index(conf, "  - name: warning"):]
	if end := strings.Index(w[1:], "  - name:"); end > 0 {
		w = w[:end+1]
	}
	if !strings.Contains(w, "email_configs") || strings.Contains(w, "webhook_configs") {
		t.Fatalf("warning receiver should have only email (webhook is critical-only):\n%s", w)
	}
	if empty, s := alertmanagerConfig(store.AlertingSettings{}); strings.Contains(empty, "name: critical") || len(s) != 0 {
		t.Fatalf("no receivers: %s", empty)
	}
}
