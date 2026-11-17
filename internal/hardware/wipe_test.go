package hardware

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/store"
)

func TestSPDKMetadataDetectedAndWiped(t *testing.T) {
	h := newFakeHost(t)
	h.blockDev("pci0000:00/a/block", "sdh", "8:112")
	h.blockDev("pci0000:00/b/block", "sdi", "8:128")
	h.blockDev("pci0000:00/c/block", "sdj", "8:144")
	h.symlink("../../sdj", filepath.Join(h.sys, "class", "block", "sdj", "holders", "dm-7"))

	// A former RAID member (superblock at 0) and a former non-superblock RAID0 member
	// (blobstore at 0), each 48 MiB with data at the end too.
	const size = 48 << 20
	raidMember := make([]byte, size)
	copy(raidMember, "SPDKRAID")
	copy(raidMember[1<<20:], "SPDKBLOB")
	copy(raidMember[size-4096:], "EFI PART tail")
	blobMember := make([]byte, size)
	copy(blobMember, "SPDKBLOB")
	sdh, sdi, sdj := filepath.Join(h.dev, "sdh"), filepath.Join(h.dev, "sdi"), filepath.Join(h.dev, "sdj")
	mustWriteFile(t, sdh, string(raidMember))
	mustWriteFile(t, sdi, string(blobMember))
	mustWriteFile(t, sdj, string(make([]byte, 1<<20)))
	m := h.manager()

	for dev, want := range map[string]string{sdh: "RAID member", sdi: "volume store"} {
		err := m.CheckClaimable(&store.Disk{ID: "x", DevicePath: dev}, false)
		if !errors.Is(err, store.ErrInUse) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v, want refusal naming a Velocor %s", dev, err, want)
		}
	}

	if err := m.WipeMetadata(&store.Disk{ID: "h", DevicePath: sdh}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(sdh)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != size || !bytes.Equal(got[:wipeHeadBytes], make([]byte, wipeHeadBytes)) ||
		!bytes.Equal(got[size-wipeTailBytes:], make([]byte, wipeTailBytes)) {
		t.Fatal("head or tail of the disk not zeroed (or size changed)")
	}
	if err := m.CheckClaimable(&store.Disk{ID: "h", DevicePath: sdh}, false); err != nil {
		t.Fatalf("wiped disk must be claimable, got %v", err)
	}

	// Refusals: boot disk, a disk in use, no kernel device.
	if err := m.WipeMetadata(&store.Disk{ID: "b", DevicePath: sdi, IsBootDevice: true}); !errors.Is(err, store.ErrBootDeviceProtected) {
		t.Errorf("boot disk: %v", err)
	}
	if err := m.WipeMetadata(&store.Disk{ID: "j", DevicePath: sdj}); !errors.Is(err, store.ErrInUse) {
		t.Errorf("disk with holders: %v", err)
	}
	if err := m.WipeMetadata(&store.Disk{ID: "n"}); !errors.Is(err, store.ErrInvalidArgument) {
		t.Errorf("disk without device path: %v", err)
	}
}
