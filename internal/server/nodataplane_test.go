package server

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/store"
)

// A server without a data plane answers 503 for a group snapshot instead of panicking: the
// group size limit comes from the data plane, so it is checked only once there is one.
func TestGroupSnapshotWithoutDataPlane(t *testing.T) {
	st, err := store.OpenBoltStore(filepath.Join(t.TempDir(), "no-dp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	if err := st.UpsertDisk(&store.Disk{ID: "d1", Name: "d1", Type: store.DiskTypeSSD, SizeBytes: 1 << 40, Claimed: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePool(&store.Pool{ID: "pool-1", Name: "p1", RaidLevel: store.RaidLevel1, DiskIDs: []string{"d1"}, ThinPoolName: "lvs_pool-1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateVolume(&store.Volume{ID: "vol-1", Name: "v1", PoolID: "pool-1", SizeBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	s := &Server{store: st}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/v1/volume-group-snapshots", strings.NewReader(`{"name":"g","volume_ids":["vol-1"]}`))
	s.handleCreateVolumeGroupSnapshot(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d (%s), want 503", rec.Code, rec.Body)
	}
}
