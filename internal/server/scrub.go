package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/velocor/velocor/internal/dataplane"
	"github.com/velocor/velocor/internal/store"
)

// Background scrubs of the pools (docs/review-history.md P2-2). md does the reading, comparing and
// repairing; velod schedules it, follows it and keeps the results:
//
//   - a run covers a pool once. It can span several md checks ("segments"): a scheduled run
//     pauses when its window ends and resumes at the offset it reached in the next window; a run
//     interrupted by a restart, a member removal or a rebuild resumes the same
//     way once the pool is ONLINE again.
//   - at most one scheduled scrub runs at a time (they share the data plane's one reactor with
//     host I/O); operator-started ones run at once and ignore the window.
//   - the record of every pool's current or last run is kept in the store (bucket "scrubs").

// scrubTickInterval is how often the scheduler follows running scrubs and starts due ones.
const scrubTickInterval = 30 * time.Second

// RunScrubScheduler follows and schedules pool scrubs until ctx ends.
func (s *Server) RunScrubScheduler(ctx context.Context) {
	t := time.NewTicker(scrubTickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := s.scrubTick(ctx, time.Now()); err != nil {
				s.logger.Debug("scrub scheduler pass skipped", "error", err)
			}
		}
	}
}

func scrubCounts(sc *dataplane.Scrub) store.ScrubCounts {
	return store.ScrubCounts{
		RepairedBlocks:         sc.RepairedBlocks,
		MismatchedBlocks:       sc.MismatchedBlocks,
		MismatchRepairedBlocks: sc.MismatchRepairedBlocks,
		UnrecoverableBlocks:    sc.UnrecoverableBlocks,
		WriteErrors:            sc.WriteErrors,
	}
}

// scrubEligible reports why a pool can't be scrubbed now ("" if it can).
func scrubEligible(pool *store.Pool, rb *dataplane.Array) string {
	switch {
	case !redundant(pool.RaidLevel):
		return fmt.Sprintf("it is %s, which keeps no second copy to check against", pool.RaidLevel)
	case rb == nil:
		return "its RAID isn't in the data plane"
	case rb.State != "online":
		return "its RAID isn't online"
	case rb.Operational < rb.Slots:
		// md's count, not only the pool's health: the reconciler records a member that just
		// failed (e.g. a scrub's failed repair write) up to a pass later, and a scrub resumed in
		// between would scan a single copy and record the pool as scrubbed.
		return fmt.Sprintf("it is degraded (%d of %d members)", rb.Operational, rb.Slots)
	case rb.Rebuild != nil:
		return "it is rebuilding"
	case pool.HealthStatus != store.HealthStatusOnline:
		return fmt.Sprintf("it is %s", pool.HealthStatus)
	}
	return ""
}

// scrubTick follows every pool's scrub, pauses scheduled ones outside the window and starts the
// most overdue pool's scheduled scrub.
func (s *Server) scrubTick(ctx context.Context, now time.Time) error {
	s.scrubMu.Lock()
	defer s.scrubMu.Unlock()
	if err := s.requireDataPlane(); err != nil {
		return err
	}
	if s.reconciler != nil && s.reconciler.Restoring() {
		return errors.New("the data plane is being restored")
	}
	st, err := s.store.GetSettings()
	if err != nil {
		return err
	}
	cfg := st.Scrub.Effective()
	raids, err := s.raidsByName(ctx)
	if err != nil {
		return err
	}
	pools, err := s.store.ListPools()
	if err != nil {
		return err
	}

	recs := map[string]*store.ScrubRecord{}
	for _, p := range pools {
		rec, err := s.syncScrub(ctx, p, raids[p.ArrayName], now)
		if err != nil {
			s.logger.Warn("scrub: following pool failed", "pool", p.Name, "error", err)
			continue
		}
		recs[p.ID] = rec
	}

	inWindow := cfg.InWindow(now.Local())
	scheduledRunning := false
	for _, p := range pools {
		rec := recs[p.ID]
		if rec == nil || rec.State != store.ScrubRunning || rec.Manual {
			continue
		}
		if cfg.Disabled || !inWindow {
			note := "the scrub window ended; it resumes in the next one"
			if cfg.Disabled {
				note = "scheduled scrubs were turned off"
			}
			if err := s.requestScrubStop(ctx, p, store.ScrubPaused, note); err != nil {
				s.logger.Warn("scrub: pausing failed", "pool", p.Name, "error", err)
			}
			continue
		}
		scheduledRunning = true
	}

	// Operator runs interrupted by a restart or a member change resume at once.
	for _, p := range pools {
		rec := recs[p.ID]
		if rec != nil && rec.State == store.ScrubPaused && rec.Manual && scrubEligible(p, raids[p.ArrayName]) == "" {
			if err := s.startScrub(ctx, p, rec, true, true, rec.Repair, rec.MaxMBps, now); err != nil {
				s.logger.Warn("scrub: resuming failed", "pool", p.Name, "error", err)
			}
		}
	}

	if cfg.Disabled || !inWindow || scheduledRunning {
		return nil
	}
	type candidate struct {
		pool *store.Pool
		due  time.Time
	}
	var due []candidate
	for _, p := range pools {
		rec := recs[p.ID]
		if scrubEligible(p, raids[p.ArrayName]) != "" || (rec != nil && rec.State == store.ScrubRunning) {
			continue
		}
		at := scrubDue(p, rec, cfg)
		if rec != nil && rec.State == store.ScrubPaused {
			at = time.Time{} // an unfinished run goes first
		}
		if !at.After(now) {
			due = append(due, candidate{p, at})
		}
	}
	// One at a time, the most overdue first; a pool whose scrub won't start doesn't keep the others
	// from theirs.
	slices.SortStableFunc(due, func(a, b candidate) int { return a.due.Compare(b.due) })
	var errs []error
	for _, c := range due {
		rec := recs[c.pool.ID]
		resume := rec != nil && rec.State == store.ScrubPaused
		err := s.startScrub(ctx, c.pool, rec, resume, false, true, *cfg.MaxMBps, now)
		if err == nil {
			break
		}
		errs = append(errs, fmt.Errorf("start scheduled scrub of pool %s: %w", c.pool.Name, err))
	}
	return errors.Join(errs...)
}

// scrubDue is when a pool's next scheduled scrub is due.
func scrubDue(p *store.Pool, rec *store.ScrubRecord, cfg store.ScrubSettings) time.Time {
	interval := time.Duration(cfg.IntervalDays) * 24 * time.Hour
	if rec == nil {
		return p.CreatedAt.Add(interval)
	}
	switch rec.State {
	case store.ScrubFailed:
		return rec.FinishedAt.Add(24 * time.Hour)
	case store.ScrubStopped:
		return maxTime(rec.LastCompletedAt, rec.StartedAt).Add(interval)
	}
	if rec.LastCompletedAt.IsZero() {
		return p.CreatedAt.Add(interval)
	}
	return rec.LastCompletedAt.Add(interval)
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

func (s *Server) raidsByName(ctx context.Context) (map[string]*dataplane.Array, error) {
	list, err := s.dp.Arrays(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]*dataplane.Array, len(list))
	for i := range list {
		out[list[i].Name] = &list[i]
	}
	return out, nil
}

// syncScrub brings a pool's scrub record up to date with what md reports and returns it (nil
// if the pool was never scrubbed).
func (s *Server) syncScrub(ctx context.Context, p *store.Pool, rb *dataplane.Array, now time.Time) (*store.ScrubRecord, error) {
	rec, err := s.store.GetScrubRecord(p.ID)
	if errors.Is(err, store.ErrNotFound) {
		rec = nil
	} else if err != nil {
		return nil, err
	}
	var sc *dataplane.Scrub
	if rb != nil {
		sc = rb.Scrub
	}
	tracking := rec != nil && rec.State == store.ScrubRunning
	ours := tracking && sc != nil && sc.StartOffsetBlocks == rec.SegmentStartBlocks

	switch {
	case ours && sc.State == dataplane.ScrubRunning:
		return s.store.MutateScrubRecord(p.ID, func(r *store.ScrubRecord) error {
			r.OffsetBlocks, r.TotalBlocks, r.Segment, r.UpdatedAt = sc.OffsetBlocks, sc.TotalBlocks, scrubCounts(sc), now
			return nil
		})
	case ours:
		out, err := s.store.MutateScrubRecord(p.ID, func(r *store.ScrubRecord) error {
			r.Counts = r.Counts.Add(scrubCounts(sc))
			r.Segment = store.ScrubCounts{}
			r.OffsetBlocks, r.TotalBlocks, r.UpdatedAt = sc.OffsetBlocks, sc.TotalBlocks, now
			switch sc.State {
			case dataplane.ScrubCompleted:
				r.State, r.FinishedAt, r.Note = store.ScrubCompleted, now, ""
				counts := r.Counts
				r.LastCompletedAt, r.LastCompleted = now, &counts
			case dataplane.ScrubFailed:
				r.State, r.FinishedAt = store.ScrubFailed, now
				r.Note = fmt.Sprintf("the data plane's scrub failed (error %d)", sc.Status)
			default: // stopped
				switch {
				case r.Stopping != "" && sc.Status == dataplane.ScrubStatusCanceled:
					r.State = r.Stopping
				case sc.Status == dataplane.ScrubStatusMemberRemoved:
					r.State, r.Note = store.ScrubPaused, "a member left the RAID; the scrub resumes once the pool is ONLINE"
				case sc.Status == dataplane.ScrubStatusRebuild:
					r.State, r.Note = store.ScrubPaused, "a rebuild started; the scrub resumes after it"
				default:
					r.State, r.Note = store.ScrubPaused, fmt.Sprintf("the data plane stopped the scrub (error %d)", sc.Status)
				}
				if r.State == store.ScrubStopped {
					r.FinishedAt = now
				}
			}
			r.Stopping = ""
			return nil
		})
		if err == nil {
			s.logScrubEnd(p, out)
		}
		return out, err
	case tracking:
		// md has no scrub (or another one): the appliance restarted, or velod lost track.
		return s.store.MutateScrubRecord(p.ID, func(r *store.ScrubRecord) error {
			r.Counts = r.Counts.Add(r.Segment)
			r.Segment = store.ScrubCounts{}
			r.State, r.Stopping, r.UpdatedAt = store.ScrubPaused, "", now
			r.Note = "interrupted (the data plane restarted); the scrub resumes where it was"
			return nil
		})
	case sc != nil && sc.State == dataplane.ScrubRunning:
		// A scrub velod didn't start (e.g. by hand through sysfs): follow it.
		return s.store.MutateScrubRecord(p.ID, func(r *store.ScrubRecord) error {
			r.State, r.Manual, r.Repair, r.StartedAt, r.UpdatedAt = store.ScrubRunning, true, sc.Repair, now, now
			r.FinishedAt, r.Note, r.Stopping = time.Time{}, "started outside velod", ""
			r.SegmentStartBlocks, r.OffsetBlocks, r.TotalBlocks = sc.StartOffsetBlocks, sc.OffsetBlocks, sc.TotalBlocks
			r.Counts, r.Segment = store.ScrubCounts{}, scrubCounts(sc)
			return nil
		})
	}
	return rec, nil
}

func (s *Server) logScrubEnd(p *store.Pool, r *store.ScrubRecord) {
	c := r.Counts
	args := []any{"pool", p.Name, "state", r.State, "repaired_blocks", c.RepairedBlocks, "mismatched_blocks", c.MismatchedBlocks,
		"mismatch_repaired_blocks", c.MismatchRepairedBlocks, "unrecoverable_blocks", c.UnrecoverableBlocks, "write_errors", c.WriteErrors}
	if r.Note != "" {
		args = append(args, "note", r.Note)
	}
	switch {
	case c.UnrecoverableBlocks > 0 || c.WriteErrors > 0:
		s.logger.Error("scrub found data no member could supply or couldn't repair it", args...)
	case c.RepairedBlocks > 0 || c.MismatchedBlocks > 0:
		s.logger.Warn("scrub repaired or found differing copies", args...)
	default:
		s.logger.Info("scrub ended", args...)
	}
}

// startScrub starts a scrub of the pool's RAID: a new run, or (resume) the paused run at
// the offset it reached.
func (s *Server) startScrub(ctx context.Context, p *store.Pool, rec *store.ScrubRecord, resume, manual, repair bool, maxMBps uint32, now time.Time) error {
	offset := uint64(0)
	if resume && rec != nil {
		manual, repair, maxMBps = rec.Manual, rec.Repair, rec.MaxMBps
		if rec.OffsetBlocks < rec.TotalBlocks {
			offset = rec.OffsetBlocks
		}
	}
	if err := s.dp.StartScrub(ctx, p.ArrayName, repair, maxMBps, offset); err != nil {
		return err
	}
	_, err := s.store.MutateScrubRecord(p.ID, func(r *store.ScrubRecord) error {
		if !resume {
			r.Manual, r.StartedAt, r.FinishedAt = manual, now, time.Time{}
			r.Counts, r.OffsetBlocks, r.TotalBlocks = store.ScrubCounts{}, 0, 0
		}
		r.State, r.Repair, r.MaxMBps, r.UpdatedAt = store.ScrubRunning, repair, maxMBps, now
		r.SegmentStartBlocks, r.OffsetBlocks, r.Segment = offset, offset, store.ScrubCounts{}
		r.Stopping, r.Note = "", ""
		return nil
	})
	if err == nil {
		s.logger.Info("scrub started", "pool", p.Name, "resume", resume, "offset_blocks", offset, "repair", repair, "max_mb_per_sec", maxMBps, "manual", manual)
	}
	return err
}

// requestScrubStop asks md to stop a pool's running scrub; the record moves to state (PAUSED
// or STOPPED) once md reports it stopped.
func (s *Server) requestScrubStop(ctx context.Context, p *store.Pool, state store.ScrubState, note string) error {
	if _, err := s.store.MutateScrubRecord(p.ID, func(r *store.ScrubRecord) error {
		r.Stopping, r.Note = state, note
		return nil
	}); err != nil {
		return err
	}
	return s.dp.StopScrub(ctx, p.ArrayName)
}

// --- API ---

type scrubSettingsView struct {
	Disabled        bool   `json:"disabled"`
	IntervalDays    uint32 `json:"interval_days"`
	WindowStartHour uint8  `json:"window_start_hour"`
	WindowEndHour   uint8  `json:"window_end_hour"`
	MaxMBps         uint32 `json:"max_mb_per_sec"`
}

func viewScrubSettings(st store.ScrubSettings) scrubSettingsView {
	e := st.Effective()
	return scrubSettingsView{Disabled: e.Disabled, IntervalDays: e.IntervalDays, WindowStartHour: *e.WindowStartHour,
		WindowEndHour: *e.WindowEndHour, MaxMBps: *e.MaxMBps}
}

// handleGetScrubSettings / handlePutScrubSettings: GET|PUT /api/v1/system/scrub.
func (s *Server) handleGetScrubSettings(w http.ResponseWriter, _ *http.Request) {
	st, err := s.store.GetSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scrub": viewScrubSettings(st.Scrub)})
}

func (s *Server) handlePutScrubSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Disabled        *bool   `json:"disabled"`
		IntervalDays    *uint32 `json:"interval_days"`
		WindowStartHour *uint8  `json:"window_start_hour"`
		WindowEndHour   *uint8  `json:"window_end_hour"`
		MaxMBps         *uint32 `json:"max_mb_per_sec"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	switch {
	case req.IntervalDays != nil && (*req.IntervalDays < 1 || *req.IntervalDays > 365):
		writeError(w, fmt.Errorf("%w: interval_days must be 1-365", store.ErrInvalidArgument))
		return
	case req.WindowStartHour != nil && *req.WindowStartHour > 23, req.WindowEndHour != nil && *req.WindowEndHour > 23:
		writeError(w, fmt.Errorf("%w: window hours must be 0-23 (equal hours: any time)", store.ErrInvalidArgument))
		return
	case req.MaxMBps != nil && *req.MaxMBps > maxRebuildMBps:
		writeError(w, fmt.Errorf("%w: max_mb_per_sec must be 0-%d (0: unlimited)", store.ErrInvalidArgument, maxRebuildMBps))
		return
	}
	s.scrubMu.Lock()
	defer s.scrubMu.Unlock()
	st, err := s.store.GetSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	if req.Disabled != nil {
		st.Scrub.Disabled = *req.Disabled
	}
	if req.IntervalDays != nil {
		st.Scrub.IntervalDays = *req.IntervalDays
	}
	if req.WindowStartHour != nil {
		st.Scrub.WindowStartHour = req.WindowStartHour
	}
	if req.WindowEndHour != nil {
		st.Scrub.WindowEndHour = req.WindowEndHour
	}
	if req.MaxMBps != nil {
		st.Scrub.MaxMBps = req.MaxMBps
	}
	if err := s.store.SaveSettings(st); err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"scrub": viewScrubSettings(st.Scrub)})
}

// scrubView is a pool's scrub record with the derived fields the UI and veloctl show.
type scrubView struct {
	*store.ScrubRecord
	Total   store.ScrubCounts `json:"total"`
	Percent float64           `json:"percent"`
}

func (s *Server) poolScrubView(p *store.Pool, rec *store.ScrubRecord, rb *dataplane.Array, cfg store.ScrubSettings) map[string]any {
	out := map[string]any{"scrub": nil}
	if why := scrubEligible(p, rb); why != "" {
		out["unavailable"] = "can't scrub the pool now: " + why
	}
	if !cfg.Disabled && (rec == nil || rec.State != store.ScrubRunning) {
		due := scrubDue(p, rec, cfg)
		out["next_due"] = due
	}
	if rec != nil {
		v := scrubView{ScrubRecord: rec, Total: rec.Total()}
		if rec.TotalBlocks > 0 {
			v.Percent = float64(rec.OffsetBlocks) * 100 / float64(rec.TotalBlocks)
		}
		out["scrub"] = v
	}
	return out
}

// handleGetPoolScrub: GET /api/v1/pools/{id}/scrub.
func (s *Server) handleGetPoolScrub(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.GetPool(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	var rb *dataplane.Array
	var rec *store.ScrubRecord
	synced := false
	if s.dp != nil {
		if raids, err := s.raidsByName(r.Context()); err == nil {
			rb = raids[p.ArrayName]
			// Bring the record up to date with md, so progress is current.
			s.scrubMu.Lock()
			rec, err = s.syncScrub(r.Context(), p, rb, time.Now())
			s.scrubMu.Unlock()
			if err != nil {
				writeError(w, err)
				return
			}
			synced = true
		}
	}
	if !synced {
		var err error
		rec, err = s.store.GetScrubRecord(p.ID)
		if errors.Is(err, store.ErrNotFound) {
			rec = nil
		} else if err != nil {
			writeError(w, err)
			return
		}
	}
	st, err := s.store.GetSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.poolScrubView(p, rec, rb, st.Scrub.Effective()))
}

// handleStartPoolScrub: POST /api/v1/pools/{id}/scrub {repair?, max_mb_per_sec?} starts a scrub
// now (outside the schedule and its window).
func (s *Server) handleStartPoolScrub(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repair  *bool   `json:"repair"`
		MaxMBps *uint32 `json:"max_mb_per_sec"`
	}
	if err := decodeOptionalJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.MaxMBps != nil && *req.MaxMBps > maxRebuildMBps {
		writeError(w, fmt.Errorf("%w: max_mb_per_sec must be 0-%d (0: unlimited)", store.ErrInvalidArgument, maxRebuildMBps))
		return
	}
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	s.scrubMu.Lock()
	defer s.scrubMu.Unlock()
	p, err := s.store.GetPool(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	raids, err := s.raidsByName(r.Context())
	if err != nil {
		writeError(w, err)
		return
	}
	now := time.Now()
	rec, err := s.syncScrub(r.Context(), p, raids[p.ArrayName], now)
	if err != nil {
		writeError(w, err)
		return
	}
	if rec != nil && rec.State == store.ScrubRunning {
		writeError(w, fmt.Errorf("%w: pool %s is already being scrubbed (%d%%)", store.ErrInUse, p.Name, scrubPercent(rec)))
		return
	}
	if why := scrubEligible(p, raids[p.ArrayName]); why != "" {
		writeError(w, fmt.Errorf("%w: can't scrub pool %s now: %s", store.ErrInUse, p.Name, why))
		return
	}
	st, err := s.store.GetSettings()
	if err != nil {
		writeError(w, err)
		return
	}
	repair, maxMBps := true, *st.Scrub.Effective().MaxMBps
	if req.Repair != nil {
		repair = *req.Repair
	}
	if req.MaxMBps != nil {
		maxMBps = *req.MaxMBps
	}
	if err := s.startScrub(r.Context(), p, rec, false, true, repair, maxMBps, now); err != nil {
		writeError(w, fmt.Errorf("start scrub of pool %s: %w", p.Name, err))
		return
	}
	rec, _ = s.store.GetScrubRecord(p.ID)
	writeJSON(w, http.StatusAccepted, s.poolScrubView(p, rec, raids[p.ArrayName], st.Scrub.Effective()))
}

func scrubPercent(r *store.ScrubRecord) int {
	if r.TotalBlocks == 0 {
		return 0
	}
	return int(r.OffsetBlocks * 100 / r.TotalBlocks)
}

// handleStopPoolScrub: DELETE /api/v1/pools/{id}/scrub stops a running scrub (after its current
// window); the run ends as STOPPED and the schedule starts the next one afresh.
func (s *Server) handleStopPoolScrub(w http.ResponseWriter, r *http.Request) {
	if err := s.requireDataPlane(); err != nil {
		writeError(w, err)
		return
	}
	s.scrubMu.Lock()
	defer s.scrubMu.Unlock()
	p, err := s.store.GetPool(r.PathValue("id"))
	if err != nil {
		writeError(w, err)
		return
	}
	rec, err := s.store.GetScrubRecord(p.ID)
	if errors.Is(err, store.ErrNotFound) || (err == nil && rec.State != store.ScrubRunning && rec.State != store.ScrubPaused) {
		writeError(w, fmt.Errorf("%w: pool %s isn't being scrubbed", store.ErrInvalidArgument, p.Name))
		return
	} else if err != nil {
		writeError(w, err)
		return
	}
	if rec.State == store.ScrubPaused {
		// Nothing runs: end the run here.
		rec, err = s.store.MutateScrubRecord(p.ID, func(r *store.ScrubRecord) error {
			r.State, r.FinishedAt, r.Note, r.UpdatedAt = store.ScrubStopped, time.Now(), "stopped by an operator", time.Now()
			return nil
		})
	} else {
		err = s.requestScrubStop(r.Context(), p, store.ScrubStopped, "stopped by an operator")
		if err == nil {
			rec, err = s.store.GetScrubRecord(p.ID)
		}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"scrub": rec})
}
