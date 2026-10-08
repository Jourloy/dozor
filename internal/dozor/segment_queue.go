package dozor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// The recorder hook persists completion before trying HTTP. A full channel or
// a stopped application cannot discard a completed segment notification.
func QueueSegment(root, path string, duration float64) (string, error) {
	seg, err := ParseSegment(root, path, duration)
	if err != nil {
		return "", err
	}
	name := filepath.Join(root, ".segment-queue", seg.CameraID+"-"+filepath.Base(seg.Path)+".json")
	return name, WriteJSON(name, seg)
}
func DrainSegmentQueue(ctx context.Context, s *Store, register func(Segment) error) error {
	dir := filepath.Join(s.Root, ".segment-queue")
	f, err := os.Open(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	entries, err := f.ReadDir(recoveryBatch)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var seg Segment
		decodeErr := json.Unmarshal(data, &seg)
		parsed, err := ParseSegment(s.Root, filepath.Join(s.Root, seg.Path), float64(seg.End-seg.Start)/1000)
		if decodeErr != nil || err != nil || parsed != seg {
			s.Notice(fmt.Sprintf("Повреждённое уведомление фрагмента: %s", entry.Name()))
			rejected := filepath.Join(s.Root, ".segment-queue-invalid")
			if err := os.MkdirAll(rejected, 0700); err != nil {
				return err
			}
			if err := os.Rename(path, filepath.Join(rejected, entry.Name())); err != nil {
				return err
			}
			if err := syncDirectory(rejected); err != nil {
				return err
			}
			if err := syncDirectory(dir); err != nil {
				return err
			}
			continue
		}
		if err = register(seg); err != nil {
			return err
		}
		if err = removeDurable(path); err != nil {
			return err
		}
	}
	return nil
}
