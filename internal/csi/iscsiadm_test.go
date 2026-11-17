package csi

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
)

// Outside a host-PID pod (as in unit tests) the bundled iscsiadm is used; beside a host the
// host's own, through nsenter. VELOCOR_ISCSIADM=container forces the bundled one.
func TestISCSIAdmSelection(t *testing.T) {
	reset := func() { hostISCSIAdmOnce = sync.Once{}; hostISCSIAdm = false }
	t.Cleanup(reset)

	reset()
	t.Setenv("VELOCOR_ISCSIADM", "container")
	if c := iscsiadm(context.Background(), "-m", "session"); !strings.HasSuffix(c.Path, "iscsiadm") && c.Args[0] != "iscsiadm" {
		t.Fatalf("forced container mode ran %v", c.Args)
	}

	reset()
	t.Setenv("VELOCOR_ISCSIADM", "")
	host, _ := os.Readlink(hostMountNS)
	self, _ := os.Readlink("/proc/self/ns/mnt")
	c := iscsiadm(context.Background(), "-m", "session")
	got := strings.Join(c.Args, " ")
	if host != "" && self != "" && host != self {
		if c.Args[0] != "nsenter" || !strings.HasSuffix(got, "iscsiadm -m session") {
			t.Fatalf("beside a host: %q, want nsenter ... iscsiadm -m session", got)
		}
	} else if c.Args[0] != "iscsiadm" {
		t.Fatalf("same mount namespace as PID 1: %q, want the bundled iscsiadm", got)
	}
}
