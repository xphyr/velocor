package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/monitoring"
	"github.com/velocor/velocor/internal/reconciler"
	"github.com/velocor/velocor/internal/spdk/spdktest"
	"github.com/velocor/velocor/internal/store"
)

func TestMonitoringSetting(t *testing.T) {
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "srv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := spdktest.New(t)
	authMgr, _ := auth.NewManager(st, "admin-pass-123")
	clearMustChange(t, st)
	var mu sync.Mutex
	var calls []string
	mon := &monitoring.Manager{Dir: t.TempDir(), Instance: "test", Run: func(_ context.Context, name string, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil, nil
	}}
	srv := New(st, f.Client(), nil, authMgr, reconciler.New(st, f.Client(), nil, nil), nil, WithMonitoring(mon))
	h := &harness{t: t, st: st, fake: f, ts: httptest.NewServer(srv.Handler())}
	t.Cleanup(h.ts.Close)
	_, resp := h.do("POST", "/api/v1/auth/login", "", map[string]any{"username": "admin", "password": "admin-pass-123"})
	h.admin = resp["access_token"].(string)

	got := h.expect(h.do("GET", "/api/v1/system/monitoring", h.admin, nil))(http.StatusOK)
	if got["available"] != true || got["monitoring"].(map[string]any)["enabled"] != false {
		t.Fatalf("default: %v", got)
	}
	h.expect(h.do("PUT", "/api/v1/system/monitoring", h.csiToken(), map[string]any{"enabled": true}))(http.StatusForbidden)
	h.expect(h.do("PUT", "/api/v1/system/monitoring", h.admin, map[string]any{}))(http.StatusBadRequest)
	h.expect(h.do("PUT", "/api/v1/system/monitoring", h.admin, map[string]any{"enabled": true}))(http.StatusOK)
	if s, _ := st.GetSettings(); !s.MonitoringEnabled {
		t.Fatal("setting not saved")
	}
	mu.Lock()
	joined := strings.Join(calls, "\n")
	mu.Unlock()
	if !strings.Contains(joined, "systemctl restart "+monitoring.PrometheusUnit) {
		t.Fatalf("services not started: %s", joined)
	}
	// One scrape token, replaced (not added to) when the configuration is applied again.
	h.expect(h.do("PUT", "/api/v1/system/monitoring", h.admin, map[string]any{"enabled": true}))(http.StatusOK)
	tokens, _ := st.ListApiTokens()
	n := 0
	for _, tk := range tokens {
		if tk.Name == monitoring.ScrapeTokenName {
			n++
			if tk.Role != store.RoleReadOnly {
				t.Fatalf("scrape token role %s", tk.Role)
			}
		}
	}
	if n != 1 {
		t.Fatalf("%d scrape tokens, want 1", n)
	}
	h.expect(h.do("PUT", "/api/v1/system/monitoring", h.admin, map[string]any{"enabled": false}))(http.StatusOK)
	if s, _ := st.GetSettings(); s.MonitoringEnabled {
		t.Fatal("setting not cleared")
	}
}

func TestNoAlertingWarning(t *testing.T) {
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "w.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := &Server{store: st, startedAt: time.Now()}
	if w := s.monitoringWarning(); w != "" {
		t.Fatalf("warned right after start: %s", w)
	}
	s.startedAt = time.Now().Add(-time.Hour)
	if w := s.monitoringWarning(); !strings.Contains(w, "No alerts reach anyone") {
		t.Fatalf("no warning after an hour without scrapes: %q", w)
	}
	s.mon.lastExternalScrape.Store(time.Now().UnixNano())
	if w := s.monitoringWarning(); w != "" {
		t.Fatalf("warned although /metrics was just scraped: %s", w)
	}
	s.mon.lastExternalScrape.Store(time.Now().Add(-time.Hour).UnixNano())
	if err := st.SaveSettings(&store.Settings{MonitoringEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if w := s.monitoringWarning(); w != "" {
		t.Fatalf("warned with built-in monitoring on: %s", w)
	}
}

func TestAlertsSilencesAndGraphs(t *testing.T) {
	var mu sync.Mutex
	var queries []string
	silences := map[string]bool{}
	am := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/api/v2/alerts":
			_, _ = w.Write([]byte(`[
			  {"labels":{"alertname":"VelocorWatchdog","severity":"none"},"annotations":{},"startsAt":"2026-11-19T10:00:00Z","status":{"state":"active"}},
			  {"labels":{"alertname":"VelocorPoolSpaceWarning","severity":"warning","pool":"p1","job":"velocor"},"annotations":{"summary":"Pool p1 is filling up"},"startsAt":"2026-11-19T10:00:00Z","status":{"state":"active"}},
			  {"labels":{"alertname":"VelocorPoolDegraded","severity":"critical","pool":"p2"},"annotations":{"summary":"Pool p2 is degraded"},"startsAt":"2026-11-19T11:00:00Z","status":{"state":"active"}}]`))
		case r.Method == "GET" && r.URL.Path == "/api/v2/silences":
			out := "["
			for id := range silences {
				out += `{"id":"` + id + `","matchers":[{"name":"pool","value":"p1","isRegex":false}],"startsAt":"2026-11-19T10:00:00Z","endsAt":"2026-11-19T12:00:00Z","createdBy":"admin","comment":"","status":{"state":"active"}}`
			}
			_, _ = w.Write([]byte(out + "]"))
		case r.Method == "POST" && r.URL.Path == "/api/v2/silences":
			silences["s1"] = true
			_, _ = w.Write([]byte(`{"silenceID":"s1"}`))
		case r.Method == "DELETE" && r.URL.Path == "/api/v2/silence/s1":
			delete(silences, "s1")
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(am.Close)
	prom := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.Query().Get("query"))
		mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[{"metric":{},"values":[[1795000000,"0.5"],[1795000030,"0.6"]]}]}}`))
	}))
	t.Cleanup(prom.Close)

	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "srv.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := spdktest.New(t)
	authMgr, _ := auth.NewManager(st, "admin-pass-123")
	clearMustChange(t, st)
	mon := &monitoring.Manager{Dir: t.TempDir(), Instance: "test",
		PrometheusAddr: strings.TrimPrefix(prom.URL, "http://"), AlertmanagerAddr: strings.TrimPrefix(am.URL, "http://"),
		Run: func(context.Context, string, ...string) ([]byte, error) { return nil, nil }}
	srv := New(st, f.Client(), nil, authMgr, reconciler.New(st, f.Client(), nil, nil), nil, WithMonitoring(mon))
	h := &harness{t: t, st: st, fake: f, ts: httptest.NewServer(srv.Handler())}
	t.Cleanup(h.ts.Close)
	_, resp := h.do("POST", "/api/v1/auth/login", "", map[string]any{"username": "admin", "password": "admin-pass-123"})
	h.admin = resp["access_token"].(string)

	h.expect(h.do("GET", "/api/v1/monitoring/alerts", h.admin, nil))(http.StatusConflict) // off
	h.expect(h.do("PUT", "/api/v1/system/monitoring", h.admin, map[string]any{"enabled": true}))(http.StatusOK)

	got := h.expect(h.do("GET", "/api/v1/monitoring/alerts", h.admin, nil))(http.StatusOK)
	alerts := got["alerts"].([]any)
	if len(alerts) != 2 || alerts[0].(map[string]any)["name"] != "VelocorPoolDegraded" {
		t.Fatalf("alerts (watchdog hidden, critical first): %v", alerts)
	}
	if l := alerts[1].(map[string]any)["labels"].(map[string]any); l["pool"] != "p1" || l["job"] != nil {
		t.Fatalf("labels: %v", l)
	}

	h.expect(h.do("POST", "/api/v1/monitoring/silences", h.csiToken(), map[string]any{"matchers": []map[string]string{{"name": "pool", "value": "p1"}}, "duration_hours": 2}))(http.StatusForbidden)
	h.expect(h.do("POST", "/api/v1/monitoring/silences", h.admin, map[string]any{"matchers": []map[string]string{}, "duration_hours": 2}))(http.StatusBadRequest)
	h.expect(h.do("POST", "/api/v1/monitoring/silences", h.admin, map[string]any{"matchers": []map[string]string{{"name": "pool", "value": "p1"}}, "duration_hours": 2, "comment": "disk swap"}))(http.StatusCreated)
	if s := h.expect(h.do("GET", "/api/v1/monitoring/alerts", h.admin, nil))(http.StatusOK)["silences"].([]any); len(s) != 1 {
		t.Fatalf("silences: %v", s)
	}
	h.expect(h.do("DELETE", "/api/v1/monitoring/silences/s1", h.admin, nil))(http.StatusOK)

	g := h.expect(h.do("GET", `/api/v1/monitoring/graphs/pool-latency?pool=we%22ird&range=24h`, h.admin, nil))(http.StatusOK)
	if g["unit"] != "seconds" || len(g["lines"].([]any)) != 2 {
		t.Fatalf("graph: %v", g)
	}
	mu.Lock()
	q := queries[0]
	mu.Unlock()
	if !strings.Contains(q, `pool="we\"ird"`) || !strings.Contains(q, "[1440s]") {
		t.Fatalf("query %q: want the escaped selector and a 24-minute window (4 steps of 24h/240)", q)
	}
	h.expect(h.do("GET", "/api/v1/monitoring/graphs/pool-latency", h.admin, nil))(http.StatusBadRequest)
	h.expect(h.do("GET", "/api/v1/monitoring/graphs/nope?pool=p1", h.admin, nil))(http.StatusNotFound)
	h.expect(h.do("GET", "/api/v1/monitoring/graphs/pool-iops?pool=p1&range=3y", h.admin, nil))(http.StatusBadRequest)
}

func TestAlertingSettingsKeepSecrets(t *testing.T) {
	h := newHarness(t)
	put := func(body map[string]any) map[string]any {
		return h.expect(h.do("PUT", "/api/v1/monitoring/settings", h.admin, body))(http.StatusOK)["alerting"].(map[string]any)
	}
	email := map[string]any{"to": []string{"ops@example.com"}, "from": "velocor@example.com", "smtp_host": "smtp.example.com", "password": "s3cret"}
	got := put(map[string]any{"email": email, "webhook": map[string]any{"url": "https://hooks.example.com/T/abc", "min_severity": "critical"},
		"heartbeat_url": "https://hc.example.com/ping/x"})
	raw := h.metrics() + func() string { b, _ := jsonString(got); return b }()
	if strings.Contains(raw, "s3cret") || strings.Contains(raw, "/T/abc") || strings.Contains(raw, "/ping/x") {
		t.Fatalf("a secret came back: %v", got)
	}
	e := got["email"].(map[string]any)
	if e["password_set"] != true || e["smtp_port"] != float64(587) || e["require_tls"] != true || e["min_severity"] != "warning" {
		t.Fatalf("email view: %v", e)
	}
	if wh := got["webhook"].(map[string]any); wh["url_set"] != true || wh["url_host"] != "hooks.example.com" {
		t.Fatalf("webhook view: %v", wh)
	}

	// Leaving the password out keeps it; "" clears it; null removes a receiver.
	delete(email, "password")
	email["to"] = []string{"ops@example.com", "oncall@example.com"}
	put(map[string]any{"email": email})
	if s, _ := h.st.GetSettings(); s.Alerting.Email.Password != "s3cret" || len(s.Alerting.Email.To) != 2 || s.Alerting.Webhook == nil {
		t.Fatalf("after an update without the password: %+v", s.Alerting)
	}
	email["password"] = ""
	put(map[string]any{"email": email, "webhook": nil})
	if s, _ := h.st.GetSettings(); s.Alerting.Email.Password != "" || s.Alerting.Webhook != nil || s.Alerting.HeartbeatURL == "" {
		t.Fatalf("after clearing: %+v", s.Alerting)
	}

	for _, bad := range []map[string]any{
		{"email": map[string]any{"to": []string{"Ops <ops@example.com>"}, "from": "a@b.c", "smtp_host": "h"}},
		{"email": map[string]any{"to": []string{"ops@example.com"}, "from": "a@b.c", "smtp_host": "h", "smtp_port": 70000}},
		{"webhook": map[string]any{"url": "ftp://x/y"}},
		{"heartbeat_url": "not a url"},
		{"rules": map[string]any{"VelocorPoolDegraded": map[string]any{"for": "30s"}}},
		{"rules": map[string]any{"NoSuchRule": map[string]any{"disabled": true}}},
	} {
		h.expect(h.do("PUT", "/api/v1/monitoring/settings", h.admin, bad))(http.StatusBadRequest)
	}
	put(map[string]any{"rules": map[string]any{"VelocorDiskHot": map[string]any{"threshold": 65}, "VelocorPoolSpaceWarning": map[string]any{"disabled": true}}})
	rules := h.expect(h.do("GET", "/api/v1/monitoring/rules", h.admin, nil))(http.StatusOK)["rules"].([]any)
	found := false
	for _, r := range rules {
		r := r.(map[string]any)
		if r["name"] == "VelocorDiskHot" {
			found = r["setting"].(map[string]any)["threshold"] == float64(65) && r["tunable"].(map[string]any)["default"] == float64(70)
		}
	}
	if !found || len(rules) != 24 {
		t.Fatalf("rules: %d, VelocorDiskHot setting shown: %v", len(rules), found)
	}
	h.expect(h.do("GET", "/api/v1/monitoring/settings", h.csiToken(), nil))(http.StatusForbidden)
	h.expect(h.do("POST", "/api/v1/monitoring/test", h.admin, nil))(http.StatusBadRequest) // no built-in monitoring here
}

func jsonString(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
