package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/velocor/velocor/internal/store"
)

// Boot-time kernel options the API can change. Only init_on_alloc: Fedora's kernel zeroes every
// page and slab object it allocates (hardening against leaking stale kernel memory), which costs
// the kernel data plane CPU on every I/O (up to 15% on large reads, K-3). The OS image boots with
// init_on_alloc=0 (kargs.d); an operator who prefers the hardening turns it back on here.
//
// The boot entries are the record, not the database: the running value comes from
// /proc/cmdline, the next boot's from `rpm-ostree kargs`. A change stages a deployment of the
// same image with the new arguments, so it takes effect at the next reboot, and after that
// "rollback" is the same image with the old arguments: an older OS version kept for rollback is
// dropped, which needs force.

const initOnAllocArg = "init_on_alloc"

// kernelOption is one boot option's state.
type kernelOption struct {
	Available      bool   `json:"available"` // it can be changed here (an ostree host)
	Message        string `json:"message,omitempty"`
	Running        bool   `json:"running"`
	NextBoot       bool   `json:"next_boot"`
	RebootRequired bool   `json:"reboot_required"`
}

// kernelBool reads a boolean early parameter from a kernel command line: the last occurrence
// wins, as in the kernel; def without one. Values as kstrtobool reads them.
func kernelBool(cmdline, name string, def bool) bool {
	v := def
	for _, f := range strings.Fields(cmdline) {
		val, ok := strings.CutPrefix(f, name+"=")
		if !ok {
			continue
		}
		switch val = strings.ToLower(val); {
		case val == "on" || strings.HasPrefix(val, "y") || strings.HasPrefix(val, "t") || val == "1":
			v = true
		case val == "off" || strings.HasPrefix(val, "n") || strings.HasPrefix(val, "f") || val == "0":
			v = false
		}
	}
	return v
}

// kargsChange returns the rpm-ostree options that leave name=value the only setting of name on
// cmdline, or nil if it already is.
func kargsChange(cmdline, name, value string) []string {
	want, have := name+"="+value, false
	var args []string
	for _, f := range strings.Fields(cmdline) {
		switch {
		case f == want && !have:
			have = true
		case f == want:
			// A duplicate changes nothing.
		case f == name || strings.HasPrefix(f, name+"="):
			args = append(args, "--delete-if-present="+f)
		}
	}
	if !have {
		args = append(args, "--append-if-missing="+want)
	}
	return args
}

func (o *osLifecycle) runKargs(ctx context.Context, args ...string) (string, error) {
	if o.kargs != nil {
		return o.kargs(ctx, args...)
	}
	out, err := hostCommand(ctx, "rpm-ostree", append([]string{"kargs"}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func (o *osLifecycle) bootedCmdline() string {
	if o.cmdline != nil {
		return o.cmdline()
	}
	b, _ := os.ReadFile("/proc/cmdline")
	return strings.TrimSpace(string(b))
}

// initOnAlloc is the option's state; absent from the command line it is Fedora's default, on.
func (s *Server) initOnAlloc(ctx context.Context) kernelOption {
	k := kernelOption{Running: kernelBool(s.os.bootedCmdline(), initOnAllocArg, true)}
	k.NextBoot = k.Running
	if !s.os.bootcStatus(ctx).Available {
		k.Message = "Not an image-mode (bootc) host: boot options can't be changed here."
		return k
	}
	next, err := s.os.runKargs(ctx)
	if err != nil {
		k.Message = fmt.Sprintf("Can't read the next boot's kernel arguments: %v (%s)", err, next)
		return k
	}
	k.Available = true
	k.NextBoot = kernelBool(next, initOnAllocArg, true)
	k.RebootRequired = k.NextBoot != k.Running
	return k
}

func (s *Server) handleGetKernelOptions(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	writeJSON(w, http.StatusOK, map[string]any{initOnAllocArg: s.initOnAlloc(ctx)})
}

// handlePutKernelOptions: PUT /api/v1/system/kernel-options {init_on_alloc, force}. It applies
// at the next reboot.
func (s *Server) handlePutKernelOptions(w http.ResponseWriter, r *http.Request) {
	var req struct {
		InitOnAlloc *bool `json:"init_on_alloc"`
		Force       bool  `json:"force"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.InitOnAlloc == nil {
		writeError(w, fmt.Errorf("%w: body must be {\"init_on_alloc\": true|false, \"force\": true|false}", store.ErrInvalidArgument))
		return
	}
	// Staging takes a while (rpm-ostree writes a deployment); one change at a time, and none
	// while an upgrade stages its own.
	if !s.os.kargsMu.TryLock() {
		writeError(w, fmt.Errorf("%w: a kernel option change is already running", store.ErrInUse))
		return
	}
	defer s.os.kargsMu.Unlock()
	if j := s.os.snapshot(); j != nil && j.State == "RUNNING" {
		writeError(w, fmt.Errorf("%w: an OS upgrade is running", store.ErrInUse))
		return
	}
	// The request context ends with the client; a half-written deployment must not.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Minute)
	defer cancel()

	st := s.os.bootcStatus(ctx)
	if !st.Available {
		writeError(w, fmt.Errorf("%w: not an image-mode (bootc) host: boot options can't be changed here", store.ErrInvalidArgument))
		return
	}
	next, err := s.os.runKargs(ctx)
	if err != nil {
		writeError(w, fmt.Errorf("read the kernel arguments: %v (%s)", err, next))
		return
	}
	value := "0"
	if *req.InitOnAlloc {
		value = "1"
	}
	if kernelBool(next, initOnAllocArg, true) == *req.InitOnAlloc { // already so for the next boot
		writeJSON(w, http.StatusOK, map[string]any{initOnAllocArg: s.initOnAlloc(ctx)})
		return
	}
	if lost := rollbackLost(st); lost != nil && !req.Force {
		writeJSON(w, http.StatusConflict, map[string]any{
			"error": fmt.Sprintf("the new boot entry would replace the rollback to %s: after the reboot, rolling back returns to this OS version with the old setting. Send \"force\": true to change it anyway",
				lost.Image),
			"rollback": lost,
		})
		return
	}
	if out, err := s.os.runKargs(ctx, kargsChange(next, initOnAllocArg, value)...); err != nil {
		writeError(w, fmt.Errorf("change the kernel arguments: %v (%s)", err, out))
		return
	}
	k := s.initOnAlloc(ctx)
	s.logger.Warn("kernel option changed; it takes effect at the next reboot",
		"option", initOnAllocArg, "value", *req.InitOnAlloc, "running", k.Running, "by", subjectFromContext(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{initOnAllocArg: k})
}

// rollbackLost is the rollback deployment a new one would replace: the previous OS version,
// unless an upgrade is staged (which replaces it anyway).
func rollbackLost(st OSStatus) *OSDeployment {
	if st.Staged != nil || st.Rollback == nil || st.Booted == nil || st.Rollback.ImageDigest == st.Booted.ImageDigest {
		return nil
	}
	return st.Rollback
}
