package kernelplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/velocor/velocor/internal/store"
)

// NVMeHealth reads the drive's SMART log with nvme-cli.
func (p *Plane) NVMeHealth(ctx context.Context, d *store.Disk) (*store.SmartHealth, error) {
	ctrl, err := p.nvmeController(d)
	if err != nil {
		return nil, err
	}
	if st := p.nvmeState(ctrl); st.health != nil {
		h := *st.health
		return &h, nil
	}
	out, err := p.k.Run.Run(ctx, "nvme", "smart-log", ctrl, "-o", "json")
	if err != nil {
		return nil, err
	}
	h, err := parseSmartLog(out)
	if err != nil {
		return nil, err
	}
	keep := *h
	p.keepNVMe(ctrl, func(st *nvmeState) { st.health = &keep })
	return h, nil
}

// nvmeTTL is how long a drive's SMART log and write-cache state are reused: they are read on every
// reconciler pass, and each read is an admin command through nvme-cli.
const nvmeTTL = time.Minute

type nvmeState struct {
	at     time.Time
	health *store.SmartHealth
	cache  store.WriteCacheState
}

func (p *Plane) nvmeState(ctrl string) nvmeState {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.nvme[ctrl]; time.Since(st.at) < nvmeTTL {
		return st
	}
	return nvmeState{}
}

func (p *Plane) keepNVMe(ctrl string, set func(*nvmeState)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.nvme[ctrl]
	if time.Since(st.at) >= nvmeTTL {
		st = nvmeState{at: time.Now()}
	}
	set(&st)
	if p.nvme == nil {
		p.nvme = map[string]nvmeState{}
	}
	p.nvme[ctrl] = st
}

func parseSmartLog(out []byte) (*store.SmartHealth, error) {
	var l struct {
		CriticalWarning  uint32 `json:"critical_warning"`
		Temperature      int32  `json:"temperature"` // kelvin
		AvailSpare       uint32 `json:"avail_spare"`
		SpareThresh      uint32 `json:"spare_thresh"`
		PercentUsed      uint32 `json:"percent_used"`
		DataUnitsRead    uint64 `json:"data_units_read"`
		DataUnitsWritten uint64 `json:"data_units_written"`
		MediaErrors      uint64 `json:"media_errors"`
		PowerOnHours     uint64 `json:"power_on_hours"`
		UnsafeShutdowns  uint64 `json:"unsafe_shutdowns"`
	}
	if err := json.Unmarshal(out, &l); err != nil {
		return nil, fmt.Errorf("nvme smart-log: %w", err)
	}
	t := l.Temperature
	if t > 0 {
		t -= 273
	}
	return &store.SmartHealth{
		TemperatureCelsius:             t,
		AvailableSparePercent:          l.AvailSpare,
		AvailableSpareThresholdPercent: l.SpareThresh,
		PercentageUsed:                 l.PercentUsed,
		DataUnitsRead:                  l.DataUnitsRead,
		DataUnitsWritten:               l.DataUnitsWritten,
		MediaErrors:                    l.MediaErrors,
		PowerOnHours:                   l.PowerOnHours,
		UnsafeShutdowns:                l.UnsafeShutdowns,
		CriticalWarning:                l.CriticalWarning != 0,
	}, nil
}

// NVMeWriteCache reads the Volatile Write Cache feature (FID 06h): bit 0 is WCE.
func (p *Plane) NVMeWriteCache(ctx context.Context, d *store.Disk) (store.WriteCacheState, error) {
	ctrl, err := p.nvmeController(d)
	if err != nil {
		return store.WriteCacheUnknown, err
	}
	if st := p.nvmeState(ctrl); st.cache != "" && st.cache != store.WriteCacheUnknown {
		return st.cache, nil
	}
	out, err := p.k.Run.Run(ctx, "nvme", "get-feature", ctrl, "--feature-id=6")
	if err != nil {
		return store.WriteCacheUnknown, err
	}
	v, err := parseFeatureValue(string(out))
	if err != nil {
		return store.WriteCacheUnknown, err
	}
	state := store.WriteCacheWriteThrough
	if v&1 != 0 {
		state = store.WriteCacheWriteBack
	}
	p.keepNVMe(ctrl, func(st *nvmeState) { st.cache = state })
	return state, nil
}

// SetNVMeWriteCache sets WCE, saved across power cycles where the drive can (else for the
// current power cycle; velod sets it again at start), and returns the state read back.
func (p *Plane) SetNVMeWriteCache(ctx context.Context, d *store.Disk, enabled bool) (store.WriteCacheState, error) {
	ctrl, err := p.nvmeController(d)
	if err != nil {
		return store.WriteCacheUnknown, err
	}
	p.keepNVMe(ctrl, func(st *nvmeState) { *st = nvmeState{} }) // read it back below
	v := "--value=0"
	if enabled {
		v = "--value=1"
	}
	if _, err := p.k.Run.Run(ctx, "nvme", "set-feature", ctrl, "--feature-id=6", v, "--save"); err != nil {
		if _, err2 := p.k.Run.Run(ctx, "nvme", "set-feature", ctrl, "--feature-id=6", v); err2 != nil {
			return store.WriteCacheUnknown, errors.Join(err, err2)
		}
	}
	got, err := p.NVMeWriteCache(ctx, d)
	if err != nil {
		return got, err
	}
	want := store.WriteCacheWriteThrough
	if enabled {
		want = store.WriteCacheWriteBack
	}
	if got != want {
		return got, fmt.Errorf("write cache of %s is %q after setting it: the drive did not accept the change", ctrl, got)
	}
	return got, nil
}

// parseFeatureValue reads "get-feature:0x06 (Volatile Write Cache), Current value:0x00000001"
// (nvme-cli prints the value with or without 0x).
func parseFeatureValue(out string) (uint64, error) {
	_, v, ok := strings.Cut(out, "Current value:")
	if !ok {
		return 0, fmt.Errorf("nvme get-feature: unexpected output %q", strings.TrimSpace(out))
	}
	v, _, _ = strings.Cut(strings.TrimSpace(v), " ")
	v, _, _ = strings.Cut(v, "\n")
	v = strings.TrimPrefix(strings.TrimSpace(v), "0x")
	return strconv.ParseUint(v, 16, 64)
}
