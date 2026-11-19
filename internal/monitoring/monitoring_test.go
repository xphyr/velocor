package monitoring

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	if err := m.Apply(context.Background(), true); err != nil {
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
	if err := m.Apply(context.Background(), false); err != nil {
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
