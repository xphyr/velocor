package reconciler

import (
	"strings"
	"testing"

	"github.com/velocor/velocor/internal/dataplane/planetest"
	"github.com/velocor/velocor/internal/store"
)

// The reconciler reads the write cache of NVMe drives, keeps it off where the policy says so, and rates the pool by its weakest member (docs/review-history.md P0-4).
func TestNVMeWriteCacheKeptOffAndPoolDurability(t *testing.T) {
	e := newEnv(t)
	e.fake.SetNVMeCache("0000:01:00.0", planetest.NVMeCache{Present: true, Enabled: true, Saveable: true})
	e.fake.SetNVMeCache("0000:02:00.0", planetest.NVMeCache{})
	pool, _ := e.provision(store.RaidLevel1, "0000:01:00.0", "0000:02:00.0")

	e.reconcile()
	if d, _ := e.st.GetDisk("disk-a"); d.WriteCache != store.WriteCacheWriteBack {
		t.Fatalf("disk-a write cache: %s, want WRITE_BACK read from the drive", d.WriteCache)
	}
	if d, _ := e.st.GetDisk("disk-b"); d.WriteCache != store.WriteCacheWriteThrough {
		t.Fatalf("disk-b (no volatile cache): %s", d.WriteCache)
	}
	if p := e.pool(); p.Durability != store.DurabilityAtRisk {
		t.Fatalf("pool durability %s, want AT_RISK", p.Durability)
	}

	if _, err := e.st.MutateDisk("disk-a", func(d *store.Disk) error { d.WriteCachePolicy = store.WriteCachePolicyDisabled; return nil }); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if c := e.fake.NVMeCacheOf("0000:01:00.0"); c.Enabled || !c.Saved {
		t.Fatalf("drive cache after the policy: %+v, want off and saved", c)
	}
	if p := e.pool(); p.Durability != store.DurabilityDurable {
		t.Fatalf("pool durability %s, want DURABLE", p.Durability)
	}

	// The drive turns it back on (power cycle): switched off again on the next pass.
	e.fake.SetNVMeCache("0000:01:00.0", planetest.NVMeCache{Present: true, Enabled: true, Saveable: true})
	e.reconcile()
	if e.fake.NVMeCacheOf("0000:01:00.0").Enabled {
		t.Fatal("a re-enabled cache was not switched off again")
	}

	// A drive that refuses: recorded, not retried, pool at risk, pass clean.
	e.fake.SetNVMeCache("0000:01:00.0", planetest.NVMeCache{Present: true, Enabled: true, Refuse: true})
	st := e.reconcile()
	d, _ := e.st.GetDisk("disk-a")
	if !strings.Contains(d.WriteCacheError, "refused") || strings.Contains(strings.Join(st.DegradedResources, " "), "disk-a") {
		t.Fatalf("refusal: %q, degraded %v", d.WriteCacheError, st.DegradedResources)
	}
	if p := e.pool(); p.Durability != store.DurabilityAtRisk || p.ID != pool.ID {
		t.Fatalf("pool durability %s, want AT_RISK", p.Durability)
	}
	before := e.fake.CallCount("SetNVMeWriteCache")
	e.reconcile()
	if n := e.fake.CallCount("SetNVMeWriteCache") - before; n != 0 {
		t.Fatalf("the refused drive was retried on the next pass (%d times)", n)
	}
}
