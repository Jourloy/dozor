package dozor

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const disconnectPriorityDuration = 90 * 24 * time.Hour

var errArchiveLimit = errors.New("лимит диска достигнут, завершённых записей для удаления больше нет")

// Compare bytes without rounded display percentages or uint64 multiplication overflow.
func atDiskLimit(used, total uint64, percent int) bool {
	threshold := total/100*uint64(percent) + (total%100*uint64(percent)+99)/100
	return used >= threshold
}

type pruneCandidate struct {
	part     Part
	priority bool
}

type pendingPrune struct {
	PartID   string `json:"part_id"`
	Priority bool   `json:"priority"`
}

// Only published parts are eligible. Engine assembly/disconnect and upload
// acknowledgements share this lock, so flags cannot change during selection.
func (s *Store) PruneArchive(now time.Time, policy StoragePolicy, usage func() (uint64, uint64, error)) error {
	if err := policy.validate(); err != nil {
		return err
	}
	s.mutate.Lock()
	defer s.mutate.Unlock()
	if err := s.Guard.Check(); err != nil {
		return err
	}
	// Complete an already committed deletion even if the limit has since changed.
	var pending string
	err := s.DB.QueryRow("SELECT value FROM metadata WHERE key='archive_prune_pending'").Scan(&pending)
	if err == nil {
		var job pendingPrune
		if err = json.Unmarshal([]byte(pending), &job); err != nil {
			return err
		}
		if err = s.finishArchivePrune(job); err != nil {
			return err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	readUsage := func() (uint64, uint64, error) {
		if err := s.Guard.Check(); err != nil {
			return 0, 0, err
		}
		used, total, err := usage()
		if err == nil && total == 0 {
			err = errors.New("не удалось определить ёмкость диска")
		}
		return used, total, err
	}
	used, total, err := readUsage()
	if err != nil {
		return err
	}
	if !atDiskLimit(used, total, policy.MaxDiskUsagePercent) {
		return nil
	}
	rows, err := s.DB.Query(`SELECT p.payload,e.payload FROM parts p JOIN events e ON e.id=p.event WHERE p.deleted=0 ORDER BY p.start,p.id`)
	if err != nil {
		return err
	}
	candidates := []pruneCandidate{}
	for rows.Next() {
		var partJSON, eventJSON string
		var p Part
		var ev Event
		if err = rows.Scan(&partJSON, &eventJSON); err == nil {
			err = json.Unmarshal([]byte(partJSON), &p)
		}
		if err == nil {
			err = json.Unmarshal([]byte(eventJSON), &ev)
		}
		if err != nil {
			rows.Close()
			return err
		}
		protected := ev.DisconnectedAt > 0 && ev.DisconnectedAt > now.Add(-disconnectPriorityDuration).UnixMilli()
		candidates = append(candidates, pruneCandidate{p, protected})
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	// Preserve chronological order within both groups, irrespective of S3 upload.
	sort.SliceStable(candidates, func(i, j int) bool { return !candidates[i].priority && candidates[j].priority })
	for _, candidate := range candidates {
		if !atDiskLimit(used, total, policy.MaxDiskUsagePercent) {
			return nil
		}
		if err = s.Guard.Check(); err != nil {
			return err
		}
		job := pendingPrune{candidate.part.ID, candidate.priority}
		data, err := json.Marshal(job)
		if err != nil {
			return err
		}
		if _, err = s.DB.Exec("INSERT OR REPLACE INTO metadata(key,value) VALUES('archive_prune_pending',?)", string(data)); err != nil {
			return err
		}
		if err = s.finishArchivePrune(job); err != nil {
			return err
		}
		used, total, err = readUsage()
		if err != nil {
			return err
		}
	}
	if atDiskLimit(used, total, policy.MaxDiskUsagePercent) {
		return errArchiveLimit
	}
	return nil
}

// The durable pending marker allows a failed unlink/sidecar write to be retried
// on the next tick. Existing part tombstones still support catalog reconstruction.
func (s *Store) finishArchivePrune(job pendingPrune) error {
	if err := s.Guard.Check(); err != nil {
		return err
	}
	p, err := s.Part(job.PartID)
	if err != nil {
		return err
	}
	ev, err := s.Event(p.EventID)
	if err != nil {
		return err
	}
	full, err := checkedPath(s.Root, p.Path)
	if err != nil {
		return err
	}
	if filepath.Dir(p.Path) != eventDir(ev) || filepath.Ext(p.Path) != ".mp4" {
		return errors.New("неверный путь завершённой записи")
	}
	p.Deleted = true
	p.Lost = !p.Uploaded
	if err = s.SavePart(p); err != nil {
		return err
	}
	if p.Lost {
		ev.Lost = true
		ev.Uploaded = false
		if err = s.SaveEvent(ev); err != nil {
			return err
		}
		if err = s.Enqueue("event", ev.ID); err != nil {
			return err
		}
	}
	if err = s.Guard.Check(); err != nil {
		return err
	}
	if err = os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err = s.DoneJob("part:" + p.ID); err != nil {
		return err
	}
	if job.Priority {
		s.Notice(fmt.Sprintf("Для соблюдения лимита диска удалена запись перед отключением младше 90 дней: событие %s, фрагмент %s", p.EventID, p.ID))
	}
	if p.Lost {
		s.Notice("Удалена невыгруженная запись: " + p.ID)
	}
	_, err = s.DB.Exec("DELETE FROM metadata WHERE key='archive_prune_pending'")
	return err
}
