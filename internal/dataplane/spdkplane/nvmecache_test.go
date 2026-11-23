package spdkplane

import (
	"context"
	"testing"

	"github.com/velocor/velocor/internal/spdk/spdktest"
	"github.com/velocor/velocor/internal/store"
)

func TestNVMeWriteCacheThroughSPDK(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name      string
		cache     spdktest.NVMeCache
		before    store.WriteCacheState
		after     store.WriteCacheState
		saved     bool
		wantError bool
	}{
		{name: "no volatile cache", cache: spdktest.NVMeCache{}, before: store.WriteCacheWriteThrough, after: store.WriteCacheWriteThrough},
		{name: "cache on, saveable", cache: spdktest.NVMeCache{Present: true, Enabled: true, Saveable: true}, before: store.WriteCacheWriteBack, after: store.WriteCacheWriteThrough, saved: true},
		{name: "cache on, not saveable", cache: spdktest.NVMeCache{Present: true, Enabled: true}, before: store.WriteCacheWriteBack, after: store.WriteCacheWriteThrough},
		{name: "saving refused for this feature", cache: spdktest.NVMeCache{Present: true, Enabled: true, Saveable: true, NotSaveable: true}, before: store.WriteCacheWriteBack, after: store.WriteCacheWriteThrough},
		{name: "drive refuses", cache: spdktest.NVMeCache{Present: true, Enabled: true, Refuse: true}, before: store.WriteCacheWriteBack, after: store.WriteCacheWriteBack, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := spdktest.New(t)
			f.AddNVMe("0000:01:00.0")
			f.SetNVMeCache("0000:01:00.0", tc.cache)
			c := f.Client()
			if _, err := c.BdevNvmeAttachController(ctx, NVMeControllerName("0000:01:00.0"), "PCIe", "0000:01:00.0"); err != nil {
				t.Fatal(err)
			}
			ctrl := NVMeControllerName("0000:01:00.0")
			if got, err := NVMeWriteCache(ctx, c, ctrl); err != nil || got != tc.before {
				t.Fatalf("before: %s, %v; want %s", got, err, tc.before)
			}
			got, err := SetNVMeWriteCache(ctx, c, ctrl, false)
			if (err != nil) != tc.wantError || got != tc.after {
				t.Fatalf("switch off: %s, %v; want %s (error %v)", got, err, tc.after, tc.wantError)
			}
			if f.NVMeCacheOf("0000:01:00.0").Saved != tc.saved {
				t.Fatalf("saved = %v, want %v", !tc.saved, tc.saved)
			}
			// Leaving the drive alone again switches a cache it has back on.
			if tc.cache.Present && !tc.cache.Refuse {
				if got, err := SetNVMeWriteCache(ctx, c, ctrl, true); err != nil || got != store.WriteCacheWriteBack {
					t.Fatalf("switch on: %s, %v", got, err)
				}
			}
		})
	}
}
