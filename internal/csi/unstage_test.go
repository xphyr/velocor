package csi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/container-storage-interface/spec/lib/go/csi"
)

func TestParseISCSISessions(t *testing.T) {
	out := "tcp: [2] 172.16.20.115:3260,1 iqn.2026-11.io.velocor:csi-vol-df5d4aaeea83-ede24dfd845f (non-flash)\n" +
		"tcp: [14] [fd00::5]:3260,1 iqn.2000-01.com.synology:syno1221.pvc-3e55 (non-flash)\n" +
		"iscsiadm: No active sessions.\n"
	got := parseISCSISessions(out)
	want := []iscsiSession{
		{SID: "2", Portal: "172.16.20.115:3260", Target: "iqn.2026-11.io.velocor:csi-vol-df5d4aaeea83-ede24dfd845f"},
		{SID: "14", Portal: "[fd00::5]:3260", Target: "iqn.2000-01.com.synology:syno1221.pvc-3e55"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
}

// fakeISCSIAdm installs an iscsiadm on PATH that keeps sessions and node records in files and
// logs its calls, and makes the driver use it.
func fakeISCSIAdm(t *testing.T, sessions, records string) (dir string) {
	t.Helper()
	dir = t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "sessions"), []byte(sessions), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "records"), []byte(records), 0o644)
	script := `#!/bin/bash
d="$(dirname "$0")"; echo "$*" >> "$d/calls"
case "$*" in
  "-m session") [ -s "$d/sessions" ] || { echo "iscsiadm: No active sessions."; exit 21; }; cat "$d/sessions" ;;
  "-m session -r "*" -u") grep -v "^tcp: \[$4\] " "$d/sessions" > "$d/s.new"; mv "$d/s.new" "$d/sessions" ;;
  "-m node") cat "$d/records" ;;
  "-m node -T "*" -o delete") grep -v " $4\$" "$d/records" > "$d/r.new"; mv "$d/r.new" "$d/records" ;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "iscsiadm"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("VELOCOR_ISCSIADM", "container")
	hostISCSIAdmOnce, hostISCSIAdm = sync.Once{}, false
	t.Cleanup(func() { hostISCSIAdmOnce, hostISCSIAdm = sync.Once{}, false })
	return dir
}

// A session on a portal the publish context doesn't name (the volume's portals changed since,
// or another iface) is still logged out, by session ID, and its records forgotten; sessions of
// other volumes stay.
func TestDisconnectISCSIFindsTheTargetsSessions(t *testing.T) {
	const mine = "iqn.2026-11.io.velocor:csi-vol-aaa-ede24dfd845f"
	const other = "iqn.2026-11.io.velocor:csi-vol-bbb-ede24dfd845f"
	dir := fakeISCSIAdm(t,
		"tcp: [2] 172.16.20.115:3260,1 "+mine+" (non-flash)\ntcp: [3] 172.16.20.115:3260,1 "+other+" (non-flash)\n",
		"172.16.20.115:3260,1 "+mine+"\n172.16.15.112:3260,1 "+mine+"\n172.16.20.115:3260,1 "+other+"\n")
	m := &LinuxMounter{}
	err := m.DisconnectTarget(context.Background(), map[string]string{"protocol": "ISCSI", "target_iqn": mine,
		"portal_ip": "10.9.9.9", "portal_port": "3260"})
	if err != nil {
		t.Fatal(err)
	}
	sessions, _ := os.ReadFile(filepath.Join(dir, "sessions"))
	records, _ := os.ReadFile(filepath.Join(dir, "records"))
	if strings.Contains(string(sessions), mine) || !strings.Contains(string(sessions), other) {
		t.Fatalf("sessions left: %s", sessions)
	}
	if strings.Contains(string(records), mine) || !strings.Contains(string(records), other) {
		t.Fatalf("records left: %s", records)
	}

	// By volume ID (an unstage without its metadata).
	if err := m.DisconnectVolume(context.Background(), "vol-bbb"); err != nil {
		t.Fatal(err)
	}
	if s, _ := os.ReadFile(filepath.Join(dir, "sessions")); strings.TrimSpace(string(s)) != "" {
		t.Fatalf("by volume ID left: %s", s)
	}
}

// A logout that doesn't take (the session is still there) fails the unstage, so Kubernetes
// retries instead of detaching the volume under a live session.
func TestDisconnectISCSIFailsIfTheSessionStays(t *testing.T) {
	const mine = "iqn.2026-11.io.velocor:csi-vol-aaa-ede24dfd845f"
	dir := fakeISCSIAdm(t, "tcp: [2] 172.16.20.115:3260,1 "+mine+" (non-flash)\n", "")
	// Make logout a no-op.
	script, _ := os.ReadFile(filepath.Join(dir, "iscsiadm"))
	_ = os.WriteFile(filepath.Join(dir, "iscsiadm"), []byte(strings.Replace(string(script), `grep -v "^tcp: \[$4\] " "$d/sessions" > "$d/s.new"; mv "$d/s.new" "$d/sessions"`, `true`, 1)), 0o755)
	err := (&LinuxMounter{}).DisconnectTarget(context.Background(), map[string]string{"protocol": "ISCSI", "target_iqn": mine})
	if err == nil || !strings.Contains(err.Error(), "still logged in") {
		t.Fatalf("got %v, want an error saying the session is still there", err)
	}
}

// An unstage without stage metadata still disconnects the volume.
func TestUnstageWithoutMetadataDisconnectsTheVolume(t *testing.T) {
	m := newFakeMounter()
	ns := NewNodeServer("node-a", m)
	if _, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: "vol-x", StagingTargetPath: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(m.disconnectedVolumes, []string{"vol-x"}) {
		t.Fatalf("disconnected %v, want [vol-x]", m.disconnectedVolumes)
	}
}

// A node without iscsiadm (NVMe only) has no iSCSI sessions: disconnecting by volume ID works.
func TestDisconnectVolumeWithoutISCSIAdm(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("VELOCOR_ISCSIADM", "container")
	hostISCSIAdmOnce, hostISCSIAdm = sync.Once{}, false
	t.Cleanup(func() { hostISCSIAdmOnce, hostISCSIAdm = sync.Once{}, false })
	if err := (&LinuxMounter{SysRoot: t.TempDir()}).DisconnectVolume(context.Background(), "vol-x"); err != nil {
		t.Fatalf("without iscsiadm: %v", err)
	}
}

// A raw block device that is gone (a node reboot, a lost session) doesn't keep its volume from
// being unstaged; one that can't be flushed does.
func TestUnstageOfAGoneBlockDevice(t *testing.T) {
	if err := (&LinuxMounter{}).FlushDevice(context.Background(), filepath.Join(t.TempDir(), "gone")); !errors.Is(err, ErrDeviceGone) {
		t.Fatalf("flush of a missing device: %v", err)
	}
	m := newFakeMounter()
	ns := NewNodeServer("node-a", m)
	staging := t.TempDir()
	meta := filepath.Join(staging, stageMetaFilename)
	b, _ := json.Marshal(stageMetadata{DevicePath: "/dev/nvme9n1", IsBlock: true})
	if err := os.WriteFile(meta, b, 0o600); err != nil {
		t.Fatal(err)
	}
	unstage := func() error {
		_, err := ns.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{VolumeId: "vol-x", StagingTargetPath: staging})
		return err
	}
	m.flushErr = errors.New("I/O error")
	if err := unstage(); err == nil {
		t.Fatal("unstaged a device that couldn't be flushed")
	}
	m.flushErr = fmt.Errorf("%w: no such file or directory", ErrDeviceGone)
	if err := unstage(); err != nil {
		t.Fatalf("unstage of a gone device: %v", err)
	}
	if _, err := os.Stat(meta); err == nil {
		t.Fatal("the stage metadata stays")
	}
}
