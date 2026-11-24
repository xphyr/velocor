package server

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeBootc: an upgrade that waits for release, then stages "new"; a boot ID that can change.
type fakeBootc struct {
	release chan struct{}
	bootID  atomic.Value
	booted  atomic.Value // digest of the booted deployment
	staged  atomic.Value // digest staged ("" = none)
}

func (f *fakeBootc) install(s *Server) {
	f.release = make(chan struct{})
	f.bootID.Store("boot-1")
	f.booted.Store("sha256:old")
	f.staged.Store("")
	s.os.bootID = func() string { return f.bootID.Load().(string) }
	s.os.run = func(_ context.Context, w *tailBuffer, args ...string) error {
		_, _ = fmt.Fprintf(w, "bootc %s\nFetching...\n", strings.Join(args, " "))
		<-f.release
		if args[0] == "switch" && strings.Contains(args[1], "broken") {
			return fmt.Errorf("exit status 1")
		}
		f.staged.Store("sha256:new")
		_, _ = fmt.Fprintln(w, "Queued for next boot")
		return nil
	}
	s.os.status = func(context.Context) OSStatus {
		st := OSStatus{Available: true, Booted: &OSDeployment{Image: "registry.example/os:old", ImageDigest: f.booted.Load().(string)}}
		if d := f.staged.Load().(string); d != "" {
			st.Staged = &OSDeployment{Image: "registry.example/os:new", ImageDigest: d}
		}
		return st
	}
}

func waitJob(t *testing.T, h *harness, state string) map[string]any {
	t.Helper()
	for i := 0; i < 100; i++ {
		j := h.expect(h.do("GET", "/api/v1/system/upgrade", h.admin, nil))(http.StatusOK)["job"].(map[string]any)
		if j["state"] == state {
			return j
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("job never reached %s", state)
	return nil
}

func TestUpgradeIsABackgroundJob(t *testing.T) {
	h := newHarness(t)
	f := &fakeBootc{}
	f.install(h.srv)
	j := h.expect(h.do("POST", "/api/v1/system/upgrade", h.admin, map[string]any{"image_ref": "registry.example/os:new"}))(http.StatusAccepted)["job"].(map[string]any)
	if j["state"] != "RUNNING" {
		t.Fatalf("job %v", j)
	}
	// Output so far is visible; a second upgrade and a reboot are refused meanwhile.
	if out := waitJob(t, h, "RUNNING")["output"].(string); !strings.Contains(out, "bootc switch registry.example/os:new") {
		t.Fatalf("running output %q", out)
	}
	h.expect(h.do("POST", "/api/v1/system/upgrade", h.admin, map[string]any{}))(http.StatusConflict)
	h.expect(h.do("POST", "/api/v1/system/reboot", h.admin, map[string]any{"force": true}))(http.StatusConflict)

	close(f.release)
	done := waitJob(t, h, "SUCCEEDED")
	if done["staged"].(map[string]any)["image_digest"] != "sha256:new" || !strings.Contains(done["output"].(string), "Queued for next boot") {
		t.Fatalf("finished job %v", done)
	}
	if st, _ := h.st.GetSettings(); st.OSUpgrade == nil || st.OSUpgrade.Digest != "sha256:new" || st.OSUpgrade.BootID != "boot-1" {
		t.Fatalf("staged record %+v", st.OSUpgrade)
	}
}

func TestUpgradeFailureIsReported(t *testing.T) {
	h := newHarness(t)
	f := &fakeBootc{}
	f.install(h.srv)
	close(f.release)
	h.expect(h.do("POST", "/api/v1/system/upgrade", h.admin, map[string]any{"image_ref": "registry.example/os:broken"}))(http.StatusAccepted)
	j := waitJob(t, h, "FAILED")
	if !strings.Contains(fmt.Sprint(j["error"]), "exit status 1") {
		t.Fatalf("failed job %v", j)
	}
	if st, _ := h.st.GetSettings(); st.OSUpgrade != nil {
		t.Fatal("a failed upgrade must not be recorded as staged")
	}
}

// After a reboot, a staged image that isn't the booted one is reported until dismissed; one
// that is clears quietly.
func TestUpgradeThatDidntTakeEffect(t *testing.T) {
	h := newHarness(t)
	f := &fakeBootc{}
	f.install(h.srv)
	close(f.release)
	h.expect(h.do("POST", "/api/v1/system/upgrade", h.admin, map[string]any{"image_ref": "registry.example/os:new"}))(http.StatusAccepted)
	waitJob(t, h, "SUCCEEDED")
	warnings := func() string {
		return fmt.Sprint(h.expect(h.do("GET", "/api/v1/system/status", h.admin, nil))(http.StatusOK)["warnings"])
	}

	h.srv.CheckOSUpgrade(context.Background()) // no reboot yet
	if strings.Contains(warnings(), "didn't take effect") {
		t.Fatal("warned before any reboot")
	}
	f.bootID.Store("boot-2") // rebooted, but still on the old image
	h.srv.CheckOSUpgrade(context.Background())
	if w := warnings(); !strings.Contains(w, "didn't take effect") || !strings.Contains(w, "registry.example/os:new") {
		t.Fatalf("warnings %s", w)
	}
	if st, _ := h.st.GetSettings(); st.OSUpgrade != nil {
		t.Fatal("the record must be cleared once checked")
	}
	h.expect(h.do("DELETE", "/api/v1/system/upgrade/warning", h.admin, nil))(http.StatusOK)
	if strings.Contains(warnings(), "didn't take effect") {
		t.Fatal("dismissed warning still shown")
	}

	// Staged again, and this time it boots.
	f.release = make(chan struct{})
	close(f.release)
	h.expect(h.do("POST", "/api/v1/system/upgrade", h.admin, map[string]any{"image_ref": "registry.example/os:new"}))(http.StatusAccepted)
	waitJob(t, h, "SUCCEEDED")
	f.bootID.Store("boot-3")
	f.booted.Store("sha256:new")
	h.srv.CheckOSUpgrade(context.Background())
	if strings.Contains(warnings(), "didn't take effect") {
		t.Fatal("warned although the staged image booted")
	}
}

func TestRebootNamesConnectedHosts(t *testing.T) {
	h := newHarness(t)
	exp, _, _ := h.nvmeExport("nqn.2026-01.io.example:host-a")
	h.fake.Connect(exp["subsystem_nqn"].(string), "nqn.2026-01.io.example:host-a")
	code, out := h.do("POST", "/api/v1/system/reboot", h.admin, map[string]any{})
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(out["error"]), "host-a") {
		t.Fatalf("reboot with a connected host: %d %v", code, out)
	}
	h.expect(h.do("POST", "/api/v1/system/reboot", h.admin, map[string]any{"force": true}))(http.StatusAccepted)
}
