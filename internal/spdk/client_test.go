package spdk

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"path/filepath"
	"testing"
)

type mockRPCHandler func(method string, params map[string]any) (any, *RPCError)

func startMockSPDKServer(t *testing.T, handler mockRPCHandler) *Client {
	t.Helper()
	sockPath := filepath.Join(t.TempDir(), "spdk-test.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix %s: %v", sockPath, err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				var req struct {
					JSONRPC string         `json:"jsonrpc"`
					Method  string         `json:"method"`
					Params  map[string]any `json:"params"`
					ID      uint64         `json:"id"`
				}
				if err := json.NewDecoder(bufio.NewReader(c)).Decode(&req); err != nil {
					return
				}
				resVal, rpcErr := handler(req.Method, req.Params)
				resp := map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
				}
				if rpcErr != nil {
					resp["error"] = rpcErr
				} else {
					resp["result"] = resVal
				}
				_ = json.NewEncoder(c).Encode(resp)
			}(conn)
		}
	}()

	return NewClient(sockPath)
}

func TestSPDKClientTypedRPCs(t *testing.T) {
	// Bitmask 0b00110111 (0x37) -> clusters 0,1,2 allocated (3 clusters), cluster 3 free, clusters 4,5 allocated (2 clusters)
	bitmaskB64 := base64.StdEncoding.EncodeToString([]byte{0x37})

	client := startMockSPDKServer(t, func(method string, params map[string]any) (any, *RPCError) {
		switch method {
		case "spdk_get_version":
			return map[string]any{"version": "SPDK v24.09"}, nil
		case "bdev_nvme_attach_controller":
			if params["traddr"] == "0000:01:00.0" {
				return []string{"Nvme1n1"}, nil
			}
			return nil, &RPCError{Code: -19, Message: "No such device"}
		case "bdev_nvme_get_controller_health_info":
			return NvmeHealthInfo{
				TemperatureCelsius: 38,
				AvailableSpare:     100,
				PercentageUsed:     1,
				PowerOnHours:       240,
			}, nil
		case "bdev_uring_create":
			return params["name"], nil
		case "bdev_raid_create":
			return true, nil
		case "bdev_lvol_create_lvstore":
			return "lvs-uuid-1234", nil
		case "bdev_lvol_create":
			return "lvol-uuid-5678", nil
		case "bdev_lvol_snapshot":
			return "snap-uuid-9999", nil
		case "bdev_lvol_get_fragmap":
			return FragmapResponse{
				ClusterSize: 65536,
				Offset:      0,
				Size:        8 * 65536,
				NumClusters: 5,
				Bitmask:     bitmaskB64,
			}, nil
		case "nvmf_create_subsystem":
			return true, nil
		case "nvmf_subsystem_add_ns":
			return uint32(1), nil
		case "iscsi_create_target_node":
			return true, nil
		case "bdev_get_iostat":
			return BdevGetIostatResponse{
				TickRate: 2400000000,
				Ticks:    1000000,
				Bdevs: []BdevIoStat{
					{
						Name:         "lvs_pool0/vol1",
						BytesRead:    409600,
						NumReadOps:   100,
						BytesWritten: 819200,
						NumWriteOps:  200,
					},
				},
			}, nil
		default:
			return nil, &RPCError{Code: -32601, Message: "Method not found"}
		}
	})

	ctx := context.Background()

	ver, err := client.GetVersion(ctx)
	if err != nil || ver != "SPDK v24.09" {
		t.Fatalf("GetVersion got (%q, %v), want SPDK v24.09", ver, err)
	}

	bdevs, err := client.BdevNvmeAttachController(ctx, "Nvme1", "PCIe", "0000:01:00.0")
	if err != nil || len(bdevs) != 1 || bdevs[0] != "Nvme1n1" {
		t.Fatalf("BdevNvmeAttachController failed: bdevs=%v, err=%v", bdevs, err)
	}

	// Verify structured RPCError on bad PCI address.
	_, err = client.BdevNvmeAttachController(ctx, "NvmeBad", "PCIe", "0000:99:00.0")
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != -19 {
		t.Fatalf("expected RPCError code -19, got %v", err)
	}

	health, err := client.BdevNvmeGetControllerHealthInfo(ctx, "Nvme1")
	if err != nil || health.TemperatureCelsius != 38 || health.AvailableSpare != 100 {
		t.Fatalf("BdevNvmeGetControllerHealthInfo unexpected: %+v, err=%v", health, err)
	}

	uringBdev, err := client.BdevUringCreate(ctx, "uring_sdb", "/dev/sdb", 4096, "")
	if err != nil || uringBdev != "uring_sdb" {
		t.Fatalf("BdevUringCreate failed: %q, %v", uringBdev, err)
	}

	if err := client.BdevRaidCreate(ctx, "raid_pool0", "RAID1", 64, []string{"Nvme1n1", "Nvme2n1"}, true); err != nil {
		t.Fatalf("BdevRaidCreate failed: %v", err)
	}

	lvsUUID, err := client.BdevLvolCreateLvstore(ctx, "raid_pool0", "lvs_pool0", 65536, "")
	if err != nil || lvsUUID != "lvs-uuid-1234" {
		t.Fatalf("BdevLvolCreateLvstore failed: %q, %v", lvsUUID, err)
	}

	lvolUUID, err := client.BdevLvolCreate(ctx, "lvs_pool0", "vol1", 10*1024*1024*1024, true, "")
	if err != nil || lvolUUID != "lvol-uuid-5678" {
		t.Fatalf("BdevLvolCreate failed: %q, %v", lvolUUID, err)
	}

	snapUUID, err := client.BdevLvolSnapshot(ctx, "lvs_pool0/vol1", "snap1")
	if err != nil || snapUUID != "snap-uuid-9999" {
		t.Fatalf("BdevLvolSnapshot failed: %q, %v", snapUUID, err)
	}

	frag, err := client.BdevLvolGetFragmap(ctx, "lvs_pool0/snap1", 0, 8*65536)
	if err != nil {
		t.Fatalf("BdevLvolGetFragmap failed: %v", err)
	}
	extents, err := DecodeFragmapExtents(frag)
	if err != nil {
		t.Fatalf("DecodeFragmapExtents failed: %v", err)
	}
	if len(extents) != 2 {
		t.Fatalf("expected 2 coalesced extents from bitmask 0x37, got %+v", extents)
	}
	if extents[0].OffsetBytes != 0 || extents[0].SizeBytes != 3*65536 {
		t.Fatalf("unexpected extent[0]: %+v", extents[0])
	}
	if extents[1].OffsetBytes != 4*65536 || extents[1].SizeBytes != 2*65536 {
		t.Fatalf("unexpected extent[1]: %+v", extents[1])
	}

	// Verify MergeBlockExtents bridges cluster 3 when combined with another snapshot delta.
	bridged := MergeBlockExtents(extents, []BlockExtent{{OffsetBytes: 3 * 65536, SizeBytes: 65536}})
	if len(bridged) != 1 || bridged[0].OffsetBytes != 0 || bridged[0].SizeBytes != 6*65536 {
		t.Fatalf("expected single coalesced extent of 6 clusters, got %+v", bridged)
	}

	if err := client.NvmfCreateSubsystem(ctx, "nqn.2026-11.io.velocor:vol1", "VELO0001", "", false, 32); err != nil {
		t.Fatalf("NvmfCreateSubsystem failed: %v", err)
	}
	nsid, err := client.NvmfSubsystemAddNs(ctx, "nqn.2026-11.io.velocor:vol1", "lvs_pool0/vol1", 1, "")
	if err != nil || nsid != 1 {
		t.Fatalf("NvmfSubsystemAddNs failed: nsid=%d, err=%v", nsid, err)
	}

	if err := client.IscsiCreateTargetNode(
		ctx,
		"iqn.2026-11.io.velocor:vol1",
		"vol1",
		[]IscsiLun{{BdevName: "lvs_pool0/vol1", LunID: 0}},
		[]IscsiPgIgMap{{PgTag: 1, IgTag: 1}},
		64,
		true, false, false, 0,
	); err != nil {
		t.Fatalf("IscsiCreateTargetNode failed: %v", err)
	}

	iostat, err := client.BdevGetIostat(ctx, "lvs_pool0/vol1")
	if err != nil || len(iostat.Bdevs) != 1 || iostat.Bdevs[0].NumWriteOps != 200 {
		t.Fatalf("BdevGetIostat failed: %+v, err=%v", iostat, err)
	}
}
