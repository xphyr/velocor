package server

import (
	"context"
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
