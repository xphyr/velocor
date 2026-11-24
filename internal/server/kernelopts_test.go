package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestKernelBool(t *testing.T) {
	for _, tc := range []struct {
		cmdline string
		def     bool
		want    bool
	}{
		{"ro quiet", true, true},                            // absent: the kernel's default
		{"ro init_on_alloc=0", true, false},                 //
		{"init_on_alloc=1 ro init_on_alloc=0", true, false}, // the last wins
		{"init_on_alloc=0 init_on_alloc=1", true, true},
		{"init_on_alloc=off", true, false},
		{"init_on_alloc=on", false, true},
		{"init_on_alloc=N", true, false},
		{"init_on_alloc=yes", false, true},
		{"init_on_alloc=", true, true},
		{"xinit_on_alloc=0 init_on_alloc_x=0", true, true},
	} {
		if got := kernelBool(tc.cmdline, initOnAllocArg, tc.def); got != tc.want {
			t.Errorf("%q: %v, want %v", tc.cmdline, got, tc.want)
		}
	}
}

func TestKargsChange(t *testing.T) {
	for _, tc := range []struct {
		cmdline, value string
		want           []string
	}{
		{"ro init_on_alloc=0", "0", nil},
		{"ro", "1", []string{"--append-if-missing=init_on_alloc=1"}},
		{"ro init_on_alloc=0 quiet", "1", []string{"--delete-if-present=init_on_alloc=0", "--append-if-missing=init_on_alloc=1"}},
		{"init_on_alloc=1 init_on_alloc=n", "0", []string{"--delete-if-present=init_on_alloc=1", "--delete-if-present=init_on_alloc=n", "--append-if-missing=init_on_alloc=0"}},
		{"init_on_alloc", "0", []string{"--delete-if-present=init_on_alloc", "--append-if-missing=init_on_alloc=0"}},
	} {
		if got := kargsChange(tc.cmdline, initOnAllocArg, tc.value); !slices.Equal(got, tc.want) {
			t.Errorf("%q to %s: %v, want %v", tc.cmdline, tc.value, got, tc.want)
		}
	}
}

// fakeKargs is rpm-ostree kargs: the next boot's arguments, and the calls that changed them.
type fakeKargs struct {
	next  string
	calls [][]string
	err   error
	block chan struct{}
}

func (f *fakeKargs) run(_ context.Context, args ...string) (string, error) {
	if len(args) == 0 {
		return f.next, nil
	}
	f.calls = append(f.calls, args)
	if f.block != nil {
		<-f.block
	}
	if f.err != nil {
		return "error: sysroot locked", f.err
	}
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--delete-if-present="); ok {
			f.next = strings.Join(slices.DeleteFunc(strings.Fields(f.next), func(x string) bool { return x == v }), " ")
		}
		if v, ok := strings.CutPrefix(a, "--append-if-missing="); ok && !slices.Contains(strings.Fields(f.next), v) {
			f.next += " " + v
		}
	}
	return "Changes queued for next boot", nil
}

func TestKernelOptions(t *testing.T) {
	h := newHarness(t)
	f := &fakeKargs{next: "root=UUID=1 rw init_on_alloc=0"}
	h.srv.os.cmdline = func() string { return "root=UUID=1 rw init_on_alloc=0" }
	h.srv.os.kargs = f.run
	status := OSStatus{Available: true, Booted: &OSDeployment{Image: "r/os:new", ImageDigest: "sha256:new"}, Rollback: &OSDeployment{Image: "r/os:old", ImageDigest: "sha256:old"}}
	h.srv.os.status = func(context.Context) OSStatus { return status }
	get := func() map[string]any {
		return h.expect(h.do("GET", "/api/v1/system/kernel-options", h.admin, nil))(http.StatusOK)["init_on_alloc"].(map[string]any)
	}
	put := func(body map[string]any, code int) map[string]any {
		t.Helper()
		return h.expect(h.do("PUT", "/api/v1/system/kernel-options", h.admin, body))(code)
	}
	if o := get(); o["available"] != true || o["running"] != false || o["next_boot"] != false || o["reboot_required"] != false {
		t.Fatalf("at first: %v", o)
	}
	// Only an admin, with a value; the value already set changes nothing.
	h.expect(h.do("PUT", "/api/v1/system/kernel-options", h.csiToken(), map[string]any{"init_on_alloc": true}))(http.StatusForbidden)
	put(map[string]any{}, http.StatusBadRequest)
	put(map[string]any{"init_on_alloc": false}, http.StatusOK)
	if len(f.calls) != 0 {
		t.Fatalf("a change without a difference called rpm-ostree: %v", f.calls)
	}
	// It would replace the rollback to the older OS version: refused, unless forced.
	r := put(map[string]any{"init_on_alloc": true}, http.StatusConflict)
	if !strings.Contains(r["error"].(string), "r/os:old") || len(f.calls) != 0 {
		t.Fatalf("not refused: %v %v", r, f.calls)
	}
	o := put(map[string]any{"init_on_alloc": true, "force": true}, http.StatusOK)["init_on_alloc"].(map[string]any)
	if o["running"] != false || o["next_boot"] != true || o["reboot_required"] != true || f.next != "root=UUID=1 rw init_on_alloc=1" {
		t.Fatalf("forced: %v, next boot %q", o, f.next)
	}
	// No rollback to lose (the same image), nor with an upgrade staged: not refused.
	status.Rollback.ImageDigest = "sha256:new"
	put(map[string]any{"init_on_alloc": false}, http.StatusOK)
	if f.next != "root=UUID=1 rw init_on_alloc=0" {
		t.Fatalf("back off: %q", f.next)
	}
	status.Rollback.ImageDigest = "sha256:old"
	status.Staged = &OSDeployment{Image: "r/os:newer", ImageDigest: "sha256:newer"}
	put(map[string]any{"init_on_alloc": true}, http.StatusOK)

	// rpm-ostree failing says so and changes nothing.
	f.err, f.next = errors.New("exit status 1"), "root=UUID=1 rw init_on_alloc=0"
	if r := put(map[string]any{"init_on_alloc": true}, http.StatusInternalServerError); !strings.Contains(r["error"].(string), "sysroot locked") {
		t.Fatalf("failure: %v", r)
	}
	f.err = nil

	// Not an image-mode host: reported, and refused.
	status.Available = false
	if o := get(); o["available"] != false || o["message"] == "" {
		t.Fatalf("not bootc: %v", o)
	}
	put(map[string]any{"init_on_alloc": true}, http.StatusBadRequest)
}

// One change at a time, and neither an upgrade during one nor one during an upgrade.
func TestKernelOptionsExclusive(t *testing.T) {
	h := newHarness(t)
	f := &fakeKargs{next: "rw init_on_alloc=0", block: make(chan struct{})}
	fb := &fakeBootc{}
	fb.install(h.srv)
	h.srv.os.cmdline = func() string { return "rw init_on_alloc=0" }
	h.srv.os.kargs = f.run
	body := map[string]any{"init_on_alloc": true}

	done := make(chan int)
	go func() {
		code, _ := h.do("PUT", "/api/v1/system/kernel-options", h.admin, body)
		done <- code
	}()
	for i := 0; i < 100 && h.srv.os.kargsMu.TryLock(); i++ {
		h.srv.os.kargsMu.Unlock()
		time.Sleep(10 * time.Millisecond)
	}
	h.expect(h.do("PUT", "/api/v1/system/kernel-options", h.admin, body))(http.StatusConflict)
	h.expect(h.do("POST", "/api/v1/system/upgrade", h.admin, map[string]any{"image_ref": "registry.example/os:new"}))(http.StatusConflict)
	close(f.block)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("first change: %d", code)
	}
	h.expect(h.do("POST", "/api/v1/system/upgrade", h.admin, map[string]any{"image_ref": "registry.example/os:new"}))(http.StatusAccepted)
	h.expect(h.do("PUT", "/api/v1/system/kernel-options", h.admin, map[string]any{"init_on_alloc": false}))(http.StatusConflict)
	close(fb.release)
	waitJob(t, h, "SUCCEEDED")
}
