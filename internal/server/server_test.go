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
