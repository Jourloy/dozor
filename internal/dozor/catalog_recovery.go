package dozor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/mattn/go-sqlite3"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const recoveryBatch = 100

type RecoveryProgress struct {
	State     string `json:"state"`
	Processed int64  `json:"processed"`
	Errors    int64  `json:"errors"`
}

func corruptCatalog(err error) bool {
	var e sqlite3.Error
	return errors.As(err, &e) && (e.Code == sqlite3.ErrCorrupt || e.Code == sqlite3.ErrNotADB)
}
func quarantineCatalog(g Guard) error {
	if err := g.Check(); err != nil {
		return err
	}
	name := ".catalog-backup-" + ID()
	if err := os.Mkdir(filepath.Join(g.Root, name), 0700); err != nil {
		return err
	}
	if err := WriteJSON(filepath.Join(g.Root, ".catalog-quarantine.json"), name); err != nil {
		return err
	}
	return resumeCatalogQuarantine(g)
}
func resumeCatalogQuarantine(g Guard) error {
	if err := g.Check(); err != nil {
		return err
	}
	marker := filepath.Join(g.Root, ".catalog-quarantine.json")
	data, err := os.ReadFile(marker)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var name string
	if json.Unmarshal(data, &name) != nil || !strings.HasPrefix(name, ".catalog-backup-") || !safeID.MatchString(strings.TrimPrefix(name, ".catalog-backup-")) {
		return errors.New("invalid catalog quarantine")
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		from := filepath.Join(g.Root, "catalog.sqlite"+suffix)
		to := filepath.Join(g.Root, name, "catalog.sqlite"+suffix)
		if _, err = os.Stat(to); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err = os.Rename(from, to); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err = syncDirectory(filepath.Join(g.Root, name)); err != nil {
		return err
	}
	if err = syncDirectory(g.Root); err != nil {
		return err
	}
	return removeDurable(marker)
}
func (s *Store) scheduleRecovery() error {
	var marker string
	err := s.DB.QueryRow("SELECT value FROM metadata WHERE key='catalog_journal_v1'").Scan(&marker)
	if err == nil {
		return nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.DB.Exec("INSERT OR IGNORE INTO recovery_directories(path) VALUES('events')")
	return err
}
func (s *Store) RequestRecovery() error { return s.requestRecovery("") }
func (s *Store) requestRecovery(transition string) error {
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	if transition != "" {
		var previous string
		err := s.DB.QueryRow("SELECT value FROM metadata WHERE key='catalog_transition'").Scan(&previous)
		if err == nil && previous == transition {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	s.recoveryPath = ""
	s.recoveryEntries = nil
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{"DELETE FROM metadata WHERE key IN ('catalog_journal_v1','recovery_processed','recovery_errors')", "DELETE FROM recovery_directories", "DELETE FROM recovery_parts", "INSERT INTO recovery_directories(path) VALUES('events')"} {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	if transition != "" {
		if _, err = tx.Exec("INSERT OR REPLACE INTO metadata VALUES('catalog_transition',?)", transition); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) RecoveryProgress() RecoveryProgress {
	p := RecoveryProgress{State: "complete"}
	var pending int
	if s.DB.QueryRow("SELECT COUNT(*) FROM recovery_directories WHERE done=0").Scan(&pending) != nil {
		p.State = "error"
		return p
	}
	if pending > 0 {
		p.State = "running"
	} else {
		var marker string
		if s.DB.QueryRow("SELECT value FROM metadata WHERE key='catalog_journal_v1'").Scan(&marker) != nil {
			p.State = "awaiting_update"
		}
	}
	_ = s.DB.QueryRow("SELECT value FROM metadata WHERE key='recovery_processed'").Scan(&p.Processed)
	_ = s.DB.QueryRow("SELECT value FROM metadata WHERE key='recovery_errors'").Scan(&p.Errors)
	return p
}
func (s *Store) recoveryError(path string, err error) {
	s.Notice(fmt.Sprintf("Восстановление каталога: %s: %v", path, err))
	_, _ = s.DB.Exec("INSERT INTO metadata VALUES('recovery_errors','1') ON CONFLICT(key) DO UPDATE SET value=CAST(value AS INTEGER)+1")
}
func (s *Store) CompleteRecovery() error {
	_, err := s.DB.Exec("INSERT OR REPLACE INTO metadata VALUES('catalog_journal_v1','1')")
	return err
}

// One persisted directory cursor is advanced per batch. A directory may be
// enumerated again after a crash; committed entries are never imported again.
func (s *Store) RecoveryStep(ctx context.Context) (bool, error) {
	s.recoveryMu.Lock()
	defer s.recoveryMu.Unlock()
	var rel, cursor string
	err := s.DB.QueryRowContext(ctx, "SELECT path,cursor FROM recovery_directories WHERE done=0 ORDER BY path LIMIT 1").Scan(&rel, &cursor)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = s.Guard.Check(); err != nil {
		return false, err
	}
	full, err := checkedPath(s.Root, rel)
	if err != nil {
		return false, err
	}
	entries := s.recoveryEntries
	if s.recoveryPath != rel {
		entries, err = os.ReadDir(full)
		if err == nil {
			s.recoveryPath, s.recoveryEntries = rel, entries
		}
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.recoveryError(rel, err)
			_, err = s.DB.ExecContext(ctx, "UPDATE recovery_directories SET done=1 WHERE path=?", rel)
			return true, err
		}
		return false, err
	}
	isEvent := len(strings.Split(filepath.ToSlash(rel), "/")) == 4
	var ev Event
	if isEvent {
		data, e := os.ReadFile(filepath.Join(full, "event.json"))
		if e == nil {
			e = json.Unmarshal(data, &ev)
		}
		if e == nil && (!safeID.MatchString(ev.ID) || !safeID.MatchString(ev.CameraID) || eventDir(ev) != rel) {
			e = errors.New("invalid event manifest")
		}
		if e != nil {
			var pe *os.PathError
			if errors.As(e, &pe) && !errors.Is(e, os.ErrNotExist) {
				return false, e
			}
			s.recoveryError(rel, e)
			_, err = s.DB.ExecContext(ctx, "UPDATE recovery_directories SET done=1 WHERE path=?", rel)
			return true, err
		}
	}
	count := 0
	first := sort.Search(len(entries), func(i int) bool { return entries[i].Name() > cursor })
	for _, entry := range entries[first:] {
		if err = ctx.Err(); err != nil {
			return false, err
		}
		if count == recoveryBatch {
			return true, nil
		}
		child := filepath.Join(rel, entry.Name())
		// Never follow symlinks, including directory symlinks.
		if entry.Type()&os.ModeSymlink == 0 {
			if entry.IsDir() && !isEvent {
				if _, err = s.DB.ExecContext(ctx, "INSERT OR IGNORE INTO recovery_directories(path) VALUES(?)", child); err != nil {
					return false, err
				}
			} else if isEvent && strings.HasSuffix(entry.Name(), ".mp4.json") {
				if err = s.stageRecoveredPart(ctx, rel, child, ev); err != nil {
					if ctx.Err() != nil {
						return false, ctx.Err()
					}
					var pe *os.PathError
					var se sqlite3.Error
					if errors.As(err, &se) || (errors.As(err, &pe) && !errors.Is(err, os.ErrNotExist)) {
						return false, err
					}
					s.recoveryError(child, err)
				}
			}
		}
		tx, e := s.DB.BeginTx(ctx, nil)
		if e != nil {
			return false, e
		}
		if _, e = tx.ExecContext(ctx, "UPDATE recovery_directories SET cursor=? WHERE path=?", entry.Name(), rel); e == nil {
			_, e = tx.ExecContext(ctx, "INSERT INTO metadata VALUES('recovery_processed','1') ON CONFLICT(key) DO UPDATE SET value=CAST(value AS INTEGER)+1")
		}
		if e == nil {
			e = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if e != nil {
			return false, e
		}
		count++
	}
	if isEvent {
		if err = s.importRecoveredEvent(ctx, rel, ev); err != nil {
			return false, err
		}
	}
	_, err = s.DB.ExecContext(ctx, "UPDATE recovery_directories SET done=1 WHERE path=?", rel)
	return true, err
}
func (s *Store) stageRecoveredPart(ctx context.Context, directory, rel string, ev Event) error {
	data, err := os.ReadFile(filepath.Join(s.Root, rel))
	if err != nil {
		return err
	}
	var p Part
	if err = json.Unmarshal(data, &p); err != nil {
		return err
	}
	if !safeID.MatchString(p.ID) || p.EventID != ev.ID || p.CameraID != ev.CameraID || p.Path+".json" != rel {
		return errors.New("invalid part manifest")
	}
	path, err := checkedPath(s.Root, p.Path)
	if err != nil {
		return err
	}
	// Existing catalog state (including uploads and tombstones) always wins.
	if _, err = s.Part(p.ID); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if p.Deleted {
		// A durable tombstone is also an unfinished unlink instruction.
		if err = removeDurable(path); err != nil {
			return err
		}
		if err = removeDurable(path + ".partial"); err != nil {
			return err
		}
	} else {
		source := path
		_, statErr := os.Stat(path)
		partial := errors.Is(statErr, os.ErrNotExist)
		if statErr != nil && !partial {
			return statErr
		}
		if partial {
			source += ".partial"
		}
		sha, _, size, hashErr := hashFileContext(ctx, source)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if hashErr != nil && !errors.Is(hashErr, os.ErrNotExist) {
			return hashErr
		}
		if hashErr != nil || sha != p.SHA256 || size != p.Size {
			p.Deleted = true
			p.Lost = !p.Uploaded
			s.recoveryError(p.Path, errors.New("видеофайл отсутствует или не совпадает с контрольной суммой; исходный файл сохранён"))
		} else if partial {
			if err = os.Rename(source, path); err != nil {
				return err
			}
			if err = syncDirectory(filepath.Dir(path)); err != nil {
				return err
			}
		}
	}
	data, _ = json.Marshal(p)
	_, err = s.DB.ExecContext(ctx, "INSERT OR REPLACE INTO recovery_parts VALUES(?,?,?)", directory, p.ID, string(data))
	return err
}
func (s *Store) importRecoveredEvent(ctx context.Context, rel string, ev Event) error {
	s.mutate.Lock()
	defer s.mutate.Unlock()
	s.files.Lock()
	defer s.files.Unlock()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// An event is exposed only once all its part descriptors have been staged.
	raw, _ := json.Marshal(ev)
	result, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO events VALUES(?,?,?,?,?,?,?)", ev.ID, ev.CameraID, ev.Start, ev.End, ev.Cursor, ev.Status, string(raw))
	if err != nil {
		return err
	}
	inserted, _ := result.RowsAffected()
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO parts SELECT id,json_extract(payload,'$.event_id'),json_extract(payload,'$.start'),json_extract(payload,'$.end'),json_extract(payload,'$.uploaded'),json_extract(payload,'$.deleted'),payload FROM recovery_parts WHERE directory=?`, rel); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO jobs(id,kind,ref) SELECT 'part:'||p.id,'part',p.id FROM parts p JOIN recovery_parts r ON p.id=r.id WHERE r.directory=? AND p.uploaded=0 AND p.deleted=0`, rel); err != nil {
		return err
	}
	if inserted == 0 {
		var current string
		if err = tx.QueryRowContext(ctx, "SELECT payload FROM events WHERE id=?", ev.ID).Scan(&current); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(current), &ev); err != nil {
			return err
		}
	}
	if inserted > 0 || ev.Status != "closed" {
		var lost bool
		if err = tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM parts WHERE event=? AND json_extract(payload,'$.lost'))", ev.ID).Scan(&lost); err != nil {
			return err
		}
		if lost {
			ev.Lost = true
			ev.Uploaded = false
		}
		if inserted > 0 && ev.Status != "closed" {
			ev.Status = "closing"
			ev.Incomplete = true
		}
		var end sql.NullInt64
		if err = tx.QueryRowContext(ctx, "SELECT MAX(end) FROM parts WHERE event=?", ev.ID).Scan(&end); err != nil {
			return err
		}
		if end.Valid && end.Int64 > ev.Cursor {
			ev.Cursor = end.Int64
			ev.AssemblyFailures = 0
		}
		if err = putEvent(tx, ev); err != nil {
			return err
		}
		if err = putOperation(tx, "event:"+ev.ID, fileOperation{Kind: "event", Event: &ev}); err != nil {
			return err
		}
		if ev.Status == "closed" && !ev.Uploaded {
			if err = enqueueTx(tx, "event", ev.ID); err != nil {
				return err
			}
		}
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM recovery_parts WHERE directory=?", rel); err != nil {
		return err
	}
	// Committing the cursor with the import prevents repeating the event merge.
	if _, err = tx.ExecContext(ctx, "UPDATE recovery_directories SET done=1 WHERE path=?", rel); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) RunRecovery(ctx context.Context, canComplete func() bool, changed func()) {
	for ctx.Err() == nil {
		worked, err := s.RecoveryStep(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			s.recoveryError("catalog", err)
			if !pause(ctx, time.Second) {
				return
			}
			continue
		}
		if changed != nil {
			changed()
		}
		if !worked {
			if canComplete == nil || canComplete() {
				if err = s.CompleteRecovery(); err == nil {
					return
				}
			}
			if !pause(ctx, time.Second) {
				return
			}
		} else if !pause(ctx, 10*time.Millisecond) {
			return
		}
	}
}
