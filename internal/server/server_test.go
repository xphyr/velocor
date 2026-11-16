package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/store"
)

func TestServerEmbeddedWebUI(t *testing.T) {
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "ui-test.db"))
	if err != nil {
		t.Fatalf("OpenBoltStore failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	authMgr, err := auth.NewManager(st, "bootstrap-pass-1")
	if err != nil {
		t.Fatalf("auth.NewManager failed: %v", err)
	}
	clearMustChange(t, st)
	srv := New(st, nil, nil, authMgr, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, tc := range []struct {
		path string
		want string
	}{
		{"/", "<title>Velocor</title>"},
		{"/assets/style.css", "--accent:"},
		{"/assets/favicon.svg", "<svg"},
		{"/assets/app.js", "\"/system/status\""},
	} {
		resp, err := http.Get(ts.URL + tc.path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", tc.path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s returned %d, want 200", tc.path, resp.StatusCode)
		}
		if !strings.Contains(string(body), tc.want) {
			t.Fatalf("GET %s body missing %q", tc.path, tc.want)
		}
	}
}

func TestServerHostsAndInitiatorGroupACLs(t *testing.T) {
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "hosts-test.db"))
	if err != nil {
		t.Fatalf("OpenBoltStore failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	authMgr, err := auth.NewManager(st, "bootstrap-pass-1")
	if err != nil {
		t.Fatalf("auth.NewManager failed: %v", err)
	}
	clearMustChange(t, st)
	srv := New(st, nil, nil, authMgr, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	jwt, _, _, err := authMgr.Login("admin", "bootstrap-pass-1")
	if err != nil {
		t.Fatalf("Login failed: %v", err)
	}
	doReq := func(method, path string, body any) (int, map[string]any) {
		var r io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			r = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, ts.URL+path, r)
		req.Header.Set("Authorization", "Bearer "+jwt)
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s failed: %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	code, hostResp := doReq("POST", "/api/v1/hosts", map[string]any{
		"name":          "worker-01",
		"host_nqn":      "nqn.2014-08.org.nvmexpress:uuid:worker-01",
		"initiator_iqn": "iqn.1994-05.com.redhat:worker-01",
		"ip_address":    "10.0.2.20",
	})
	if code != http.StatusCreated {
		t.Fatalf("create host failed (%d): %v", code, hostResp)
	}
	hostID := hostResp["host"].(map[string]any)["id"].(string)

	code, igResp := doReq("POST", "/api/v1/initiator-groups", map[string]any{
		"name":     "workers",
		"host_ids": []string{hostID},
	})
	if code != http.StatusCreated {
		t.Fatalf("create initiator group failed (%d): %v", code, igResp)
	}
	igID := igResp["initiator_group"].(map[string]any)["id"].(string)

	// Host cannot be deleted while referenced by an InitiatorGroup.
	if code, _ := doReq("DELETE", "/api/v1/hosts/"+hostID, nil); code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict deleting in-use host, got %d", code)
	}

	// Verify ACL resolution helper includes Host NQN, IQN, and IP/32.
	ig, err := st.GetInitiatorGroup(igID)
	if err != nil {
		t.Fatalf("GetInitiatorGroup failed: %v", err)
	}
	acl, err := dataplane.ResolveACL(st, []string{ig.ID})
	if err != nil || len(acl.HostNQNs) != 1 || len(acl.IQNs) != 1 || len(acl.CIDRs) != 1 || acl.CIDRs[0] != "10.0.2.20/32" {
		t.Fatalf("unexpected resolved ACLs: %+v (%v)", acl, err)
	}

	if code, _ := doReq("DELETE", "/api/v1/initiator-groups/"+igID, nil); code != http.StatusOK {
		t.Fatalf("delete initiator group failed: %d", code)
	}
	if code, _ := doReq("DELETE", "/api/v1/hosts/"+hostID, nil); code != http.StatusOK {
		t.Fatalf("delete host failed: %d", code)
	}
}

func TestServerUsersAndRBAC(t *testing.T) {
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "users-test.db"))
	if err != nil {
		t.Fatalf("OpenBoltStore failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	authMgr, err := auth.NewManager(st, "test-password-123")
	if err != nil {
		t.Fatalf("auth.NewManager failed: %v", err)
	}
	clearMustChange(t, st)
	srv := New(st, nil, nil, authMgr, nil, nil)
	handler := srv.Handler()

	doWithToken := func(tok, method, path string, body any) (int, map[string]any) {
		var rBody io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rBody = bytes.NewReader(b)
		}
		req := httptest.NewRequest(method, path, rBody)
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		var out map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &out)
		return w.Code, out
	}

	// Admin login
	code, loginResp := doWithToken("", "POST", "/api/v1/auth/login", map[string]string{
		"username": "admin",
		"password": "test-password-123",
	})
	if code != http.StatusOK {
		t.Fatalf("admin login failed (%d): %v", code, loginResp)
	}
	adminTok := loginResp["access_token"].(string)

	// Create READONLY user
	code, createResp := doWithToken(adminTok, "POST", "/api/v1/auth/users", map[string]any{
		"username": "viewer",
		"password": "viewer-pass-123",
		"role":     "READONLY",
	})
	if code != http.StatusCreated {
		t.Fatalf("create user failed (%d): %v", code, createResp)
	}

	// Login as viewer
	code, viewerLogin := doWithToken("", "POST", "/api/v1/auth/login", map[string]string{
		"username": "viewer",
		"password": "viewer-pass-123",
	})
	if code != http.StatusOK || viewerLogin["role"] != "READONLY" {
		t.Fatalf("viewer login failed (%d): %v", code, viewerLogin)
	}
	viewerTok := viewerLogin["access_token"].(string)

	// /api/v1/auth/me
	code, meResp := doWithToken(viewerTok, "GET", "/api/v1/auth/me", nil)
	if code != http.StatusOK || meResp["username"] != "viewer" || meResp["role"] != "READONLY" {
		t.Fatalf("unexpected /auth/me (%d): %v", code, meResp)
	}

	// READONLY allowed on GET, forbidden on POST/admin endpoints
	if code, _ := doWithToken(viewerTok, "GET", "/api/v1/pools", nil); code != http.StatusOK {
		t.Fatalf("expected 200 on GET /pools for READONLY, got %d", code)
	}
	if code, _ := doWithToken(viewerTok, "POST", "/api/v1/hosts", map[string]string{"name": "h1"}); code != http.StatusForbidden {
		t.Fatalf("expected 403 on POST /hosts for READONLY, got %d", code)
	}
	if code, _ := doWithToken(viewerTok, "GET", "/api/v1/auth/users", nil); code != http.StatusForbidden {
		t.Fatalf("expected 403 on GET /auth/users for READONLY, got %d", code)
	}

	// Viewer changes own password
	if code, resp := doWithToken(viewerTok, "POST", "/api/v1/auth/password", map[string]string{
		"current_password": "viewer-pass-123",
		"new_password":     "viewer-new-456",
	}); code != http.StatusOK {
		t.Fatalf("viewer password change failed (%d): %v", code, resp)
	}
	if code, _ := doWithToken("", "POST", "/api/v1/auth/login", map[string]string{
		"username": "viewer",
		"password": "viewer-new-456",
	}); code != http.StatusOK {
		t.Fatalf("viewer login with new password failed: %d", code)
	}

	// Admin cannot delete self or demote last admin
	if code, _ := doWithToken(adminTok, "DELETE", "/api/v1/auth/users/admin", nil); code != http.StatusBadRequest {
		t.Fatalf("expected 400 deleting self, got %d", code)
	}
	if code, _ := doWithToken(adminTok, "PATCH", "/api/v1/auth/users/admin", map[string]string{"role": "READONLY"}); code != http.StatusConflict {
		t.Fatalf("expected 409 demoting last admin, got %d", code)
	}

	// Admin updates viewer role and deletes viewer
	if code, resp := doWithToken(adminTok, "PATCH", "/api/v1/auth/users/viewer", map[string]string{"role": "CSI_PROVISIONER"}); code != http.StatusOK || resp["user"].(map[string]any)["role"] != "CSI_PROVISIONER" {
		t.Fatalf("update user role failed (%d): %v", code, resp)
	}
	if code, _ := doWithToken(adminTok, "DELETE", "/api/v1/auth/users/viewer", nil); code != http.StatusOK {
		t.Fatalf("delete user failed: %d", code)
	}
}

// clearMustChange lets tests use the bootstrap admin without the forced password change.
func clearMustChange(t *testing.T, st *store.BoltStore) {
	t.Helper()
	u, err := st.GetAdminUser("admin")
	if err != nil {
		t.Fatal(err)
	}
	u.MustChangePassword = false
	if err := st.SaveAdminUser(u); err != nil {
		t.Fatal(err)
	}
}
