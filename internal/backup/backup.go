// Package backup keeps local copies of velod's database (TODO P0-3): hourly for a day, daily
// for a week, and one before every OS upgrade and every restore. They guard against corruption
// and mistakes; losing the boot disk is covered by the copies on the pools (internal/meta).
package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/velocor/velocor/internal/store"
)

// Kinds of backup.
const (
	KindHourly     = "hourly"
	KindDaily      = "daily"
	KindPreUpgrade = "pre-upgrade"
	KindPreRestore = "pre-restore"
	KindManual     = "manual"
)

// keep is how many backups of each kind are kept.
var keep = map[string]int{KindHourly: 24, KindDaily: 7, KindPreUpgrade: 5, KindPreRestore: 5, KindManual: 5}

// period is how often a scheduled kind is taken.
var period = map[string]time.Duration{KindHourly: time.Hour, KindDaily: 24 * time.Hour}

const stamp = "20060102T150405Z"

var nameRe = regexp.MustCompile(`^velocor-(hourly|daily|pre-upgrade|pre-restore|manual)-(\d{8}T\d{6}Z)\.db$`)

// Source is the database being backed up.
type Source interface {
	Backup(w io.Writer) (int64, error)
}

// Info describes one backup file.
type Info struct {
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	CreatedAt time.Time `json:"created_at"`
	SizeBytes int64     `json:"size_bytes"`
}

// Manager takes, lists, prunes and restores backups.
type Manager struct {
	Dir    string // e.g. /var/lib/velocor/backups
	DBPath string // the live database
	Source Source
	Logger *slog.Logger
	Now    func() time.Time // tests
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func (m *Manager) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// PendingRestorePath is where a restore waits for velod to restart.
func PendingRestorePath(dbPath string) string { return dbPath + ".restore" }

// Take writes a backup of the given kind and prunes old ones.
func (m *Manager) Take(kind string) (Info, error) {
	if _, ok := keep[kind]; !ok {
		return Info{}, fmt.Errorf("unknown backup kind %q", kind)
	}
	if err := os.MkdirAll(m.Dir, 0o700); err != nil {
		return Info{}, err
	}
	at := m.now()
	name := fmt.Sprintf("velocor-%s-%s.db", kind, at.Format(stamp))
	n, err := writeFileAtomic(filepath.Join(m.Dir, name), func(w io.Writer) (int64, error) { return m.Source.Backup(w) })
	if err != nil {
		return Info{}, fmt.Errorf("back up the database: %w", err)
	}
	if err := m.Prune(); err != nil {
		m.logger().Warn("pruning old backups failed", "err", err)
	}
	return Info{Name: name, Kind: kind, CreatedAt: at.Truncate(time.Second), SizeBytes: n}, nil
}

// List returns the backups, newest first.
func (m *Manager) List() ([]Info, error) {
	entries, err := os.ReadDir(m.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Info{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Info{}
	for _, e := range entries {
		mm := nameRe.FindStringSubmatch(e.Name())
		if mm == nil || !e.Type().IsRegular() {
			continue
		}
		at, err := time.Parse(stamp, mm[2])
		if err != nil {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Info{Name: e.Name(), Kind: mm[1], CreatedAt: at, SizeBytes: fi.Size()})
	}
	slices.SortFunc(out, func(a, b Info) int { return b.CreatedAt.Compare(a.CreatedAt) })
	return out, nil
}

// Prune deletes the oldest backups beyond each kind's limit.
func (m *Manager) Prune() error {
	all, err := m.List()
	if err != nil {
		return err
	}
	seen := map[string]int{}
	var errs []error
	for _, b := range all { // newest first
		seen[b.Kind]++
		if seen[b.Kind] > keep[b.Kind] {
			if err := os.Remove(filepath.Join(m.Dir, b.Name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// Due takes every scheduled backup whose period has passed since the newest one of its kind.
func (m *Manager) Due() error {
	all, err := m.List()
	if err != nil {
		return err
	}
	var errs []error
	for _, kind := range []string{KindHourly, KindDaily} {
		i := slices.IndexFunc(all, func(b Info) bool { return b.Kind == kind })
		if i >= 0 && m.now().Sub(all[i].CreatedAt) < period[kind] {
			continue
		}
		if _, err := m.Take(kind); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Run takes the scheduled backups until ctx ends.
func (m *Manager) Run(ctx context.Context) {
	for {
		if err := m.Due(); err != nil {
			m.logger().Error("scheduled database backup failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(5 * time.Minute):
		}
	}
}

// Open opens a backup by name for download.
func (m *Manager) Open(name string) (*os.File, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("%w: no backup named %q", store.ErrNotFound, name)
	}
	f, err := os.Open(filepath.Join(m.Dir, name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w: no backup named %q", store.ErrNotFound, name)
	}
	return f, err
}

// StageRestore validates an uploaded database, backs up the current one, and leaves the upload
// to replace it when velod next starts (ApplyPendingRestore). The caller restarts velod.
func (m *Manager) StageRestore(r io.Reader) error {
	pending := PendingRestorePath(m.DBPath)
	tmp := pending + ".upload"
	if _, err := writeFileAtomic(tmp, func(w io.Writer) (int64, error) { return io.Copy(w, r) }); err != nil {
		return fmt.Errorf("receive the database: %w", err)
	}
	if err := store.ValidateBackup(tmp); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if _, err := m.Take(KindPreRestore); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("back up the current database first: %w", err)
	}
	return os.Rename(tmp, pending)
}

// StageRestoreFrom stages one of the local backups.
func (m *Manager) StageRestoreFrom(name string) error {
	f, err := m.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	return m.StageRestore(f)
}

// ApplyPendingRestore puts a staged restore in place of the database. velod calls it before
// opening the database.
func ApplyPendingRestore(dbPath string, logger *slog.Logger) error {
	pending := PendingRestorePath(dbPath)
	if _, err := os.Stat(pending); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err := store.ValidateBackup(pending); err != nil {
		bad := pending + ".rejected"
		_ = os.Rename(pending, bad)
		return fmt.Errorf("staged restore %s is not usable (kept as %s): %w", pending, bad, err)
	}
	if err := os.Rename(pending, dbPath); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(dbPath)); err != nil {
		return err
	}
	logger.Warn("restored the database from a backup", "db", dbPath)
	return nil
}

// writeFileAtomic writes path through a temporary file, fsyncs it and renames it into place.
func writeFileAtomic(path string, fill func(io.Writer) (int64, error)) (int64, error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return 0, err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		if !ok {
			_ = f.Close()
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o600); err != nil {
		return 0, err
	}
	n, err := fill(f)
	if err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	if err := f.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, err
	}
	ok = true
	return n, syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil && !strings.Contains(err.Error(), "invalid argument") {
		return err
	}
	return nil
}
