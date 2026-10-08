package dozor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Intent and catalog mutations commit together. Files are projections of the
// catalog; an interrupted projection can be replayed without walking events/.
type fileOperation struct {
	Kind   string `json:"kind"`
	Path   string `json:"path,omitempty"`
	Event  *Event `json:"event,omitempty"`
	Part   *Part  `json:"part,omitempty"`
	Cursor int64  `json:"cursor,omitempty"`
}

func putOperation(tx *sql.Tx, id string, op fileOperation) error {
	b, err := json.Marshal(op)
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT OR REPLACE INTO file_operations VALUES(?,?)", id, string(b))
	return err
}
func putEvent(tx *sql.Tx, e Event) error {
	b, _ := json.Marshal(e)
	_, err := tx.Exec(`INSERT INTO events VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET end=excluded.end,cursor=excluded.cursor,status=excluded.status,payload=excluded.payload`, e.ID, e.CameraID, e.Start, e.End, e.Cursor, e.Status, string(b))
	return err
}
func putPart(tx *sql.Tx, p Part) error {
	b, _ := json.Marshal(p)
	_, err := tx.Exec(`INSERT INTO parts VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET uploaded=excluded.uploaded,deleted=excluded.deleted,payload=excluded.payload`, p.ID, p.EventID, p.Start, p.End, p.Uploaded, p.Deleted, string(b))
	return err
}
func enqueueTx(tx *sql.Tx, kind, ref string) error {
	_, err := tx.Exec("INSERT OR IGNORE INTO jobs(id,kind,ref) VALUES(?,?,?)", kind+":"+ref, kind, ref)
	return err
}
func (s *Store) saveEvent(ev Event, enqueue bool) error {
	s.files.Lock()
	defer s.files.Unlock()
	if err := s.Guard.Check(); err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = putEvent(tx, ev); err != nil {
		return err
	}
	id := "event:" + ev.ID
	if err = putOperation(tx, id, fileOperation{Kind: "event", Event: &ev}); err != nil {
		return err
	}
	if enqueue {
		if err = enqueueTx(tx, "event", ev.ID); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.applyFileOperation(id)
}
func (s *Store) savePart(p Part) error {
	s.files.Lock()
	defer s.files.Unlock()
	if err := s.Guard.Check(); err != nil {
		return err
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = putPart(tx, p); err != nil {
		return err
	}
	id := "part:" + p.ID
	if err = putOperation(tx, id, fileOperation{Kind: "part", Part: &p}); err != nil {
		return err
	}
	if !p.Deleted && !p.Uploaded {
		if err = enqueueTx(tx, "part", p.ID); err != nil {
			return err
		}
	} else {
		if _, err = tx.Exec("DELETE FROM jobs WHERE id=?", "part:"+p.ID); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.applyFileOperation(id)
}
func (s *Store) queueFileOperation(id string, op fileOperation) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = putOperation(tx, id, op); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) recordFileOperation(id string, op fileOperation) error {
	s.files.Lock()
	defer s.files.Unlock()
	if err := s.queueFileOperation(id, op); err != nil {
		return err
	}
	return s.applyFileOperation(id)
}
func removeDurable(path string) error {
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

// Caller owns files. Never keep a SQL transaction open while doing filesystem IO.
func (s *Store) applyFileOperation(id string) error {
	return s.applyFileOperationContext(context.Background(), id)
}
func (s *Store) applyFileOperationContext(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var data string
	if err := s.DB.QueryRow("SELECT payload FROM file_operations WHERE id=?", id).Scan(&data); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	var op fileOperation
	if err := json.Unmarshal([]byte(data), &op); err != nil {
		return err
	}
	if err := s.Guard.Check(); err != nil {
		return err
	}
	switch op.Kind {
	case "event":
		if op.Event == nil || !safeID.MatchString(op.Event.ID) || !safeID.MatchString(op.Event.CameraID) {
			return errors.New("invalid event operation")
		}
		if err := WriteJSON(filepath.Join(s.Root, eventDir(*op.Event), "event.json"), op.Event); err != nil {
			return err
		}
	case "part":
		if op.Part == nil {
			return errors.New("invalid part operation")
		}
		path, err := checkedPath(s.Root, op.Part.Path)
		if err != nil {
			return err
		}
		if err = WriteJSON(path+".json", op.Part); err != nil {
			return err
		}

	case "delete_segment":
		path, err := checkedPath(s.Root, op.Path)
		if err != nil {
			return err
		}
		if err = removeDurable(path); err != nil {
			return err
		}
		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec("DELETE FROM segments WHERE path=?", op.Path); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM file_operations WHERE id=?", id); err != nil {
			return err
		}
		return tx.Commit()
	case "assemble":
		// Only replayed after a restart: the worker has not published this partial.
		if op.Part == nil {
			return errors.New("invalid assembly operation")
		}
		path, err := checkedPath(s.Root, op.Part.Path)
		if err != nil {
			return err
		}
		if err = removeDurable(path + ".partial"); err != nil {
			return err
		}
	case "publish":
		if op.Part == nil || op.Event == nil {
			return errors.New("invalid publication operation")
		}
		p := *op.Part
		current, err := s.Event(p.EventID)
		if err != nil {
			return err
		}
		existing, partErr := s.Part(p.ID)
		if partErr != nil && !errors.Is(partErr, sql.ErrNoRows) {
			return partErr
		}
		path, err := checkedPath(s.Root, p.Path)
		if err != nil {
			return err
		}
		if partErr == nil {
			// Never replace a newer upload/deletion acknowledgement on replay.
			if err = WriteJSON(path+".json", existing); err != nil {
				return err
			}
			_, err = s.DB.Exec("DELETE FROM file_operations WHERE id=?", id)
			return err
		}
		if current.Cursor != op.Cursor {
			// Another publication or recovered part already owns this position.
			for _, suffix := range []string{".partial", ".json", ""} {
				if err = removeDurable(path + suffix); err != nil {
					return err
				}
			}
			_, err = s.DB.Exec("DELETE FROM file_operations WHERE id=?", id)
			return err
		}
		if _, err = os.Stat(path); errors.Is(err, os.ErrNotExist) {
			sha, _, size, err := hashFileContext(ctx, path+".partial")
			if err != nil || sha != p.SHA256 || size != p.Size {
				return fmt.Errorf("incomplete publication %s", p.ID)
			}
			if err = WriteJSON(path+".json", p); err != nil {
				return err
			}
			if err = os.Rename(path+".partial", path); err != nil {
				return err
			}
			if err = syncDirectory(filepath.Dir(path)); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		tx, err := s.DB.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var raw string
		if err = tx.QueryRow("SELECT payload FROM events WHERE id=?", p.EventID).Scan(&raw); err != nil {
			return err
		}
		var ev Event
		if err = json.Unmarshal([]byte(raw), &ev); err != nil {
			return err
		}
		// A committed part is idempotent. Do not move an event cursor backwards.
		if ev.Cursor == op.Cursor {
			ev.Cursor = max(ev.Cursor, p.End)
			ev.AssemblyFailures = 0
		}
		if err = putPart(tx, p); err != nil {
			return err
		}
		if err = enqueueTx(tx, "part", p.ID); err != nil {
			return err
		}
		if err = putEvent(tx, ev); err != nil {
			return err
		}
		if err = putOperation(tx, "event:"+ev.ID, fileOperation{Kind: "event", Event: &ev}); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM file_operations WHERE id=?", id); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return s.applyFileOperation("event:" + ev.ID)
	default:
		return errors.New("unknown catalog operation")
	}
	_, err := s.DB.Exec("DELETE FROM file_operations WHERE id=?", id)
	return err
}
func (s *Store) ReplayOperations(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var id string
		err := s.DB.QueryRowContext(ctx, "SELECT id FROM file_operations ORDER BY id LIMIT 1").Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		s.files.Lock()
		err = s.applyFileOperationContext(ctx, id)
		s.files.Unlock()
		if err != nil {
			return err
		}
	}
}

// Only pending events need reconciliation on an ordinary restart.
func (s *Store) RecoverPending(ctx context.Context) error {
	s.mutate.Lock()
	defer s.mutate.Unlock()
	events, err := s.PendingEvents()
	if err != nil {
		return err
	}
	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			return err
		}
		parts, err := s.Parts(ev.ID)
		if err != nil {
			return err
		}
		for _, p := range parts {
			if p.End > ev.Cursor {
				ev.Cursor = p.End
				ev.AssemblyFailures = 0
			}
		}
		ev.Status = "closing"
		ev.Incomplete = true
		if err = s.SaveEvent(ev); err != nil {
			return err
		}
	}
	return nil
}
func (s *Store) ProbeHealth(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	value := ID()
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT OR REPLACE INTO health_probe VALUES(1,?)", value); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	var actual string
	if err = s.DB.QueryRowContext(ctx, "SELECT value FROM health_probe WHERE id=1").Scan(&actual); err != nil {
		return err
	}
	if actual != value {
		return errors.New("catalog health transaction mismatch")
	}
	return nil
}
