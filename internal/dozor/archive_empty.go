package dozor

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const (
	// A disconnect recording may still receive the file the recorder was writing.
	emptyEventGrace = 5 * time.Minute
	// Only a long tail without video is worth a new manifest upload.
	emptyTailMinimum = 60 * time.Second
	emptyEventBatch  = 100
)

// A part has video while the Pi keeps its file or S3 has acknowledged it.
func partHasVideo(p Part) bool {
	return !p.Lost && (!p.Deleted || p.Uploaded)
}

func eventHasVideo(parts []Part) bool {
	for _, p := range parts {
		if partHasVideo(p) {
			return true
		}
	}
	return false
}

// Same condition as partHasVideo for SQL; `lost` is always present in the payload.
const partHasVideoSQL = `COALESCE(json_extract(p.payload,'$.lost'),0)=0 AND (p.deleted=0 OR p.uploaded=1)`

// CleanupEmptyEvents removes closed events without any video and trims a closed
// event's end to its last recorded part, so the archive shows only real video.
// Events with a disconnect marker keep their end: a late tail reopens them by it.
func (s *Store) CleanupEmptyEvents(now time.Time) (int, error) {
	s.mutate.Lock()
	defer s.mutate.Unlock()
	if err := s.Guard.Check(); err != nil {
		return 0, err
	}
	before := now.Add(-emptyEventGrace).UnixMilli()
	empty, err := s.eventsWhere(`SELECT payload FROM events e WHERE status='closed' AND end<? AND NOT EXISTS(SELECT 1 FROM parts p WHERE p.event=e.id AND `+partHasVideoSQL+`) ORDER BY start LIMIT ?`, before, emptyEventBatch)
	if err != nil {
		return 0, err
	}
	for _, ev := range empty {
		if err = s.deleteEvent(ev); err != nil {
			return 0, eventError(ev, "удаление события без видео", err)
		}
	}
	tails, err := s.eventsWhere(`SELECT payload FROM events WHERE status='closed' AND end<? AND cursor>start AND end-cursor>=? AND COALESCE(json_extract(payload,'$.disconnected_at'),0)=0 ORDER BY start LIMIT ?`, before, emptyTailMinimum.Milliseconds(), emptyEventBatch)
	if err != nil {
		return 0, err
	}
	for _, ev := range tails {
		ev.End = ev.Cursor
		ev.Uploaded = false
		if err = s.saveEvent(ev, true); err != nil {
			return 0, eventError(ev, "обрезка события без видео", err)
		}
	}
	if len(empty) > 0 {
		s.Notice(fmt.Sprintf("Удалено событий без видео: %d", len(empty)))
	}
	return len(empty) + len(tails), nil
}

func (s *Store) eventsWhere(query string, args ...any) ([]Event, error) {
	rows, err := s.DB.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	v := []Event{}
	for rows.Next() {
		var raw string
		var ev Event
		if err = rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(raw), &ev); err != nil {
			return nil, err
		}
		v = append(v, ev)
	}
	return v, rows.Err()
}

// The deletion intent replaces the event's projection under the same id, so a
// replay can never write event.json back after the directory is gone.
func (s *Store) deleteEvent(ev Event) error {
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
	for _, query := range []string{
		"DELETE FROM jobs WHERE id IN (SELECT 'part:'||id FROM parts WHERE event=?)",
		"DELETE FROM file_operations WHERE id IN (SELECT 'part:'||id FROM parts WHERE event=?)",
		"DELETE FROM parts WHERE event=?",
		"DELETE FROM jobs WHERE id='event:'||?",
		"DELETE FROM events WHERE id=?",
	} {
		if _, err = tx.Exec(query, ev.ID); err != nil {
			return err
		}
	}
	id := "event:" + ev.ID
	if err = putOperation(tx, id, fileOperation{Kind: "delete_event", Event: &ev}); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return s.applyFileOperation(id)
}

func removeEventDirectory(root string, ev Event) error {
	if !safeID.MatchString(ev.ID) || !safeID.MatchString(ev.CameraID) {
		return errors.New("invalid event deletion")
	}
	dir, err := checkedPath(root, eventDir(ev))
	if err != nil {
		return err
	}
	if err = os.RemoveAll(dir); err != nil {
		return err
	}
	if err = syncDirectory(filepath.Dir(dir)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
