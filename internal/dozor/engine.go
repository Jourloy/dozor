package dozor

import (
	"context"
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
	Store    *Store
	Bins     Binaries
	mu       sync.Mutex
	states   map[string]MotionState
	active   map[string]string
	paused   bool
	assemble func(context.Context, *Store, Binaries, Event, []Segment) (Part, error)
}

func NewEngine(s *Store, b Binaries) *Engine {
	return &Engine{Store: s, Bins: b, states: map[string]MotionState{}, active: map[string]string{}, assemble: Assemble}
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
	for _, s := range e.states {
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
	for _, ev := range events {
		expired := now.UnixMilli() > ev.End+12000 || ev.Status == "closing"
		segs, err := e.Store.Segments(ev.CameraID, ev.Cursor, ev.End)
		if err != nil {
			return err
		}
		if len(segs) > 0 {
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
					return err
				}
				ev.Cursor = p.End
				if err = e.Store.SaveEvent(ev); err != nil {
					return err
				}
				// A long event can have several pending chunks; drain one per tick.
				if len(selected) < len(segs) {
					continue
				}
			}
		}
		if expired {
			if ev.Cursor < ev.End-1500 {
				ev.Incomplete = true
			}
			ev.Status = "closed"
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
	return e.Store.PruneBuffer(now)
}
