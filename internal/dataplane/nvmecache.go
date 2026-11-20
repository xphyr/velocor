package dataplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/velocor/velocor/internal/spdk"
	"github.com/velocor/velocor/internal/store"
)

// The volatile write cache of NVMe drives SPDK owns (docs/review-history.md P0-4, steps 1 and 4 for NVMe).
// The kernel can't see a drive bound to vfio-pci, so the cache is read and set with admin
// commands through SPDK:
//   - Identify Controller (CNS 01h): VWC (byte 525, bit 0) says whether the drive has a
//     volatile write cache at all; ONCS bit 4 whether Set Features can save a value.
//   - Get / Set Features, Volatile Write Cache (FID 06h): bit 0 of the result / CDW11 is WCE.
const (
	nvmeOpSetFeatures = 0x09
	nvmeOpGetFeatures = 0x0a
	nvmeOpIdentify    = 0x06
	nvmeFeatureVWC    = 0x06
	nvmeSaveBit       = 1 << 31

	nvmeSCTCommandSpecific   = 0x1
	nvmeSCFeatureNotSaveable = 0x0d
)

// nvmeCacheCaps is what Identify Controller says about the write cache.
type nvmeCacheCaps struct {
	present  bool // VWC: the drive has a volatile write cache
	saveable bool // ONCS bit 4: Set Features can save the value across power cycles
}

func nvmeIdentifyCache(ctx context.Context, c *spdk.Client, ctrl string) (nvmeCacheCaps, error) {
	_, data, err := c.BdevNvmeAdminCmd(ctx, ctrl, spdk.NvmeCmd{Opcode: nvmeOpIdentify, CDW10: 1}, 4096)
	if err != nil {
		return nvmeCacheCaps{}, fmt.Errorf("identify controller %s: %w", ctrl, err)
	}
	if len(data) < 526 {
		return nvmeCacheCaps{}, fmt.Errorf("identify controller %s: %d bytes of data", ctrl, len(data))
	}
	return nvmeCacheCaps{present: data[525]&1 != 0, saveable: data[520]&(1<<4) != 0}, nil
}

func nvmeGetWCE(ctx context.Context, c *spdk.Client, ctrl string) (store.WriteCacheState, error) {
	cpl, _, err := c.BdevNvmeAdminCmd(ctx, ctrl, spdk.NvmeCmd{Opcode: nvmeOpGetFeatures, CDW10: nvmeFeatureVWC}, 0)
	if err != nil {
		return store.WriteCacheUnknown, fmt.Errorf("get the write cache feature of %s: %w", ctrl, err)
	}
	if cpl.CDW0&1 != 0 {
		return store.WriteCacheWriteBack, nil
	}
	return store.WriteCacheWriteThrough, nil
}

// NVMeWriteCache returns the write cache state of an NVMe controller SPDK owns: WRITE_THROUGH
// for a drive without a volatile cache or with it switched off, WRITE_BACK when it is on.
func NVMeWriteCache(ctx context.Context, c *spdk.Client, ctrl string) (store.WriteCacheState, error) {
	caps, err := nvmeIdentifyCache(ctx, c, ctrl)
	if err != nil {
		return store.WriteCacheUnknown, err
	}
	if !caps.present {
		return store.WriteCacheWriteThrough, nil
	}
	return nvmeGetWCE(ctx, c, ctrl)
}

// SetNVMeWriteCache switches the volatile write cache of an NVMe controller SPDK owns on or
// off and returns the state the drive reports afterwards. The value is saved across power
// cycles when the drive supports that (the reconciler re-applies it either way).
func SetNVMeWriteCache(ctx context.Context, c *spdk.Client, ctrl string, enabled bool) (store.WriteCacheState, error) {
	caps, err := nvmeIdentifyCache(ctx, c, ctrl)
	if err != nil {
		return store.WriteCacheUnknown, err
	}
	if !caps.present {
		return store.WriteCacheWriteThrough, nil // nothing to switch either way
	}
	cdw11 := uint32(0)
	if enabled {
		cdw11 = 1
	}
	cdw10 := uint32(nvmeFeatureVWC)
	if caps.saveable {
		cdw10 |= nvmeSaveBit
	}
	_, _, err = c.BdevNvmeAdminCmd(ctx, ctrl, spdk.NvmeCmd{Opcode: nvmeOpSetFeatures, CDW10: cdw10, CDW11: cdw11}, 0)
	var se *spdk.NvmeStatusError
	if errors.As(err, &se) && cdw10&nvmeSaveBit != 0 && se.Cpl.SCT == nvmeSCTCommandSpecific && se.Cpl.SC == nvmeSCFeatureNotSaveable {
		// Advertised as saveable, but not this feature: set it for now.
		_, _, err = c.BdevNvmeAdminCmd(ctx, ctrl, spdk.NvmeCmd{Opcode: nvmeOpSetFeatures, CDW10: nvmeFeatureVWC, CDW11: cdw11}, 0)
	}
	if err != nil {
		state, _ := nvmeGetWCE(ctx, c, ctrl)
		return state, fmt.Errorf("%s refused to switch its write cache %s: %w", ctrl, map[bool]string{true: "on", false: "off"}[enabled], err)
	}
	state, err := nvmeGetWCE(ctx, c, ctrl)
	if err != nil {
		return state, err
	}
	if want := map[bool]store.WriteCacheState{true: store.WriteCacheWriteBack, false: store.WriteCacheWriteThrough}[enabled]; state != want {
		return state, fmt.Errorf("the write cache of %s is %s after switching it %s: the drive did not accept the change", ctrl, state, map[bool]string{true: "on", false: "off"}[enabled])
	}
	return state, nil
}
