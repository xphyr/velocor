package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/store"
)

func TestAuthHardening(t *testing.T) {
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	authMgr, err := auth.NewManager(st, "bootstrap-pass-1")
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, st: st, ts: httptest.NewServer(New(st, nil, nil, authMgr, nil, nil).Handler())}
	defer h.ts.Close()

	// Unauthenticated /metrics is refused by default.
	if code, _ := h.do("GET", "/metrics", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("/metrics without a token returned %d", code)
	}

	// Bootstrap admin: everything except changing the password is blocked.
	login := h.expect(h.do("POST", "/api/v1/auth/login", "", map[string]any{"username": "admin", "password": "bootstrap-pass-1"}))(http.StatusOK)
	if login["must_change_password"] != true {
		t.Fatalf("login must report must_change_password: %v", login)
	}
	tok := login["access_token"].(string)
	if code, _ := h.do("GET", "/api/v1/pools", tok, nil); code != http.StatusForbidden {
		t.Fatalf("API use before password change returned %d", code)
	}
	changed := h.expect(h.do("POST", "/api/v1/auth/password", tok, map[string]any{"current_password": "bootstrap-pass-1", "new_password": "a-real-password"}))(http.StatusOK)
	if code, _ := h.do("GET", "/api/v1/pools", tok, nil); code != http.StatusUnauthorized {
		t.Fatalf("pre-change session must be revoked, got %d", code)
	}
	fresh := changed["access_token"].(string)
	h.expect(h.do("GET", "/api/v1/pools", fresh, nil))(http.StatusOK)
	h.expect(h.do("GET", "/metrics", fresh, nil))(http.StatusOK)

	// Brute force is throttled per (user, IP)...
	for i := 0; i < loginMaxPerUser; i++ {
		h.expect(h.do("POST", "/api/v1/auth/login", "", map[string]any{"username": "admin", "password": "wrong-guess"}))(http.StatusUnauthorized)
	}
	if code, _ := h.do("POST", "/api/v1/auth/login", "", map[string]any{"username": "admin", "password": "a-real-password"}); code != http.StatusTooManyRequests {
		t.Fatalf("login after %d failures returned %d, want 429", loginMaxPerUser, code)
	}
}
