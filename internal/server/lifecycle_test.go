package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/reconciler"
	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

func TestLifecycleEndpointsAndRBAC(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "lifecycle-test.db")
	st, err := store.OpenBoltStore(dbPath)
	if err != nil {
		t.Fatalf("OpenBoltStore failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	spdkClient := spdk.NewClient(filepath.Join(t.TempDir(), "unused.sock"))
	authMgr, err := auth.NewManager(st, "admin-pass-123")
	if err != nil {
		t.Fatalf("auth.NewManager failed: %v", err)
	}
	rec := reconciler.New(st, spdkClient, nil, nil)
	srv := New(st, spdkClient, nil, authMgr, rec, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	doJSON := func(method, path, token string, body any) (int, map[string]any) {
		t.Helper()
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, err := http.NewRequest(method, ts.URL+path, rdr)
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("Do(%s %s): %v", method, path, err)
		}
		defer func() { _ = resp.Body.Close() }()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		return resp.StatusCode, out
	}

	// Unauthenticated requests must be rejected.
	if code, _ := doJSON("GET", "/api/v1/system/os", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated GET /system/os, got %d", code)
	}
	if code, _ := doJSON("GET", "/api/v1/system/containers/updates", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated GET /system/containers/updates, got %d", code)
	}

	// Login as admin.
	code, loginResp := doJSON("POST", "/api/v1/auth/login", "", map[string]string{
		"username": "admin",
		"password": "admin-pass-123",
	})
	if code != http.StatusOK {
		t.Fatalf("admin login failed (%d): %v", code, loginResp)
	}
	adminTok := loginResp["access_token"].(string)

	// Issue a READONLY token to verify RBAC on mutating lifecycle endpoints.
	code, tokResp := doJSON("POST", "/api/v1/auth/tokens", adminTok, map[string]any{
		"name": "viewer",
		"role": "READONLY",
	})
	if code != http.StatusCreated {
		t.Fatalf("create readonly token failed (%d): %v", code, tokResp)
	}
	roTok := tokResp["secret_token"].(string)

	// GET /api/v1/system/os succeeds cleanly even without bootc installed.
	code, osResp := doJSON("GET", "/api/v1/system/os", roTok, nil)
	if code != http.StatusOK {
		t.Fatalf("expected 200 on GET /system/os, got %d: %v", code, osResp)
	}
	if _, ok := osResp["available"]; !ok {
		t.Fatalf("expected 'available' key in GET /system/os response: %v", osResp)
	}

	// GET /api/v1/system/containers/updates succeeds cleanly even without podman installed.
	code, cuResp := doJSON("GET", "/api/v1/system/containers/updates", roTok, nil)
	if code != http.StatusOK {
		t.Fatalf("expected 200 on GET /system/containers/updates, got %d: %v", code, cuResp)
	}
	if _, ok := cuResp["available"]; !ok {
		t.Fatalf("expected 'available' key in GET /system/containers/updates response: %v", cuResp)
	}
	if _, ok := cuResp["containers"]; !ok {
		t.Fatalf("expected 'containers' key in GET /system/containers/updates response: %v", cuResp)
	}

	// READONLY cannot trigger OS reboot or container updates.
	if code, _ := doJSON("POST", "/api/v1/system/reboot", roTok, map[string]any{}); code != http.StatusForbidden {
		t.Fatalf("expected 403 on POST /system/reboot for READONLY, got %d", code)
	}
	if code, _ := doJSON("POST", "/api/v1/system/containers/update", roTok, map[string]any{}); code != http.StatusForbidden {
		t.Fatalf("expected 403 on POST /system/containers/update for READONLY, got %d", code)
	}
	if code, _ := doJSON("PUT", "/api/v1/system/network/interfaces/eth0", roTok, map[string]any{"method": "dhcp"}); code != http.StatusForbidden {
		t.Fatalf("expected 403 on PUT /system/network/interfaces/eth0 for READONLY, got %d", code)
	}

	// GET /api/v1/system/network returns host interfaces JSON cleanly.
	code, netResp := doJSON("GET", "/api/v1/system/network", roTok, nil)
	if code != http.StatusOK {
		t.Fatalf("expected 200 on GET /system/network, got %d: %v", code, netResp)
	}
	if _, ok := netResp["interfaces"]; !ok {
		t.Fatalf("expected 'interfaces' key in GET /system/network response: %v", netResp)
	}

	// Validation checks for PUT /api/v1/system/network/interfaces/{name} as ADMIN.
	if code, _ := doJSON("PUT", "/api/v1/system/network/interfaces/lo", adminTok, map[string]any{"method": "dhcp"}); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for protected loopback interface, got %d", code)
	}
	if code, _ := doJSON("PUT", "/api/v1/system/network/interfaces/eth0", adminTok, map[string]any{
		"method":  "manual",
		"address": "not-a-cidr",
	}); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid CIDR, got %d", code)
	}
	if code, _ := doJSON("PUT", "/api/v1/system/network/interfaces/eth0", adminTok, map[string]any{
		"method": "dhcp",
		"mtu":    100,
	}); code != http.StatusBadRequest {
		t.Fatalf("expected 400 for invalid MTU, got %d", code)
	}
}

func TestLoadOrGenerateSelfSignedTLSConfigPersists(t *testing.T) {
	dir := t.TempDir()
	certPath := filepath.Join(dir, "tls.crt")
	keyPath := filepath.Join(dir, "tls.key")

	cfg1, err := LoadOrGenerateSelfSignedTLSConfig(certPath, keyPath)
	if err != nil {
		t.Fatalf("first LoadOrGenerateSelfSignedTLSConfig failed: %v", err)
	}
	cfg2, err := LoadOrGenerateSelfSignedTLSConfig(certPath, keyPath)
	if err != nil {
		t.Fatalf("second LoadOrGenerateSelfSignedTLSConfig failed: %v", err)
	}
	if !bytes.Equal(cfg1.Certificates[0].Certificate[0], cfg2.Certificates[0].Certificate[0]) {
		t.Fatalf("expected persisted TLS certificate to be reused across reloads")
	}
}
