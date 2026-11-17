package backup

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/velocor/velocor/internal/store"
)

func newManager(t *testing.T) (*Manager, *store.BoltStore, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "velocor.db")
	st, err := store.OpenBoltStore(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.SaveSystemConfig(&store.SystemConfig{NodeID: "node-1", JWTSecret: []byte("s")}); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	return &Manager{Dir: filepath.Join(dir, "backups"), DBPath: db, Source: st, Now: func() time.Time { return now }}, st, &now
}

func TestScheduleAndRetention(t *testing.T) {
	m, _, now := newManager(t)
	// Two days and a bit, checked every 5 minutes.
	for range 2*24*12 + 7 {
		if err := m.Due(); err != nil {
			t.Fatal(err)
		}
		*now = now.Add(5 * time.Minute)
	}
	all, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	count := map[string]int{}
	for _, b := range all {
		count[b.Kind]++
		if b.SizeBytes == 0 {
			t.Fatalf("empty backup %s", b.Name)
		}
	}
	if count[KindHourly] != 24 || count[KindDaily] != 3 {
		t.Fatalf("after 2 days: %v", count)
	}
	// A week later only 7 dailies remain.
	for range 7 * 24 {
		*now = now.Add(time.Hour)
		if err := m.Due(); err != nil {
			t.Fatal(err)
		}
	}
	all, _ = m.List()
	count = map[string]int{}
	for _, b := range all {
		count[b.Kind]++
	}
	if count[KindHourly] != 24 || count[KindDaily] != 7 {
		t.Fatalf("after 9 days: %v", count)
	}
}

func TestRestoreRoundTrip(t *testing.T) {
	m, st, now := newManager(t)
	if err := st.CreatePortal(&store.Portal{ID: "pt1", Name: "before", Protocol: store.ExportProtocolNVMeoFTCP, IPAddress: "10.0.0.1", Port: 4420}); err != nil {
		t.Fatal(err)
	}
	b, err := m.Take(KindManual)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeletePortal("pt1"); err != nil {
		t.Fatal(err)
	}
	*now = now.Add(time.Minute)
	if err := m.StageRestoreFrom(b.Name); err != nil {
		t.Fatal(err)
	}
	all, _ := m.List()
	if all[0].Kind != KindPreRestore {
		t.Fatalf("the current database must be backed up before a restore: %+v", all)
	}
	// velod restarts: close, apply, reopen.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if err := ApplyPendingRestore(m.DBPath, m.logger()); err != nil {
		t.Fatal(err)
	}
	st2, err := store.OpenBoltStore(m.DBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st2.Close() }()
	if p, err := st2.GetPortal("pt1"); err != nil || p.Name != "before" {
		t.Fatalf("restored portal: %+v %v", p, err)
	}
	if _, err := os.Stat(PendingRestorePath(m.DBPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the staged restore must be consumed")
	}
}

func TestRestoreRejectsGarbage(t *testing.T) {
	m, _, _ := newManager(t)
	err := m.StageRestore(bytes.NewReader([]byte(strings.Repeat("not a database", 1000))))
	if !errors.Is(err, store.ErrInvalidArgument) {
		t.Fatalf("garbage accepted: %v", err)
	}
	if _, err := os.Stat(PendingRestorePath(m.DBPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("nothing may be staged")
	}
	if _, err := m.Open("../velocor.db"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("path traversal: %v", err)
	}
}
