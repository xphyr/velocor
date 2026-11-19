package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/backup"
	"github.com/velocor/velocor/internal/reconciler"
	"github.com/velocor/velocor/internal/spdk/spdktest"
	"github.com/velocor/velocor/internal/store"
)

func TestBackupDownloadAndRestore(t *testing.T) {
	dir := t.TempDir()
	db := filepath.Join(dir, "velocor.db")
	st, err := store.OpenBoltStore(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	f := spdktest.New(t)
	authMgr, err := auth.NewManager(st, "admin-pass-123")
	if err != nil {
		t.Fatal(err)
	}
	clearMustChange(t, st)
	mgr := &backup.Manager{Dir: filepath.Join(dir, "backups"), DBPath: db, Source: st}
	restarted := make(chan struct{}, 1)
	srv := New(st, f.Client(), nil, authMgr, reconciler.New(st, f.Client(), nil, nil), nil,
		WithBackups(mgr, func() { restarted <- struct{}{} }))
	h := &harness{t: t, st: st, fake: f, ts: httptest.NewServer(srv.Handler())}
	t.Cleanup(h.ts.Close)
	_, resp := h.do("POST", "/api/v1/auth/login", "", map[string]any{"username": "admin", "password": "admin-pass-123"})
	h.admin = resp["access_token"].(string)

	raw := func(method, path string, body []byte) (int, []byte) {
		t.Helper()
		req, _ := http.NewRequest(method, h.ts.URL+path, bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+h.admin)
		r, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = r.Body.Close() }()
		b, _ := io.ReadAll(r.Body)
		return r.StatusCode, b
	}

	code, dump := raw("GET", "/api/v1/system/backup", nil)
	if code != http.StatusOK || len(dump) < 4096 {
		t.Fatalf("download: %d, %d bytes", code, len(dump))
	}
	if code, _ := h.do("POST", "/api/v1/system/backups", h.admin, nil); code != http.StatusCreated {
		t.Fatalf("take backup: %d", code)
	}
	if code, out := h.do("GET", "/api/v1/system/backups", h.admin, nil); code != http.StatusOK || len(out["backups"].([]any)) != 1 {
		t.Fatalf("list: %d %v", code, out)
	}
	if code, _ := raw("POST", restorePath, []byte("garbage")); code != http.StatusBadRequest {
		t.Fatalf("garbage restore: %d", code)
	}
	if code, b := raw("POST", restorePath, dump); code != http.StatusAccepted {
		t.Fatalf("restore: %d %s", code, b)
	}
	select {
	case <-restarted:
	case <-time.After(5 * time.Second):
		t.Fatal("velod was not restarted after the restore")
	}
	if _, err := os.Stat(backup.PendingRestorePath(db)); err != nil {
		t.Fatalf("restore not staged: %v", err)
	}

	// Read-only users may not download the database.
	tok := h.expect(h.do("POST", "/api/v1/auth/tokens", h.admin, map[string]any{"name": "ro", "role": "READONLY"}))(http.StatusCreated)
	ro := tok["secret_token"].(string)
	if code, _ := h.do("GET", "/api/v1/system/backups", ro, nil); code != http.StatusForbidden {
		t.Fatalf("read-only list: %d", code)
	}
}
