package server

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/auth"
	"github.com/velocor/velocor/internal/reconciler"
	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

func TestServerEndToEndWorkflowAndCBT(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "server-test.db")
	st, err := store.OpenBoltStore(dbPath)
	if err != nil {
		t.Fatalf("OpenBoltStore failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	// Seed 2 disks (1 boot NVMe, 1 data NVMe).
	_ = st.UpsertDisk(&store.Disk{
		ID:           "nvme-boot",
		Name:         "nvme0n1",
		Type:         store.DiskTypeNVMe,
		PCIAddress:   "0000:00:04.0",
		SizeBytes:    64 << 30,
		IsBootDevice: true,
	})
	_ = st.UpsertDisk(&store.Disk{
		ID:         "nvme-data1",
		Name:       "nvme1n1",
		Type:       store.DiskTypeNVMe,
		PCIAddress: "0000:01:00.0",
		SizeBytes:  500 << 30,
	})

	// Mock SPDK JSON-RPC socket returning distinct fragmap bitmasks for snap1 and snap2.
	sockPath := filepath.Join(t.TempDir(), "spdk-srv.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer func() { _ = ln.Close() }()

	// snap1 allocates cluster 0 (0b00000001 = 0x01)
	// snap2 allocates cluster 1 (0b00000010 = 0x02)
	// snap3 allocates cluster 2 (0b00000100 = 0x04)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				var req struct {
					Method string         `json:"method"`
					Params map[string]any `json:"params"`
					ID     uint64         `json:"id"`
				}
				if err := json.NewDecoder(bufio.NewReader(c)).Decode(&req); err != nil {
					return
				}
				var res any
				switch req.Method {
				case "spdk_get_version":
					res = map[string]any{"version": "SPDK v24.09"}
				case "bdev_nvme_attach_controller":
					res = []string{"Nvme1n1"}
				case "bdev_lvol_create_lvstore":
					res = "lvs-uuid-1"
				case "bdev_lvol_create":
					res = "lvol-uuid-1"
				case "bdev_lvol_snapshot":
					res = "snap-uuid"
				case "bdev_lvol_get_fragmap":
					name, _ := req.Params["name"].(string)
					var mask byte = 0x01
					if strings.HasSuffix(name, "/s2") {
						mask = 0x02
					} else if strings.HasSuffix(name, "/s3") {
						mask = 0x04
					}
					res = spdk.FragmapResponse{
						ClusterSize: 65536,
						Offset:      0,
						Size:        8 * 65536,
						Bitmask:     base64.StdEncoding.EncodeToString([]byte{mask}),
					}
				default:
					res = true
				}
				_ = json.NewEncoder(c).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": res})
			}(conn)
		}
	}()

	spdkClient := spdk.NewClient(sockPath)
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

	// 1. Login as admin.
	code, loginResp := doJSON("POST", "/api/v1/auth/login", "", map[string]any{
		"username": "admin",
		"password": "admin-pass-123",
	})
	if code != http.StatusOK {
		t.Fatalf("login failed (%d): %v", code, loginResp)
	}
	adminJWT := loginResp["access_token"].(string)

	// 2. Claim boot disk must return 409 Conflict (boot guard).
	code, _ = doJSON("POST", "/api/v1/disks/nvme-boot:claim", adminJWT, nil)
	if code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when claiming boot disk, got %d", code)
	}

	// 3. Claim data disk and create pool.
	code, _ = doJSON("POST", "/api/v1/disks/nvme-data1:claim", adminJWT, nil)
	if code != http.StatusOK {
		t.Fatalf("claim data disk failed with %d", code)
	}

	code, poolResp := doJSON("POST", "/api/v1/pools", adminJWT, map[string]any{
		"name":       "pool0",
		"raid_level": "RAID0",
		"disk_ids":   []string{"nvme-data1"},
	})
	if code != http.StatusCreated {
		t.Fatalf("create pool failed (%d): %v", code, poolResp)
	}
	poolID := poolResp["pool"].(map[string]any)["id"].(string)

	// 4. Create a CSI provisioner service token (velo_pat_...) and use it for Volume + Snapshot + CBT operations.
	code, tokResp := doJSON("POST", "/api/v1/auth/tokens", adminJWT, map[string]any{
		"name": "csi-controller",
		"role": "CSI_PROVISIONER",
	})
	if code != http.StatusCreated {
		t.Fatalf("create token failed (%d): %v", code, tokResp)
	}
	csiPAT := tokResp["secret_token"].(string)

	// CSI token cannot delete pools (requires ADMIN).
	code, _ = doJSON("DELETE", "/api/v1/pools/"+poolID, csiPAT, nil)
	if code != http.StatusForbidden {
		t.Fatalf("expected 403 Forbidden when CSI token attempts pool delete, got %d", code)
	}

	// Create Volume (and verify idempotency on second call).
	volReq := map[string]any{
		"name":           "pvc-01",
		"pool_id":        poolID,
		"size_bytes":     1073741824,
		"thin_provision": true,
	}
	code, volResp := doJSON("POST", "/api/v1/volumes", csiPAT, volReq)
	if code != http.StatusCreated {
		t.Fatalf("create volume failed (%d): %v", code, volResp)
	}
	volID := volResp["volume"].(map[string]any)["id"].(string)

	code, volResp2 := doJSON("POST", "/api/v1/volumes", csiPAT, volReq)
	if code != http.StatusOK || volResp2["volume"].(map[string]any)["id"].(string) != volID {
		t.Fatalf("expected idempotent 200 OK with same volume ID on duplicate CreateVolume, got %d: %v", code, volResp2)
	}

	// 5. Create Snapshots s1, s2, s3 and query Changed Block Tracking (CBT) delta between s1 and s3!
	var snapIDs []string
	for _, sName := range []string{"s1", "s2", "s3"} {
		code, sResp := doJSON("POST", "/api/v1/volumes/"+volID+"/snapshots", csiPAT, map[string]any{"name": sName})
		if code != http.StatusCreated {
			t.Fatalf("create snapshot %s failed (%d): %v", sName, code, sResp)
		}
		snapIDs = append(snapIDs, sResp["snapshot"].(map[string]any)["id"].(string))
	}

	// Delta between s1 (base) and s3 (target) must coalesce s2 (cluster 1) + s3 (cluster 2) -> offset=65536, size=131072!
	code, cbtResp := doJSON("GET", "/api/v1/snapshots/"+snapIDs[2]+"/blocks:changed?base_snapshot_id="+snapIDs[0], csiPAT, nil)
	if code != http.StatusOK {
		t.Fatalf("GetSnapshotChangedBlocks failed (%d): %v", code, cbtResp)
	}
	extents := cbtResp["extents"].([]any)
	if len(extents) != 1 {
		t.Fatalf("expected 1 coalesced delta extent for (s2+s3), got %v", extents)
	}
	ext0 := extents[0].(map[string]any)
	if uint64(ext0["offset_bytes"].(float64)) != 65536 || uint64(ext0["size_bytes"].(float64)) != 2*65536 {
		t.Fatalf("expected delta extent offset=65536 size=131072, got %v", ext0)
	}
}

func TestServerEmbeddedWebUI(t *testing.T) {
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "ui-test.db"))
	if err != nil {
		t.Fatalf("OpenBoltStore failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	authMgr, err := auth.NewManager(st, "")
	if err != nil {
		t.Fatalf("auth.NewManager failed: %v", err)
	}
	srv := New(st, nil, nil, authMgr, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	for _, tc := range []struct {
		path string
		want string
	}{
		{"/", "VELOCOR"},
		{"/assets/styles.css", "--accent:"},
		{"/assets/app.js", "/api/v1/system/status"},
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

	authMgr, err := auth.NewManager(st, "")
	if err != nil {
		t.Fatalf("auth.NewManager failed: %v", err)
	}
	srv := New(st, nil, nil, authMgr, nil, nil)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	jwt, _, _, err := authMgr.Login("admin", auth.DefaultAdminPassword)
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
	nqns, iqns, cidrs := srv.resolveInitiatorGroupACLs(ig)
	if len(nqns) != 1 || len(iqns) != 1 || len(cidrs) != 1 || cidrs[0] != "10.0.2.20/32" {
		t.Fatalf("unexpected resolved ACLs: nqns=%v iqns=%v cidrs=%v", nqns, iqns, cidrs)
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
