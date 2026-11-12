package reconciler

import (
	"bufio"
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

func TestReconcilerRepairsCrashDrift(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "reconcile-test.db")
	st, err := store.OpenBoltStore(dbPath)
	if err != nil {
		t.Fatalf("OpenBoltStore failed: %v", err)
	}
	defer func() { _ = st.Close() }()

	// Seed desired state in bbolt: 2 claimed NVMe disks, 1 RAID1 pool, 1 Volume, 1 Portal, 1 InitiatorGroup, 1 NVMe-oF Export.
	for _, d := range []*store.Disk{
		{ID: "nvme-1", Name: "nvme1n1", Type: store.DiskTypeNVMe, PCIAddress: "0000:01:00.0", SizeBytes: 100 << 30},
		{ID: "nvme-2", Name: "nvme2n1", Type: store.DiskTypeNVMe, PCIAddress: "0000:02:00.0", SizeBytes: 100 << 30},
	} {
		if err := st.UpsertDisk(d); err != nil {
			t.Fatalf("UpsertDisk failed: %v", err)
		}
		if _, err := st.ClaimDisk(d.ID, store.DiskDriverStateVfioPCI, "Nvme_"+d.ID+"n1"); err != nil {
			t.Fatalf("ClaimDisk failed: %v", err)
		}
	}

	pool := &store.Pool{
		ID:        "pool-1",
		Name:      "pool0",
		RaidLevel: store.RaidLevel1,
		DiskIDs:   []string{"nvme-1", "nvme-2"},
	}
	if err := st.CreatePool(pool); err != nil {
		t.Fatalf("CreatePool failed: %v", err)
	}

	vol := &store.Volume{
		ID:            "vol-1",
		Name:          "vol0",
		PoolID:        "pool-1",
		SizeBytes:     10 << 30,
		ThinProvision: true,
		QosPolicy:     store.QosPolicy{RwIosPerSec: 25000},
	}
	if err := st.CreateVolume(vol); err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	portal := &store.Portal{
		ID:        "portal-1",
		Name:      "tcp-0",
		Protocol:  store.ExportProtocolNVMeoFTCP,
		IPAddress: "10.0.0.10",
		Port:      4420,
	}
	if err := st.CreatePortal(portal); err != nil {
		t.Fatalf("CreatePortal failed: %v", err)
	}

	ig := &store.InitiatorGroup{
		ID:       "ig-1",
		Name:     "hosts-0",
		HostNQNs: []string{"nqn.2014-08.org.nvmexpress:uuid:node1"},
	}
	if err := st.CreateInitiatorGroup(ig); err != nil {
		t.Fatalf("CreateInitiatorGroup failed: %v", err)
	}

	exp := &store.Export{
		ID:                "exp-1",
		Name:              "exp0",
		VolumeID:          "vol-1",
		Protocol:          store.ExportProtocolNVMeoFTCP,
		SubsystemNQN:      "nqn.2026-11.io.velocor:vol0",
		NamespaceID:       1,
		PortalIDs:         []string{"portal-1"},
		InitiatorGroupIDs: []string{"ig-1"},
	}
	if err := st.CreateExport(exp); err != nil {
		t.Fatalf("CreateExport failed: %v", err)
	}

	// Spin up a mock spdk_tgt server starting with EMPTY runtime state (simulating spdk_tgt crash/restart).
	sockPath := filepath.Join(t.TempDir(), "spdk-reconcile.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer func() { _ = ln.Close() }()

	var mu sync.Mutex
	calls := make(map[string]int)

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
				mu.Lock()
				calls[req.Method]++
				mu.Unlock()

				var result any
				switch req.Method {
				case "spdk_get_version":
					result = map[string]any{"version": "SPDK v24.09"}
				case "bdev_get_bdevs":
					result = []spdk.BdevInfo{}
				case "bdev_nvme_attach_controller":
					result = []string{req.Params["name"].(string) + "n1"}
				case "bdev_nvme_get_controller_health_info":
					result = spdk.NvmeHealthInfo{TemperatureCelsius: 34, AvailableSpare: 100}
				case "bdev_raid_get_bdevs":
					result = []spdk.RaidBdevInfo{}
				case "bdev_raid_create":
					result = true
				case "bdev_lvol_get_lvstores":
					result = []spdk.LvolStoreInfo{
						{
							UUID:              "lvs-uuid-reconciled",
							Name:              "lvs_pool0",
							BaseBdev:          "raid_pool0",
							TotalDataClusters: 1600,
							FreeClusters:      1400,
							ClusterSize:       65536,
						},
					}
				case "bdev_lvol_create_lvstore":
					result = "lvs-uuid-reconciled"
				case "bdev_lvol_create":
					result = "lvol-uuid-reconciled"
				case "bdev_set_qos_limit":
					result = true
				case "nvmf_get_subsystems":
					result = []spdk.NvmfSubsystem{}
				case "nvmf_get_transports":
					result = []spdk.NvmfTransport{}
				case "nvmf_create_transport", "nvmf_create_subsystem", "nvmf_subsystem_add_host", "nvmf_subsystem_add_listener":
					result = true
				case "nvmf_subsystem_add_ns":
					result = uint32(1)
				default:
					result = true
				}
				_ = json.NewEncoder(c).Encode(map[string]any{
					"jsonrpc": "2.0",
					"id":      req.ID,
					"result":  result,
				})
			}(conn)
		}
	}()

	spdkClient := spdk.NewClient(sockPath)
	rec := New(st, spdkClient, nil, nil)

	state, err := rec.ReconcileOnce(context.Background())
	if err != nil {
		t.Fatalf("ReconcileOnce returned unexpected error: %v", err)
	}
	if state.DriftRepairedCount == 0 {
		t.Fatalf("expected DriftRepairedCount > 0 after crash recovery pass, got 0")
	}
	if len(state.DegradedResources) != 0 {
		t.Fatalf("expected 0 degraded resources, got %v", state.DegradedResources)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, expectedRPC := range []string{
		"bdev_nvme_attach_controller",
		"bdev_raid_create",
		"bdev_lvol_create",
		"bdev_set_qos_limit",
		"nvmf_create_transport",
		"nvmf_create_subsystem",
		"nvmf_subsystem_add_ns",
		"nvmf_subsystem_add_host",
		"nvmf_subsystem_add_listener",
	} {
		if calls[expectedRPC] == 0 {
			t.Fatalf("expected reconciler to invoke %q, but count was 0", expectedRPC)
		}
	}
}
