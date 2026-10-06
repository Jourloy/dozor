package dozor

import (
	"context"
	"errors"
	"sync"
	"time"
)

type MotionSignal struct {
	CameraID string
	Active   bool
	Healthy  bool
	Source   string
	At       time.Time
}
type MotionState struct {
	Active  bool
	Healthy bool
	Source  string
	Updated time.Time
	Started time.Time
}
type Engine struct {
	Store        *Store
	Bins         Binaries
	mu           sync.Mutex
	states       map[string]MotionState
	active       map[string]string
	online       map[string]bool
	disconnected map[string]int64
	paused       bool
	assemble     func(context.Context, *Store, Binaries, Event, []Segment) (Part, error)
}

func NewEngine(s *Store, b Binaries) *Engine {
	return &Engine{Store: s, Bins: b, states: map[string]MotionState{}, active: map[string]string{}, online: map[string]bool{}, disconnected: map[string]int64{}, assemble: Assemble}
}

func (e *Engine) SetOnline(camera string, online bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.online[camera] = online
}

// Disconnect closes every pending window and marks only the latest recording.
// A detector failure may still request continuous recording, but cannot extend
// an event while its main stream is offline.
func (e *Engine) Disconnect(camera string, at time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Store.mutate.Lock()
	defer e.Store.mutate.Unlock()
	e.online[camera] = false
	e.disconnected[camera] = at.UnixMilli()
	delete(e.active, camera)
	events, err := e.Store.PendingEvents()
	if err != nil {
		return err
	}
	latest, err := e.Store.Events(camera, 1)
	if err != nil {
		return err
	}
	for _, ev := range events {
		if ev.CameraID != camera {
			continue
		}
		ev.End = max(ev.Start, min(ev.End, at.UnixMilli()))
		ev.Status = "closing"
		if err := e.Store.SaveEvent(ev); err != nil {
			return err
		}
	}
	if len(latest) == 0 {
		return nil
	}
	ev, err := e.Store.Event(latest[0].ID)
	if err != nil || ev.DisconnectedAt != 0 {
		return err
	}
	ev.DisconnectedAt = at.UnixMilli()
	ev.Uploaded = false
	if err = e.Store.SaveEvent(ev); err != nil {
		return err
	}
	if ev.Status == "closed" {
		if err = e.markLastPart(ev); err != nil {
			return err
		}
		return e.Store.Enqueue("event", ev.ID)
	}
	return nil
}

func (e *Engine) markLastPart(ev Event) error {
	if ev.DisconnectedAt == 0 {
		return nil
	}
	parts, err := e.Store.Parts(ev.ID)
	if err != nil {
		return err
	}
	for i, part := range parts {
		at := int64(0)
		if i == len(parts)-1 {
			at = ev.DisconnectedAt
		}
		if part.DisconnectedAt != at {
			part.DisconnectedAt = at
			if err = e.Store.SavePart(part); err != nil {
				return err
			}
		}
	}
	return nil
}

// Completion hooks may race the offline hook. Reopen only the interrupted
// event that owns a late tail, without waiting for a camera reconnection.
func (e *Engine) AddSegment(seg Segment) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Store.mutate.Lock()
	defer e.Store.mutate.Unlock()
	if err := e.Store.AddSegment(seg); err != nil {
		return err
	}
	rows, err := e.Store.DB.Query("SELECT id FROM events WHERE camera=? AND status='closed' AND cursor<? AND end>?", seg.CameraID, seg.End, seg.Start)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		ev, err := e.Store.Event(id)
		if err != nil {
			return err
		}
		if ev.DisconnectedAt != 0 {
			ev.Status = "closing"
			ev.Uploaded = false
			if err = e.Store.SaveEvent(ev); err != nil {
				return err
			}
		}
	}
	return nil
}
func (e *Engine) Signal(v MotionSignal) {
	e.mu.Lock()
	defer e.mu.Unlock()
	prev, exists := e.states[v.CameraID]
	started := prev.Started
	if (v.Active || !v.Healthy) && (!exists || (!prev.Active && prev.Healthy)) {
		started = v.At
	}
	e.states[v.CameraID] = MotionState{Active: v.Active, Healthy: v.Healthy, Source: v.Source, Updated: v.At, Started: started}
}
func (e *Engine) States() map[string]MotionState {
	e.mu.Lock()
	defer e.mu.Unlock()
	v := map[string]MotionState{}
	for k, s := range e.states {
		v[k] = s
	}
	return v
}
func (e *Engine) PrepareUpdate() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if len(e.active) > 0 {
		return false
	}
	for camera, s := range e.states {
		if online, known := e.online[camera]; known && !online {
			continue
		}
		if s.Active || !s.Healthy {
			return false
		}
	}
	e.paused = true
	return true
}
func (e *Engine) Tick(ctx context.Context, now time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Store.mutate.Lock()
	defer e.Store.mutate.Unlock()
	if !e.paused {
		for cam, st := range e.states {
			if online, known := e.online[cam]; known && !online {
				continue
			}
			if !st.Healthy || st.Active {
				id := e.active[cam]
				var ev Event
				if id != "" {
					var err error
					ev, err = e.Store.Event(id)
					if err != nil {
						return err
					}
					// Waiting for the recorder to close its last segment must not
					// extend the ten-second motion merging window.
					if st.Started.UnixMilli() > ev.End {
						id = ""
					}
				}
				if id == "" {
					ev = Event{ID: ID(), CameraID: cam, Source: st.Source, Start: now.Add(-60 * time.Second).UnixMilli(), End: now.Add(10 * time.Second).UnixMilli(), Status: "open"}
					ev.Start = max(ev.Start, e.disconnected[cam])
					ev.Cursor = ev.Start
					if !st.Healthy {
						ev.Source = "detector_failure"
					}
					e.active[cam] = ev.ID
				}
				ev.End = now.Add(10 * time.Second).UnixMilli()
				if err := e.Store.SaveEvent(ev); err != nil {
					return err
				}
			}
		}
	}
	events, err := e.Store.PendingEvents()
	if err != nil {
		return err
	}
	var assemblyError error
eventsLoop:
	for _, ev := range events {
		expired := now.UnixMilli() > ev.End+12000 || ev.Status == "closing"
		segs, err := e.Store.Segments(ev.CameraID, ev.Cursor, ev.End)
		if err != nil {
			return err
		}
		for len(segs) > 0 {
			selected := []Segment{}
			for _, seg := range segs {
				selected = append(selected, seg)
				if seg.End-selected[0].Start >= 60000 {
					break
				}
			}
			duration := selected[len(selected)-1].End - selected[0].Start
			if duration >= 60000 || expired {
				if ev.Cursor == ev.Start && selected[0].Start > ev.Start+1000 {
					ev.ShortPrebuffer = true
				}
				prev := ev.Cursor
				for _, seg := range selected {
					if seg.Start > prev+1500 {
						ev.Incomplete = true
					}
					prev = seg.End
				}
				p, err := e.assemble(ctx, e.Store, e.Bins, ev, selected)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if !errors.Is(err, errVideoAssembly) {
						return err
					}
					if guardErr := e.Store.Guard.Check(); guardErr != nil {
						return guardErr
					}
					// Keep this event's cursor and buffer for retry, but let
					// later events (including this camera's) make progress.
					assemblyError = errors.Join(assemblyError, err)
					continue eventsLoop
				}
				ev.Cursor = p.End
				if err = e.Store.SaveEvent(ev); err != nil {
					return err
				}
				segs = segs[len(selected):]
				// Closed streams must drain their entire tail on this tick.
				if expired {
					continue
				}
			}
			break
		}
		if expired {
			if ev.Cursor < ev.End-1500 {
				ev.Incomplete = true
			}
			ev.Status = "closed"
			if err = e.markLastPart(ev); err != nil {
				return err
			}
			if err = e.Store.SaveEvent(ev); err != nil {
				return err
			}
			if err = e.Store.Enqueue("event", ev.ID); err != nil {
				return err
			}
			if e.active[ev.CameraID] == ev.ID {
				delete(e.active, ev.CameraID)
			}
		}
	}
	if err := e.Store.PruneBuffer(now); err != nil {
		return err
	}
	return assemblyError
}
