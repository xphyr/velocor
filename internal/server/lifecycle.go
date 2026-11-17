package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/velocor/velocor/internal/backup"
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

// ContainerInfo describes one of the appliance's quadlet containers. Containers change only
// with the OS image (`bootc upgrade`), which embeds them pinned by digest, so this is read-only.
type ContainerInfo struct {
	Unit          string `json:"unit"`
	ContainerName string `json:"container_name"`
	Image         string `json:"image"`
	ImageID       string `json:"image_id,omitempty"`
	RunningStatus string `json:"running_status,omitempty"`
}

// ContainersStatus lists the running Velocor containers.
type ContainersStatus struct {
	Available  bool            `json:"available"`
	Message    string          `json:"message,omitempty"`
	Containers []ContainerInfo `json:"containers"`
}

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
	// The reference reaches bootc's command line: anything that could be read as an option
	// (or carries whitespace / control characters) is refused.
	if imageRef != "" && !imageRefRe.MatchString(imageRef) {
		writeError(w, fmt.Errorf("%w: %q is not a container image reference", store.ErrInvalidArgument, imageRef))
		return
	}

	if s.backups != nil {
		if _, err := s.backups.Take(backup.KindPreUpgrade); err != nil {
			writeError(w, fmt.Errorf("back up the database before the upgrade: %w", err))
			return
		}
	}
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

func (s *Server) handleListContainers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, listContainers(r.Context()))
}

func listContainers(ctx context.Context) ContainersStatus {
	res := ContainersStatus{Available: true, Containers: []ContainerInfo{}}
	out, err := hostCommand(ctx, "podman", "ps", "--all", "--format", "json").CombinedOutput()
	if err != nil {
		res.Available = false
		res.Message = strings.TrimSpace(fmt.Sprintf("podman unavailable: %v (%s)", err, strings.TrimSpace(string(out))))
		return res
	}
	var items []struct {
		Names   []string `json:"Names"`
		Image   string   `json:"Image"`
		ImageID string   `json:"ImageID"`
		Status  string   `json:"Status"`
	}
	if err := json.Unmarshal(out, &items); err != nil {
		res.Available = false
		res.Message = "unexpected podman output: " + err.Error()
		return res
	}
	for _, item := range items {
		for _, name := range item.Names {
			if strings.HasPrefix(name, "velocor-") {
				res.Containers = append(res.Containers, ContainerInfo{
					Unit: name + ".service", ContainerName: name, Image: item.Image,
					ImageID: item.ImageID, RunningStatus: item.Status,
				})
			}
		}
	}
	sort.Slice(res.Containers, func(i, j int) bool { return res.Containers[i].Unit < res.Containers[j].Unit })
	return res
}

// imageRefRe accepts registry/repo[:tag][@digest] references, optionally with a bootc
// transport prefix (registry:, oci:, containers-storage:), and never a leading '-'.
var imageRefRe = regexp.MustCompile(`^(?:(?:registry|oci|oci-archive|containers-storage):)?[a-zA-Z0-9][a-zA-Z0-9._/:@+-]{0,511}$`)
