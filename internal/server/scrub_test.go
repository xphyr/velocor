package server

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/store"
)

func scrubPool(t *testing.T) (*harness, string, string) {
	t.Helper()
	h := newHarness(t)
	h.addDisk("d1", "0000:01:00.0")
	h.addDisk("d2", "0000:02:00.0")
	id := h.pool("p", "RAID1", "d1", "d2")
	h.reconcile() // the scheduler waits for velod's first pass
	p, err := h.st.GetPool(id)
	if err != nil {
		t.Fatal(err)
	}
	return h, id, p.ArrayName
}

func (h *harness) tick(at time.Time) {
	h.t.Helper()
	if err := h.srv.scrubTick(context.Background(), at); err != nil {
		h.t.Fatalf("scrub tick: %v", err)
	}
}

func (h *harness) scrubRecord(poolID string) *store.ScrubRecord {
	h.t.Helper()
	rec, err := h.st.GetScrubRecord(poolID)
	if err != nil {
		h.t.Fatalf("scrub record: %v", err)
	}
	return rec
}

// A scheduled run starts once the pool is due, pauses when the window ends, resumes where it
// was in the next window, completes, and isn't due again before the interval.
func TestScrubScheduleWindowAndResume(t *testing.T) {
	h, id, raid := scrubPool(t)
	anyTime := map[string]any{"interval_days": 1, "window_start_hour": 0, "window_end_hour": 0, "max_mb_per_sec": 50}
	got := h.expect(h.do("PUT", "/api/v1/system/scrub", h.admin, anyTime))(http.StatusOK)["scrub"].(map[string]any)
	if got["interval_days"] != float64(1) || got["max_mb_per_sec"] != float64(50) || got["disabled"] != false {
		t.Fatalf("scrub settings: %v", got)
	}
	h.expect(h.do("PUT", "/api/v1/system/scrub", h.admin, map[string]any{"window_start_hour": 24}))(http.StatusBadRequest)
	h.expect(h.do("PUT", "/api/v1/system/scrub", h.admin, map[string]any{"interval_days": 0}))(http.StatusBadRequest)

	now := time.Now()
	h.tick(now) // the pool was just created: not due
	if starts := h.fake.ScrubStarts(raid); len(starts) != 0 {
		t.Fatalf("a new pool was scrubbed at once: %v", starts)
	}

	h.fake.SetScrubStep(1 << 18) // a quarter per listing of the arrays
	now = now.Add(25 * time.Hour)
	h.tick(now)
	if starts := h.fake.ScrubStarts(raid); !slices.Equal(starts, []uint64{0}) {
		t.Fatalf("due pool: scrub starts %v, want one at 0", starts)
	}
	rec := h.scrubRecord(id)
	if rec.State != store.ScrubRunning || rec.Manual || !rec.Repair || rec.MaxMBps != 50 {
		t.Fatalf("scheduled run: %+v", rec)
	}
	h.fake.SetScrubFindings(raid, 2, 3, 0)
	h.tick(now)

	// The window ends: the run pauses (once the data plane reports it stopped) and keeps its findings.
	hour := now.Local().Hour()
	h.expect(h.do("PUT", "/api/v1/system/scrub", h.admin, map[string]any{"window_start_hour": (hour + 2) % 24, "window_end_hour": (hour + 3) % 24}))(http.StatusOK)
	h.tick(now)
	h.tick(now)
	rec = h.scrubRecord(id)
	if rec.State != store.ScrubPaused || rec.OffsetBlocks == 0 || rec.OffsetBlocks >= rec.TotalBlocks || rec.Counts.RepairedBlocks != 2 || rec.Counts.MismatchedBlocks != 3 {
		t.Fatalf("after the window: %+v", rec)
	}
	paused := rec.OffsetBlocks
	h.tick(now) // still outside the window: stays paused
	if n := len(h.fake.ScrubStarts(raid)); n != 1 {
		t.Fatalf("scrub restarted outside its window (%d starts)", n)
	}

	// Next window: it resumes at the offset it reached and completes.
	h.expect(h.do("PUT", "/api/v1/system/scrub", h.admin, anyTime))(http.StatusOK)
	h.tick(now)
	if starts := h.fake.ScrubStarts(raid); !slices.Equal(starts, []uint64{0, paused}) {
		t.Fatalf("resume: scrub starts %v, want [0 %d]", starts, paused)
	}
	for i := 0; i < 6 && h.scrubRecord(id).State == store.ScrubRunning; i++ {
		h.tick(now)
	}
	rec = h.scrubRecord(id)
	if rec.State != store.ScrubCompleted || rec.LastCompleted == nil || rec.LastCompleted.RepairedBlocks != 2 || rec.LastCompleted.MismatchRepairedBlocks != 3 || rec.LastCompletedAt.IsZero() {
		t.Fatalf("completed run: %+v", rec)
	}
	h.tick(now.Add(time.Hour)) // not due again for a day
	if n := len(h.fake.ScrubStarts(raid)); n != 2 {
		t.Fatalf("completed pool scrubbed again before its interval (%d starts)", n)
	}

	m := h.metrics()
	for _, want := range []string{
		`velocor_pool_scrub_running{pool="p"} 0`,
		`velocor_pool_scrub_repaired_blocks{pool="p"} 2`,
		`velocor_pool_scrub_mismatched_blocks{pool="p"} 3`,
		`velocor_pool_scrub_unrecoverable_blocks{pool="p"} 0`,
		`velocor_pool_scrub_overdue{pool="p"} 0`,
		`velocor_pool_scrub_last_completed_timestamp_seconds{pool="p"}`,
	} {
		if !strings.Contains(m, want) {
			t.Errorf("metrics lack %s", want)
		}
	}

	// Turned off: nothing starts even when due.
	h.expect(h.do("PUT", "/api/v1/system/scrub", h.admin, map[string]any{"disabled": true}))(http.StatusOK)
	h.tick(now.Add(72 * time.Hour))
	if n := len(h.fake.ScrubStarts(raid)); n != 2 {
		t.Fatalf("disabled schedule started a scrub (%d starts)", n)
	}
}

// An operator's scrub: refused while one runs, resumed by itself after a data-plane restart or a
// member removal (once the pool is ONLINE), and ended by a stop.
func TestScrubManualInterruptionsAndStop(t *testing.T) {
	h, id, raid := scrubPool(t)
	h.fake.SetScrubStep(1 << 18)

	resp := h.expect(h.do("POST", "/api/v1/pools/"+id+"/scrub", h.admin, map[string]any{"repair": false, "max_mb_per_sec": 10}))(http.StatusAccepted)
	if sc := resp["scrub"].(map[string]any); sc["state"] != "RUNNING" || sc["manual"] != true || sc["repair"] != false {
		t.Fatalf("manual scrub: %v", resp)
	}
	h.expect(h.do("POST", "/api/v1/pools/"+id+"/scrub", h.admin, nil))(http.StatusConflict)
	now := time.Now()
	h.tick(now)

	// A reboot: the scrub is gone; velod notices and resumes it where it was.
	reached := h.scrubRecord(id).OffsetBlocks
	if reached == 0 {
		t.Fatal("scrub made no progress")
	}
	h.fake.ForgetScrubs()
	h.tick(now)
	if starts := h.fake.ScrubStarts(raid); !slices.Equal(starts, []uint64{0, reached}) {
		t.Fatalf("after a data-plane restart: starts %v, want [0 %d]", starts, reached)
	}
	if rec := h.scrubRecord(id); rec.State != store.ScrubRunning || rec.Repair || rec.MaxMBps != 10 {
		t.Fatalf("resumed run lost its options: %+v", rec)
	}

	// An operator stop ends the run once the data plane reports it stopped; it isn't resumed.
	h.expect(h.do("DELETE", "/api/v1/pools/"+id+"/scrub", h.admin, nil))(http.StatusAccepted)
	h.tick(now)
	rec := h.scrubRecord(id)
	if rec.State != store.ScrubStopped || rec.FinishedAt.IsZero() || !strings.Contains(rec.Note, "operator") {
		t.Fatalf("stopped run: %+v", rec)
	}
	h.tick(now)
	if n := len(h.fake.ScrubStarts(raid)); n != 2 {
		t.Fatalf("stopped run resumed (%d starts)", n)
	}
	h.expect(h.do("DELETE", "/api/v1/pools/"+id+"/scrub", h.admin, nil))(http.StatusBadRequest)

	// A member leaves: the run pauses, and isn't resumed while the pool is degraded.
	h.expect(h.do("POST", "/api/v1/pools/"+id+"/scrub", h.admin, nil))(http.StatusAccepted)
	h.fake.FailDisk("0000:02:00.0")
	// The scrub notices before the reconciler has recorded the failed member (as on the lab
	// appliance, where a scrub's failed repair write failed the member): the pool still says
	// ONLINE, but the array's member count must keep the run paused.
	h.tick(now)
	rec = h.scrubRecord(id)
	if rec.State != store.ScrubPaused || !strings.Contains(rec.Note, "member") {
		t.Fatalf("after a member left: %+v", rec)
	}
	h.tick(now)
	if n := len(h.fake.ScrubStarts(raid)); n != 3 {
		t.Fatalf("scrub resumed on a degraded RAID before the reconciler noticed (%d starts)", n)
	}
	h.reconcile()
	h.tick(now)
	if n := len(h.fake.ScrubStarts(raid)); n != 3 {
		t.Fatalf("scrub resumed on a degraded pool (%d starts)", n)
	}
	view := h.expect(h.do("GET", "/api/v1/pools/"+id+"/scrub", h.admin, nil))(http.StatusOK)
	if !strings.Contains(strings.ToLower(view["unavailable"].(string)), "degraded") {
		t.Fatalf("pool scrub view: %v", view)
	}
	h.expect(h.do("POST", "/api/v1/pools/"+id+"/scrub", h.admin, nil))(http.StatusConflict)
}

// Deleting a pool drops its scrub record.
func TestScrubRecordGoesWithThePool(t *testing.T) {
	h, id, _ := scrubPool(t)
	h.expect(h.do("POST", "/api/v1/pools/"+id+"/scrub", h.admin, nil))(http.StatusAccepted)
	h.tick(time.Now())
	h.expect(h.do("DELETE", "/api/v1/pools/"+id, h.admin, nil))(http.StatusOK)
	if _, err := h.st.GetScrubRecord(id); err == nil {
		t.Fatal("scrub record outlived its pool")
	}
}

// A RAID0 pool (nothing to scrub) is never picked nor overdue, and a pool whose scrub won't start
// doesn't keep the others from theirs.
func TestScrubOfOnePoolDoesntStarveTheOthers(t *testing.T) {
	h := newHarness(t)
	for i, d := range []string{"d1", "d2", "d3", "d4", "d5"} {
		h.addDisk(d, fmt.Sprintf("0000:0%d:00.0", i+1))
	}
	h.pool("stripe", "RAID0", "d1")
	first, second := h.pool("m1", "RAID1", "d2", "d3"), h.pool("m2", "RAID1", "d4", "d5")
	h.reconcile()
	h.expect(h.do("PUT", "/api/v1/system/scrub", h.admin, map[string]any{"interval_days": 1, "window_start_hour": 0, "window_end_hour": 0}))(http.StatusOK)
	now := time.Now().Add(72 * time.Hour)
	h.fake.Fail("StartScrub", 1)
	if err := h.srv.scrubTick(context.Background(), now); err == nil {
		t.Error("the scrub that didn't start isn't reported")
	}
	r1, err1 := h.st.GetScrubRecord(first)
	r2, err2 := h.st.GetScrubRecord(second)
	if err2 != nil || r2.State != store.ScrubRunning {
		t.Fatalf("the second mirror: %+v %v (first: %+v %v)", r2, err2, r1, err1)
	}
	if m := h.metrics(); strings.Contains(m, `velocor_pool_scrub_overdue{pool="stripe"} 1`) {
		t.Error("a RAID0 pool is overdue for a scrub")
	}
	p, _ := h.st.ListPools()
	for _, x := range p {
		if x.Name == "stripe" {
			if code, body := h.do("POST", "/api/v1/pools/"+x.ID+"/scrub", h.admin, map[string]any{}); code != http.StatusBadRequest && code != http.StatusConflict {
				t.Errorf("scrub of a RAID0 pool: %d %v", code, body)
			}
		}
	}
}
