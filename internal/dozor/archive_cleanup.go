package dozor

import (
	"context"
	"errors"
	"fmt"
	"os"
)

const invalidVideoAttempts = 3

// A bad input can fail a whole minute's assembly. Check each completed source
// separately so that usable fragments from the same batch are preserved.
func (e *Engine) discardInvalidSegments(ctx context.Context, ev *Event, segs []Segment) (bool, error) {
	type invalidSegment struct {
		path string
		err  error
	}
	var invalid []invalidSegment
	for _, seg := range segs {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := e.Store.Guard.Check(); err != nil {
			return false, err
		}
		path, err := checkedPath(e.Store.Root, seg.Path)
		if err != nil {
			return false, err
		}
		_, err = os.Stat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if err == nil {
			_, err = Probe(ctx, e.Bins.FFprobe, path, false)
			if !invalidVideoData(err) {
				continue
			}
		}
		invalid = append(invalid, invalidSegment{seg.Path, err})
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if len(invalid) == 0 {
		return false, nil
	}
	// Persist the gap before unlinking. Keep the failure count until cleanup
	// finishes, so an interrupted deletion is retried after recovery.
	ev.Incomplete = true
	if err := e.Store.SaveEvent(*ev); err != nil {
		return false, err
	}
	for _, seg := range invalid {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		if err := e.Store.RemoveSegment(seg.path); err != nil {
			return false, err
		}
		e.Store.Notice(fmt.Sprintf("Удалён повреждённый фрагмент после %d ошибок сборки: камера %s, событие %s, %s: %v", invalidVideoAttempts, ev.CameraID, ev.ID, seg.path, seg.err))
	}
	return true, nil
}
