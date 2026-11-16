package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/velocor/velocor/internal/store"
)

// OSDeployment describes a single bootc OS deployment entry (booted, staged, or rollback).
type OSDeployment struct {
	Image       string `json:"image,omitempty"`
	Version     string `json:"version,omitempty"`
	ImageDigest string `json:"image_digest,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
}

// OSStatus summarizes the appliance's Fedora Image Mode (bootc) state.
type OSStatus struct {
	Available bool          `json:"available"`
	Message   string        `json:"message,omitempty"`
	Booted    *OSDeployment `json:"booted,omitempty"`
	Staged    *OSDeployment `json:"staged,omitempty"`
	Rollback  *OSDeployment `json:"rollback,omitempty"`
}

// ContainerUpdateEntry describes a running Quadlet container and whether a registry update is available.
type ContainerUpdateEntry struct {
	Unit            string `json:"unit"`
	ContainerName   string `json:"container_name"`
	Image           string `json:"image"`
	Policy          string `json:"policy,omitempty"`
	RunningStatus   string `json:"running_status,omitempty"`
	UpdateStatus    string `json:"update_status"`
	UpdateAvailable bool   `json:"update_available"`
}

// ContainerUpdatesStatus summarizes the container update check and latest update job state.
type ContainerUpdatesStatus struct {
	Available         bool                   `json:"available"`
	Message           string                 `json:"message,omitempty"`
	CheckedAt         time.Time              `json:"checked_at"`
	UpdatesAvailable  bool                   `json:"updates_available"`
	Containers        []ContainerUpdateEntry `json:"containers"`
	LastUpdateState   string                 `json:"last_update_state,omitempty"`
	LastUpdateMessage string                 `json:"last_update_message,omitempty"`
	LastUpdateAt      *time.Time             `json:"last_update_at,omitempty"`
}

var (
	lifecycleMu       sync.Mutex
	lastUpdateState   string
	lastUpdateMessage string
	lastUpdateAt      *time.Time
)

func hostCommand(ctx context.Context, name string, args ...string) *exec.Cmd {
	if isInContainer() {
		if _, err := exec.LookPath("nsenter"); err == nil {
			nsArgs := append([]string{"-t", "1", "-m", "-u", "-i", "-n", "--", "env", "-u", "container", name}, args...)
			return exec.CommandContext(ctx, "nsenter", nsArgs...)
		}
	}
	return exec.CommandContext(ctx, name, args...)
}

func isInContainer() bool {
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	return false
}

func (s *Server) handleGetOSStatus(w http.ResponseWriter, r *http.Request) {
	status := queryBootcStatus(r.Context())
	writeJSON(w, http.StatusOK, status)
}

func queryBootcStatus(ctx context.Context) OSStatus {
	out, err := hostCommand(ctx, "bootc", "status", "--json").CombinedOutput()
	if err != nil {
		return OSStatus{
			Available: false,
			Message:   strings.TrimSpace(fmt.Sprintf("bootc unavailable: %v (%s)", err, strings.TrimSpace(string(out)))),
		}
	}

	var raw struct {
		Status struct {
			Booted   *rawBootcSlot `json:"booted"`
			Staged   *rawBootcSlot `json:"staged"`
			Rollback *rawBootcSlot `json:"rollback"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return OSStatus{
			Available: false,
			Message:   fmt.Sprintf("failed to parse bootc status: %v", err),
		}
	}

	return OSStatus{
		Available: true,
		Booted:    convertBootcSlot(raw.Status.Booted),
		Staged:    convertBootcSlot(raw.Status.Staged),
		Rollback:  convertBootcSlot(raw.Status.Rollback),
	}
}

type rawBootcSlot struct {
	Image *struct {
		Image struct {
			Image string `json:"image"`
		} `json:"image"`
		Version     string `json:"version"`
		ImageDigest string `json:"imageDigest"`
		Timestamp   string `json:"timestamp"`
	} `json:"image"`
}

func convertBootcSlot(slot *rawBootcSlot) *OSDeployment {
	if slot == nil || slot.Image == nil {
		return nil
	}
	return &OSDeployment{
		Image:       slot.Image.Image.Image,
		Version:     slot.Image.Version,
		ImageDigest: slot.Image.ImageDigest,
		Timestamp:   slot.Image.Timestamp,
	}
}

func (s *Server) handleStageUpgrade(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ImageRef string `json:"image_ref"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	imageRef := strings.TrimSpace(req.ImageRef)

	var cmd *exec.Cmd
	if imageRef == "" {
		cmd = hostCommand(r.Context(), "bootc", "upgrade")
	} else {
		cmd = hostCommand(r.Context(), "bootc", "switch", imageRef)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		writeError(w, fmt.Errorf("bootc update failed: %v (%s)", err, strings.TrimSpace(string(out))))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"staged_image_ref": imageRef,
		"output":           strings.TrimSpace(string(out)),
		"reboot_required":  true,
	})
}

func (s *Server) handleRollbackUpgrade(w http.ResponseWriter, r *http.Request) {
	out, err := hostCommand(r.Context(), "bootc", "rollback").CombinedOutput()
	if err != nil {
		writeError(w, fmt.Errorf("bootc rollback failed: %v (%s)", err, strings.TrimSpace(string(out))))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"output":          strings.TrimSpace(string(out)),
		"reboot_required": true,
	})
}

func (s *Server) handleRebootSystem(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":  "rebooting",
		"message": "Appliance reboot scheduled in 1 second",
	})
	go func() {
		time.Sleep(1 * time.Second)
		_ = hostCommand(context.Background(), "systemctl", "reboot").Run()
	}()
}

func (s *Server) handleCheckContainerUpdates(w http.ResponseWriter, r *http.Request) {
	status := queryContainerUpdates(r.Context())
	writeJSON(w, http.StatusOK, status)
}

func queryContainerUpdates(ctx context.Context) ContainerUpdatesStatus {
	lifecycleMu.Lock()
	stateCopy := lastUpdateState
	msgCopy := lastUpdateMessage
	atCopy := lastUpdateAt
	lifecycleMu.Unlock()

	res := ContainerUpdatesStatus{
		Available:         true,
		CheckedAt:         time.Now().UTC(),
		Containers:        []ContainerUpdateEntry{},
		LastUpdateState:   stateCopy,
		LastUpdateMessage: msgCopy,
		LastUpdateAt:      atCopy,
	}

	psOut, psErr := hostCommand(ctx, "podman", "ps", "--format", "json").CombinedOutput()
	if psErr != nil {
		res.Available = false
		res.Message = strings.TrimSpace(fmt.Sprintf("podman unavailable: %v (%s)", psErr, strings.TrimSpace(string(psOut))))
		return res
	}

	runningMap := map[string]struct {
		Image  string
		Status string
	}{}
	var psItems []struct {
		Names  []string `json:"Names"`
		Image  string   `json:"Image"`
		Status string   `json:"Status"`
	}
	if err := json.Unmarshal(psOut, &psItems); err == nil {
		for _, item := range psItems {
			for _, name := range item.Names {
				runningMap[name] = struct {
					Image  string
					Status string
				}{Image: item.Image, Status: item.Status}
			}
		}
	}

	auOut, auErr := hostCommand(ctx, "podman", "auto-update", "--dry-run", "--format", "json").CombinedOutput()
	if auErr != nil {
		// Fall back to listing running velocor containers if auto-update dry-run fails.
		for name, info := range runningMap {
			if strings.HasPrefix(name, "velocor-") {
				res.Containers = append(res.Containers, ContainerUpdateEntry{
					Unit:          name + ".service",
					ContainerName: name,
					Image:         info.Image,
					RunningStatus: info.Status,
					UpdateStatus:  "check-failed",
				})
			}
		}
		res.Message = strings.TrimSpace(string(auOut))
		return res
	}

	var auItems []struct {
		Unit      string `json:"Unit"`
		Container string `json:"ContainerName"`
		Image     string `json:"Image"`
		Policy    string `json:"Policy"`
		Updated   string `json:"Updated"`
	}
	if err := json.Unmarshal(auOut, &auItems); err == nil {
		for _, item := range auItems {
			avail := strings.EqualFold(item.Updated, "pending")
			if avail {
				res.UpdatesAvailable = true
			}
			runInfo := runningMap[item.Container]
			res.Containers = append(res.Containers, ContainerUpdateEntry{
				Unit:            item.Unit,
				ContainerName:   item.Container,
				Image:           item.Image,
				Policy:          item.Policy,
				RunningStatus:   runInfo.Status,
				UpdateStatus:    item.Updated,
				UpdateAvailable: avail,
			})
		}
	}
	return res
}

// containerUnitRe restricts container updates to Velocor's own quadlet units. The unit name
// reaches systemctl/podman on the host, so it must never be free-form.
var containerUnitRe = regexp.MustCompile(`^velocor-[a-z0-9][a-z0-9-]{0,62}\.service$`)

// dataPlaneUnit is restarted only with explicit consent: it interrupts all SAN I/O.
const dataPlaneUnit = "velocor-spdk.service"

// updateScript pulls the image of container $1 and restarts unit $2. Values are passed as
// positional parameters, never interpolated into the script text.
const updateScript = `img="$(podman inspect --format '{{.ImageName}}' "$1" 2>/dev/null || true)"; if [ -n "$img" ]; then podman pull "$img"; fi; systemctl restart "$2"`

func (s *Server) handleApplyContainerUpdates(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Unit            string `json:"unit"`
		AllowDisruptive bool   `json:"allow_disruptive"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, fmt.Errorf("%w: invalid json body", store.ErrInvalidArgument))
			return
		}
	}
	unit := strings.TrimSpace(req.Unit)

	if unit != "" && !containerUnitRe.MatchString(unit) {
		writeError(w, fmt.Errorf("%w: unit must be a velocor-*.service container unit", store.ErrInvalidArgument))
		return
	}
	if unit == dataPlaneUnit && !req.AllowDisruptive {
		writeError(w, fmt.Errorf("%w: updating %s restarts spdk_tgt and interrupts all SAN I/O; resend with allow_disruptive=true during a maintenance window",
			store.ErrInvalidArgument, dataPlaneUnit))
		return
	}

	now := time.Now().UTC()
	lifecycleMu.Lock()
	lastUpdateState = "running"
	lastUpdateMessage = "Applying container updates..."
	lastUpdateAt = &now
	lifecycleMu.Unlock()

	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":  "updating",
		"unit":    unit,
		"message": "Container update triggered; services may briefly restart.",
	})

	// Delay briefly so the HTTP response flushes before velod itself restarts.
	go func() {
		time.Sleep(500 * time.Millisecond)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		var out []byte
		var err error
		if unit == "" {
			// Run podman auto-update in its own transient host systemd unit so restarting
			// velocor-velod.service does not kill the updater mid-flight.
			out, err = hostCommand(ctx, "systemd-run", "--no-block", "--collect",
				"--unit=velocor-container-update",
				"podman", "auto-update").CombinedOutput()
			if err != nil {
				out, err = hostCommand(ctx, "podman", "auto-update", "--format", "json").CombinedOutput()
			}
		} else {
			containerName := strings.TrimSuffix(unit, ".service")
			out, err = hostCommand(ctx, "systemd-run", "--no-block", "--collect",
				"--unit=velocor-container-update",
				"/bin/sh", "-c", updateScript, "velocor-update", containerName, unit).CombinedOutput()
			if err != nil {
				out, err = hostCommand(ctx, "/bin/sh", "-c", updateScript, "velocor-update", containerName, unit).CombinedOutput()
			}
		}

		doneAt := time.Now().UTC()
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		lastUpdateAt = &doneAt
		if err != nil {
			lastUpdateState = "failed"
			lastUpdateMessage = strings.TrimSpace(fmt.Sprintf("%v: %s", err, strings.TrimSpace(string(out))))
		} else {
			lastUpdateState = "completed"
			lastUpdateMessage = strings.TrimSpace(string(out))
		}
	}()
}
