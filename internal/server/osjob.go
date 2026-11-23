package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/velocor/velocor/internal/backup"
	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/store"
)

// OS lifecycle (docs/review-history.md P1-7).
//
// An upgrade (bootc upgrade/switch) downloads hundreds of MB and takes minutes, so it runs as a
// background job the API, UI and veloctl follow. velod records what it staged (image, digest,
// boot ID); when velod next starts after a reboot and the appliance booted something else, the
// upgrade didn't take effect (the staged deployment is finalized during shutdown, so a machine
// cut off then boots the old one) and status says so. A reboot names the hosts it would cut
// off and needs force while any are connected, and is refused while an upgrade is running.

// OSJob is an upgrade in progress or the last one.
type OSJob struct {
	ID         string    `json:"id"`
	Image      string    `json:"image"` // requested; "" = upgrade to the newest of the current image
	State      string    `json:"state"` // RUNNING, SUCCEEDED, FAILED
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	By         string    `json:"by"`
	Output     string    `json:"output"` // the last part of bootc's output
	Error      string    `json:"error,omitempty"`
	// Staged is what bootc queued for the next boot (empty: nothing new to stage).
	Staged *OSDeployment `json:"staged,omitempty"`
}

type osLifecycle struct {
	mu  sync.Mutex
	job *OSJob
	out *tailBuffer
	// warning: the last upgrade didn't take effect (set once at startup; cleared by a new
	// upgrade or dismissal).
	warning string
	// Replaceable in tests.
	run    func(ctx context.Context, w *tailBuffer, args ...string) error
	status func(ctx context.Context) OSStatus
	bootID func() string
	reboot func() // tests must never reach the real one
}

// tailBuffer keeps the last 32 KiB written to it.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if over := len(t.buf) - 32<<10; over > 0 {
		t.buf = t.buf[over:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}

func (o *osLifecycle) runBootc(ctx context.Context, w *tailBuffer, args ...string) error {
	if o.run != nil {
		return o.run(ctx, w, args...)
	}
	cmd := hostCommand(ctx, "bootc", args...)
	cmd.Stdout, cmd.Stderr = w, w
	return cmd.Run()
}

func (o *osLifecycle) bootcStatus(ctx context.Context) OSStatus {
	if o.status != nil {
		return o.status(ctx)
	}
	return queryBootcStatus(ctx)
}

func (o *osLifecycle) currentBootID() string {
	if o.bootID != nil {
		return o.bootID()
	}
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}

// snapshot returns a copy of the current or last job, with its output so far.
func (o *osLifecycle) snapshot() *OSJob {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.job == nil {
		return nil
	}
	j := *o.job
	if j.State == "RUNNING" && o.out != nil {
		j.Output = o.out.String()
	}
	return &j
}

func (s *Server) handleStageUpgrade(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ImageRef string `json:"image_ref"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	imageRef := strings.TrimSpace(req.ImageRef)
	// The reference reaches bootc's command line: anything that could be read as an option
	// (or carries whitespace / control characters) is refused.
	if imageRef != "" && !imageRefRe.MatchString(imageRef) {
		writeError(w, fmt.Errorf("%w: %q is not a container image reference", store.ErrInvalidArgument, imageRef))
		return
	}
	o := &s.os
	o.mu.Lock()
	if o.job != nil && o.job.State == "RUNNING" {
		o.mu.Unlock()
		writeError(w, fmt.Errorf("%w: an upgrade is already running (started %s by %s)", store.ErrInUse, o.job.StartedAt.Format(time.RFC3339), o.job.By))
		return
	}
	if s.backups != nil {
		if _, err := s.backups.Take(backup.KindPreUpgrade); err != nil {
			o.mu.Unlock()
			writeError(w, fmt.Errorf("back up the database before the upgrade: %w", err))
			return
		}
	}
	job := &OSJob{ID: newResourceID("osjob"), Image: imageRef, State: "RUNNING", StartedAt: time.Now().UTC(), By: subjectFromContext(r.Context())}
	o.job, o.out = job, &tailBuffer{}
	out := o.out
	o.mu.Unlock()
	s.logger.Info("OS upgrade started", "image", imageRef, "job", job.ID, "by", job.By)

	go s.runUpgrade(job, out)
	writeJSON(w, http.StatusAccepted, map[string]any{"job": s.os.snapshot()})
}

func (s *Server) runUpgrade(job *OSJob, out *tailBuffer) {
	o := &s.os
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Minute)
	defer cancel()
	args := []string{"upgrade"}
	if job.Image != "" {
		args = []string{"switch", job.Image}
	}
	err := o.runBootc(ctx, out, args...)
	var staged *OSDeployment
	if err == nil {
		st := o.bootcStatus(ctx)
		staged = st.Staged
		if staged != nil {
			s.recordStaged(staged.Image, staged.ImageDigest, job.By)
		}
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	job.FinishedAt, job.Output, job.Staged = time.Now().UTC(), out.String(), staged
	if err != nil {
		job.State, job.Error = "FAILED", err.Error()
		s.logger.Error("OS upgrade failed", "job", job.ID, "err", err)
		return
	}
	job.State = "SUCCEEDED"
	o.warning = ""
	s.logger.Info("OS upgrade staged; it takes effect at the next reboot", "job", job.ID, "staged", staged)
}

// recordStaged remembers what the next boot should run.
func (s *Server) recordStaged(image, digest, by string) {
	st, err := s.store.GetSettings()
	if err != nil {
		s.logger.Error("record the staged OS image", "err", err)
		return
	}
	st.OSUpgrade = &store.OSUpgradeRecord{Image: image, Digest: digest, StagedAt: time.Now().UTC(), By: by, BootID: s.os.currentBootID()}
	if err := s.store.SaveSettings(st); err != nil {
		s.logger.Error("record the staged OS image", "err", err)
	}
}

// CheckOSUpgrade runs once when velod starts: if an OS image was staged before a reboot and
// the appliance booted something else, that upgrade didn't take effect.
func (s *Server) CheckOSUpgrade(ctx context.Context) {
	st, err := s.store.GetSettings()
	if err != nil || st.OSUpgrade == nil {
		return
	}
	rec := st.OSUpgrade
	if rec.BootID == s.os.currentBootID() {
		return // not rebooted since it was staged
	}
	booted := s.os.bootcStatus(ctx).Booted
	if booted == nil {
		return // bootc unavailable: can't tell
	}
	if booted.ImageDigest == rec.Digest {
		s.logger.Info("the staged OS image is running", "image", rec.Image)
	} else {
		msg := fmt.Sprintf("The OS upgrade to %s staged %s by %s didn't take effect: the appliance booted %s. "+
			"The staged deployment is finalized during shutdown, so a machine cut off then (power, hypervisor) boots the previous one. "+
			"Stage the upgrade again; dismiss this on the System page.",
			rec.Image, rec.StagedAt.Format("2006-01-02 15:04 MST"), rec.By, booted.Image)
		s.logger.Warn("the staged OS upgrade didn't take effect", "staged", rec.Image, "staged_digest", rec.Digest, "booted", booted.Image, "booted_digest", booted.ImageDigest)
		s.os.mu.Lock()
		s.os.warning = msg
		s.os.mu.Unlock()
	}
	st.OSUpgrade = nil
	if err := s.store.SaveSettings(st); err != nil {
		s.logger.Error("clear the staged OS image record", "err", err)
	}
}

func (s *Server) osWarning() string {
	s.os.mu.Lock()
	defer s.os.mu.Unlock()
	return s.os.warning
}

func (s *Server) handleGetUpgrade(w http.ResponseWriter, _ *http.Request) {
	st, _ := s.store.GetSettings()
	var pending *store.OSUpgradeRecord
	if st != nil {
		pending = st.OSUpgrade
	}
	writeJSON(w, http.StatusOK, map[string]any{"job": s.os.snapshot(), "pending": pending, "warning": s.osWarning()})
}

// handleDismissUpgradeWarning clears the "didn't take effect" warning.
func (s *Server) handleDismissUpgradeWarning(w http.ResponseWriter, r *http.Request) {
	s.os.mu.Lock()
	s.os.warning = ""
	s.os.mu.Unlock()
	s.logger.Info("OS upgrade warning dismissed", "by", subjectFromContext(r.Context()))
	writeJSON(w, http.StatusOK, map[string]any{"warning": ""})
}

func (s *Server) handleRollbackUpgrade(w http.ResponseWriter, r *http.Request) {
	o := &s.os
	if j := o.snapshot(); j != nil && j.State == "RUNNING" {
		writeError(w, fmt.Errorf("%w: an upgrade is running", store.ErrInUse))
		return
	}
	before := o.bootcStatus(r.Context())
	out := &tailBuffer{}
	if err := o.runBootc(r.Context(), out, "rollback"); err != nil {
		writeError(w, fmt.Errorf("bootc rollback failed: %v (%s)", err, out.String()))
		return
	}
	if before.Rollback != nil {
		s.recordStaged(before.Rollback.Image, before.Rollback.ImageDigest, subjectFromContext(r.Context()))
	}
	writeJSON(w, http.StatusOK, map[string]any{"output": out.String(), "reboot_required": true})
}

// connectedHosts lists the hosts connected to any export ("export: host").
func (s *Server) connectedHosts(ctx context.Context) ([]string, error) {
	exps, err := s.store.ListExports("", "")
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range exps {
		conns, err := dataplane.Connections(ctx, s.dp, e)
		if err != nil {
			return nil, err
		}
		for _, c := range conns {
			who := c.Host
			if who == "" {
				who = c.Address
			}
			out = append(out, e.Name+": "+who)
		}
	}
	return out, nil
}

func (s *Server) handleRebootSystem(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Force bool `json:"force"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if j := s.os.snapshot(); j != nil && j.State == "RUNNING" {
		writeError(w, fmt.Errorf("%w: an OS upgrade is running; rebooting now would lose it", store.ErrInUse))
		return
	}
	if !req.Force && s.dp != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
		hosts, err := s.connectedHosts(ctx)
		cancel()
		if err != nil {
			writeError(w, fmt.Errorf("can't tell which hosts are connected (%v); send \"force\": true to reboot anyway", err))
			return
		}
		if len(hosts) > 0 {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": fmt.Sprintf("%d host connection(s) would lose their storage during the reboot: %s. Stop them first, or send \"force\": true",
					len(hosts), strings.Join(hosts, "; ")),
				"connected_hosts": hosts,
			})
			return
		}
	}
	s.logger.Warn("appliance reboot requested", "by", subjectFromContext(r.Context()), "force", req.Force)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":  "rebooting",
		"message": "Appliance reboot scheduled in 1 second",
	})
	reboot := s.os.reboot
	if reboot == nil {
		reboot = func() { _ = hostCommand(context.Background(), "systemctl", "reboot").Run() }
	}
	go func() {
		time.Sleep(1 * time.Second)
		reboot()
	}()
}
