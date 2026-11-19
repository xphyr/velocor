package server

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/velocor/velocor/internal/backup"
	"github.com/velocor/velocor/internal/store"
)

// maxRestoreBytes bounds an uploaded database (TODO P0-3).
const maxRestoreBytes = 1 << 30

const restorePath = "/api/v1/system/restore"

// WithBackups enables the database backup API. restart is called after a restore is staged;
// it must make velod exit so it starts again on the restored database.
func WithBackups(m *backup.Manager, restart func()) Option {
	return func(s *Server) { s.backups, s.restart = m, restart }
}

var errBackupsDisabled = fmt.Errorf("%w: database backups are not configured", errDataPlaneUnavailable)

// handleDownloadBackup streams a consistent copy of the live database, or ?name= one of the
// local backups. Admin only: it holds password hashes and the JWT secret.
func (s *Server) handleDownloadBackup(w http.ResponseWriter, r *http.Request) {
	if name := r.URL.Query().Get("name"); name != "" {
		if s.backups == nil {
			writeError(w, errBackupsDisabled)
			return
		}
		f, err := s.backups.Open(name)
		if err != nil {
			writeError(w, err)
			return
		}
		defer func() { _ = f.Close() }()
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
		_, _ = io.Copy(w, f)
		return
	}
	src, ok := s.store.(backup.Source)
	if !ok {
		writeError(w, errBackupsDisabled)
		return
	}
	name := fmt.Sprintf("velocor-%s.db", time.Now().UTC().Format("20060102T150405Z"))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	if _, err := src.Backup(w); err != nil {
		s.logger.Error("database download failed", "err", err)
	}
}

func (s *Server) handleListBackups(w http.ResponseWriter, _ *http.Request) {
	if s.backups == nil {
		writeError(w, errBackupsDisabled)
		return
	}
	list, err := s.backups.List()
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"backups": list, "directory": s.backups.Dir})
}

func (s *Server) handleTakeBackup(w http.ResponseWriter, _ *http.Request) {
	if s.backups == nil {
		writeError(w, errBackupsDisabled)
		return
	}
	b, err := s.backups.Take(backup.KindManual)
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"backup": b})
}

// handleRestore replaces the database with an uploaded one (request body), or with a local
// backup (?name=). The current database is backed up first; velod then restarts on the
// restored one.
func (s *Server) handleRestore(w http.ResponseWriter, r *http.Request) {
	if s.backups == nil || s.restart == nil {
		writeError(w, errBackupsDisabled)
		return
	}
	var err error
	if name := r.URL.Query().Get("name"); name != "" {
		err = s.backups.StageRestoreFrom(name)
	} else {
		err = s.backups.StageRestore(r.Body)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			err = fmt.Errorf("%w: the upload is larger than %d bytes", store.ErrInvalidArgument, int64(maxRestoreBytes))
		}
	}
	if err != nil {
		writeError(w, err)
		return
	}
	s.logger.Warn("database restore staged; restarting velod")
	writeJSON(w, http.StatusAccepted, map[string]any{
		"status":  "restarting",
		"message": "The database was validated and the current one backed up. velod restarts on the restored database; sign in again in a few seconds.",
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	go func() {
		time.Sleep(time.Second)
		s.restart()
	}()
}
