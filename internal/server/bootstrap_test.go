package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/store"
)

// The generated bootstrap password file goes away once the admin password is changed: at the
// change, and at startup for one changed before.
func TestBootstrapPasswordFileDeletedAfterChange(t *testing.T) {
	dir := t.TempDir()
	st, err := store.OpenBoltStore(filepath.Join(dir, "velocor.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	authMgr, err := auth.NewManager(st, "bootstrap-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	pw := filepath.Join(dir, "initial-admin-password")
	if err := os.WriteFile(pw, []byte("bootstrap-pass-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := New(st, nil, nil, authMgr, nil, nil, WithBootstrapPasswordFile(pw))
	if _, err := os.Stat(pw); err != nil {
		t.Fatalf("the file went away before the password was changed: %v", err)
	}
	h := &harness{t: t, st: st, ts: httptest.NewServer(srv.Handler())}
	t.Cleanup(h.ts.Close)
	_, resp := h.do("POST", "/api/v1/auth/login", "", map[string]any{"username": "admin", "password": "bootstrap-pass-1"})
	tok := resp["access_token"].(string)
	h.expect(h.do("POST", "/api/v1/auth/password", tok, map[string]any{"current_password": "bootstrap-pass-1", "new_password": "a-new-password-2"}))(http.StatusOK)
	if _, err := os.Stat(pw); !os.IsNotExist(err) {
		t.Fatalf("the file is still there after the change: %v", err)
	}

	// A file left over from before: removed when velod starts.
	if err := os.WriteFile(pw, []byte("bootstrap-pass-1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	New(st, nil, nil, authMgr, nil, nil, WithBootstrapPasswordFile(pw))
	if _, err := os.Stat(pw); !os.IsNotExist(err) {
		t.Fatalf("a stale file survived startup: %v", err)
	}
}
