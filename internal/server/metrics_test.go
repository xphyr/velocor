package server

import (
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
)

var (
	sampleRe = regexp.MustCompile(`^([a-zA-Z_:][a-zA-Z0-9_:]*)(\{[^}]*\})? (\S+)$`)
	metaRe   = regexp.MustCompile(`^# (HELP|TYPE) ([a-zA-Z_:][a-zA-Z0-9_:]*) (.*)$`)
)

// checkExposition fails on what strict Prometheus parsers reject: a sample before its family's
// TYPE, a family split into several runs, a repeated series, or a malformed line.
func checkExposition(t *testing.T, text string) map[string][]string {
	t.Helper()
	families := map[string][]string{}
	done := map[string]bool{}
	seen := map[string]bool{}
	current := ""
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		if mm := metaRe.FindStringSubmatch(line); mm != nil {
			if mm[2] != current {
				if done[mm[2]] {
					t.Errorf("line %d: family %s appears twice", i+1, mm[2])
				}
				if current != "" {
					done[current] = true
				}
				current = mm[2]
			}
			continue
		}
		mm := sampleRe.FindStringSubmatch(line)
		if mm == nil {
			t.Errorf("line %d malformed: %q", i+1, line)
			continue
		}
		name := mm[1]
		base := strings.TrimSuffix(name, "_total")
		if name != current && base != strings.TrimSuffix(current, "_total") {
			t.Errorf("line %d: sample of %s outside its family (current %s)", i+1, name, current)
		}
		if seen[name+mm[2]] {
			t.Errorf("line %d: series %s%s repeated", i+1, name, mm[2])
		}
		seen[name+mm[2]] = true
		families[name] = append(families[name], line)
	}
	return families
}

func TestMetricsForAlerting(t *testing.T) {
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	h.addDisk("d2", "0000:02:00.0")
	pool := h.pool("p1", "RAID1", "d1", "d2")
	v := h.volume(pool, "v1", 64*mib)
	gone := h.volume(pool, "gone", 32*mib)
	h.expect(h.do("DELETE", "/api/v1/volumes/"+gone["id"].(string), h.admin, nil))(http.StatusOK)
	h.reconcile()
	_ = v

	text := h.metrics()
	if out := os.Getenv("VELOCOR_METRICS_DUMP"); out != "" {
		_ = os.WriteFile(out, []byte(text), 0o644) // for promtool check metrics
	}
	fams := checkExposition(t, text)
	for _, want := range []string{
		`velocor_up 1`,
		`velocor_dataplane_up 1`,
		`velocor_pool_health{pool="p1",status="ONLINE"} 1`,
		`velocor_pool_members{pool="p1",state="ACTIVE"} 2`,
		`velocor_pool_members{pool="p1",state="FAILED"} 0`,
		`velocor_pool_metadata_copy_failed{pool="p1"} 0`,
		`velocor_pool_deleted_volumes{pool="p1"} 1`,
		`velocor_volume_health{volume="v1",pool="p1",status="ONLINE"} 1`,
		`velocor_disk_smart_critical_warning{disk="d1",type="NVME",pool="p1"} 0`,
		`velocor_reconciler_last_run_failed 0`,
		`velocor_spdk_unmanaged_objects{kind="LVOL"} 0`,
	} {
		if !strings.Contains(text, want+"\n") {
			t.Errorf("metrics lack %s", want)
		}
	}
	for _, fam := range []string{"velocor_pool_durability", "velocor_disk_health", "velocor_reconciler_last_run_timestamp_seconds",
		"velocor_pool_read_ops_total", "velocor_volume_written_bytes_total", "velocor_volume_read_seconds_total"} {
		if len(fams[fam]) == 0 {
			t.Errorf("no %s samples", fam)
		}
	}
	if strings.Contains(text, `volume="gone"`) {
		t.Error("a deleted volume must not have volume series (its space shows in the pool's)")
	}
}

func TestEscapeLabel(t *testing.T) {
	if got := escapeLabel("a\"b\\c\nd é"); got != `a\"b\\c\nd é` {
		t.Fatalf("escapeLabel: %q", got)
	}
	m := newMetricSet()
	m.gauge("x", "h", 0.5, "k", "v")
	m.gauge("y", "h", 3)
	m.gauge("x", "h", 1e20, "k", "w")
	var sb strings.Builder
	_ = m.writeTo(&sb)
	want := "# HELP x h\n# TYPE x gauge\nx{k=\"v\"} 0.5\nx{k=\"w\"} 1e+20\n# HELP y h\n# TYPE y gauge\ny 3\n"
	if sb.String() != want {
		t.Fatalf("grouping/format:\n%s\nwant\n%s", sb.String(), want)
	}
}

// Every velocor_* metric the shipped alert rules use must be one velod exports, so a renamed
// metric can't silently disable an alert. Metrics that only appear in some states (a rebuild
// in progress, backups taken) are checked against the exporter's source instead.
func TestAlertRulesUseExportedMetrics(t *testing.T) {
	rules, err := os.ReadFile("../../deploy/monitoring/velocor-alerts.rules.yml")
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("metrics.go")
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	h.pool("p1", "RAID0", "d1")
	h.reconcile()
	text := h.metrics()
	used := regexp.MustCompile(`velocor_[a-z_]+`).FindAllString(string(rules), -1)
	if len(used) < 10 {
		t.Fatalf("found only %d metric names in the rules", len(used))
	}
	for _, name := range used {
		if strings.Contains(text, "# TYPE "+name+" ") || strings.Contains(string(src), `"`+name+`"`) {
			continue
		}
		t.Errorf("alert rules use %s, which velod doesn't export", name)
	}
}
