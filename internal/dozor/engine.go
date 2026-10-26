package dozor

import (
	"context"
	"errors"
	"fmt"
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
	Store          *Store
	Bins           Binaries
	mu             sync.Mutex
	tickMu         sync.Mutex
	assemblyCancel context.CancelFunc
	states         map[string]MotionState
	pendingMotion  map[string][]motionWindow
	active         map[string]string
	online         map[string]bool
	disconnected   map[string]int64
	retries        map[string]assemblyRetry
	paused         bool
	deferRecovered bool
	assemble       func(context.Context, *Store, Binaries, Event, []Segment) (Part, error)
}

const (
	disconnectSource       = "disconnect"
	disconnectClipDuration = 60 * time.Second
	motionPostroll         = 10 * time.Second
)

// Keep detected intervals until the worker persists them. A quiet frame must
// not erase a short pulse, including while another camera's video is assembled.
type motionWindow struct {
	start, end time.Time
	source     string
}

type assemblyRetry struct {
	next  time.Time
	delay time.Duration
}

func NewEngine(s *Store, b Binaries) *Engine {
	return &Engine{Store: s, Bins: b, states: map[string]MotionState{}, pendingMotion: map[string][]motionWindow{}, active: map[string]string{}, online: map[string]bool{}, disconnected: map[string]int64{}, retries: map[string]assemblyRetry{}, assemble: Assemble}
}

func (e *Engine) SetOnline(camera string, online bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.online[camera] = online
}

// Disconnect closes every pending window and saves the buffered minute before
// the outage as its own recording. Only that recording carries the marker, so an
// older motion clip is never reported as the moment the camera went offline.
// A detector failure may still request continuous recording, but cannot extend
// an event while its main stream is offline.
func (e *Engine) Disconnect(camera string, at time.Time) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Store.mutate.Lock()
	defer e.Store.mutate.Unlock()
	if err := e.recordMotion(camera, at); err != nil {
		return err
	}
	e.online[camera] = false
	previous := e.disconnected[camera]
	e.disconnected[camera] = at.UnixMilli()
	delete(e.active, camera)
	events, err := e.Store.PendingEvents()
	if err != nil {
		return err
	}
	for _, ev := range events {
		if ev.CameraID != camera {
			continue
		}
		if ev.DisconnectedAt == at.UnixMilli() {
			return nil
		}
		ev.End = max(ev.Start, min(ev.End, at.UnixMilli()))
		ev.Status = "closing"
		if err := e.Store.SaveEvent(ev); err != nil {
			return err
		}
	}
	start := max(at.Add(-disconnectClipDuration).UnixMilli(), previous)
	segs, err := e.Store.Segments(camera, start, at.UnixMilli())
	if err != nil || len(segs) == 0 {
		return err
	}
	// Stays open briefly so the recorder can hand over the file it was writing.
	ev := Event{ID: ID(), CameraID: camera, Source: disconnectSource, Start: start, Cursor: start, End: at.UnixMilli(), Status: "open", DisconnectedAt: at.UnixMilli()}
	return e.Store.SaveEvent(ev)
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

// Completion hooks may arrive after an event closes. Reopen every event that
// still needs this tail, including ordinary motion recordings.
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
		ev.Status = "closing"
		ev.Uploaded = false
		if err = e.Store.SaveEvent(ev); err != nil {
			return err
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
	if online, known := e.online[v.CameraID]; known && !online {
		return
	}
	if v.Active || !v.Healthy {
		source := v.Source
		if !v.Healthy {
			source = "detector_failure"
		}
		e.queueMotion(v.CameraID, motionWindow{started, v.At, source})
	}
}

// Caller owns mu. Consecutive positive signals describe continuous motion even
// if the worker has been busy for longer than the merging window.
func (e *Engine) queueMotion(camera string, window motionWindow) {
	windows := e.pendingMotion[camera]
	if n := len(windows); n > 0 && !window.start.After(windows[n-1].end.Add(motionPostroll)) {
		if window.end.After(windows[n-1].end) {
			windows[n-1].end = window.end
		}
	} else {
		windows = append(windows, window)
	}
	e.pendingMotion[camera] = windows
}

// Caller owns mu and Store.mutate. Consume a window only after its event is
// saved, so a catalog error does not acknowledge motion that was never recorded.
func (e *Engine) recordMotion(camera string, now time.Time) error {
	if online, known := e.online[camera]; known && !online {
		return nil
	}
	if st, exists := e.states[camera]; exists && (st.Active || !st.Healthy) {
		source := st.Source
		if !st.Healthy {
			source = "detector_failure"
		}
		e.queueMotion(camera, motionWindow{st.Started, now, source})
	}
	for len(e.pendingMotion[camera]) > 0 {
		window := e.pendingMotion[camera][0]
		id := e.active[camera]
		var ev Event
		if id != "" {
			var err error
			ev, err = e.Store.Event(id)
			if err != nil {
				return eventError(Event{ID: id, CameraID: camera}, "чтение события", err)
			}
			if window.start.UnixMilli() > ev.End {
				id = ""
			}
		}
		if id == "" {
			ev = Event{ID: ID(), CameraID: camera, Source: window.source, Start: window.start.Add(-time.Minute).UnixMilli(), Status: "open"}
			ev.Start = max(ev.Start, e.disconnected[camera])
			ev.Cursor = ev.Start
		}
		ev.End = max(ev.End, window.end.Add(motionPostroll).UnixMilli())
		if err := e.Store.SaveEvent(ev); err != nil {
			return eventError(ev, "сохранение события", err)
		}
		e.active[camera] = ev.ID
		e.pendingMotion[camera] = e.pendingMotion[camera][1:]
	}
	delete(e.pendingMotion, camera)
	return nil
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
func (e *Engine) PrepareUpdate(immediate bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if immediate {
		// Shutdown finishes the current streams; startup reconciles pending events.
		e.paused = true
		if e.assemblyCancel != nil {
			e.assemblyCancel()
		}
		return true
	}
	if len(e.active) > 0 {
		return false
	}
	for camera, s := range e.states {
		if online, known := e.online[camera]; known && !online {
			continue
		}
		if s.Active || !s.Healthy || len(e.pendingMotion[camera]) > 0 {
			return false
		}
	}
	e.paused = true
	return true
}
func (e *Engine) Tick(ctx context.Context, now time.Time) error {
	if !e.tickMu.TryLock() {
		return nil
	}
	defer e.tickMu.Unlock()
	started := time.Now()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Store.mutate.Lock()
	defer e.Store.mutate.Unlock()
	if !e.paused {
		for cam := range e.states {
			if err := e.recordMotion(cam, now); err != nil {
				return err
			}
		}
	}
	events, err := e.Store.PendingEvents()
	if err != nil {
		return fmt.Errorf("чтение незавершённых событий: %w", err)
	}
	var assemblyErrors eventFailures
eventsLoop:
	for _, ev := range events {
		if e.deferRecovered && ev.Status == "closing" {
			continue
		}
		if now.Before(e.retries[ev.ID].next) {
			continue
		}
		expired := now.UnixMilli() > ev.End+12000 || ev.Status == "closing"
		segs, err := e.Store.Segments(ev.CameraID, ev.Cursor, ev.End)
		if err != nil {
			return eventError(ev, "чтение фрагментов", err)
		}
		for len(segs) > 0 {
			selected := []Segment{}
			for _, seg := range segs {
				selected = append(selected, seg)
				// The pre-outage clip is one file, so its marker covers the whole minute.
				if ev.Source != disconnectSource && seg.End-selected[0].Start >= 60000 {
					break
				}
			}
			duration := selected[len(selected)-1].End - selected[0].Start
			if (duration >= 60000 && ev.Source != disconnectSource) || expired {
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
				assemblyCtx, cancelAssembly := context.WithCancel(ctx)
				e.assemblyCancel = cancelAssembly
				e.Store.mutate.Unlock()
				e.mu.Unlock()
				p, err := e.assemble(assemblyCtx, e.Store, e.Bins, ev, selected)
				cancelAssembly()
				e.mu.Lock()
				e.Store.mutate.Lock()
				e.assemblyCancel = nil
				if errors.Is(err, context.Canceled) && e.paused {
					return nil
				}
				latest, readErr := e.Store.Event(ev.ID)
				if readErr != nil {
					return readErr
				}
				latest.ShortPrebuffer = latest.ShortPrebuffer || ev.ShortPrebuffer
				latest.Incomplete = latest.Incomplete || ev.Incomplete
				ev = latest
				if errors.Is(err, errStaleAssembly) {
					continue eventsLoop
				}
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					err = eventError(ev, fmt.Sprintf("сборка из %s — %s (%d фрагм.)", selected[0].Path, selected[len(selected)-1].Path, len(selected)), err)
					if !errors.Is(err, errVideoAssembly) {
						return err
					}
					if guardErr := e.Store.Guard.Check(); guardErr != nil {
						return eventError(ev, "проверка диска", guardErr)
					}
					failures := min(ev.AssemblyFailures+1, invalidVideoAttempts)
					if failures != ev.AssemblyFailures {
						ev.AssemblyFailures = failures
						if saveErr := e.Store.SaveEvent(ev); saveErr != nil {
							return eventError(ev, "сохранение ошибок сборки", saveErr)
						}
					}
					if failures >= invalidVideoAttempts && invalidVideoData(err) {
						removed, cleanupErr := e.discardInvalidSegments(ctx, &ev, selected, err)
						if cleanupErr != nil {
							return eventError(ev, "удаление повреждённых фрагментов", cleanupErr)
						}
						if removed {
							ev.AssemblyFailures = 0
							if saveErr := e.Store.SaveEvent(ev); saveErr != nil {
								return eventError(ev, "сохранение события", saveErr)
							}
							delete(e.retries, ev.ID)
							segs, err = e.Store.Segments(ev.CameraID, ev.Cursor, ev.End)
							if err != nil {
								return eventError(ev, "чтение фрагментов", err)
							}
							continue
						}
					}
					// Keep this event's cursor and buffer for retry, but let
					// later events (including this camera's) make progress.
					retry := e.retries[ev.ID]
					retry.delay = min(max(5*time.Second, retry.delay*2), time.Minute)
					// Include time spent assembling so slow failures also get a pause.
					retry.next = now.Add(time.Since(started) + retry.delay)
					e.retries[ev.ID] = retry
					assemblyErrors = append(assemblyErrors, fmt.Errorf("%w; повтор через %d с", err, int(retry.delay/time.Second)))
					continue eventsLoop
				}
				ev.Cursor = p.End
				ev.AssemblyFailures = 0
				if err = e.Store.SaveEvent(ev); err != nil {
					return eventError(ev, "сохранение события", err)
				}
				delete(e.retries, ev.ID)
				// Assembly releases the locks: another completed segment may have
				// arrived while the event was still open. Drain it before closing.
				segs, err = e.Store.Segments(ev.CameraID, ev.Cursor, ev.End)
				if err != nil {
					return eventError(ev, "чтение фрагментов", err)
				}
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
			ev.AssemblyFailures = 0
			if err = e.markLastPart(ev); err != nil {
				return eventError(ev, "отметка последней части", err)
			}
			if err = e.Store.saveEvent(ev, true); err != nil {
				return eventError(ev, "завершение события", err)
			}
			delete(e.retries, ev.ID)
			if e.active[ev.CameraID] == ev.ID {
				delete(e.active, ev.CameraID)
			}
		}
	}
	if err := e.Store.PruneBuffer(now); err != nil {
		return fmt.Errorf("очистка буфера: %w", err)
	}
	if len(assemblyErrors) > 0 {
		return assemblyErrors
	}
	return nil
}

// eventFailures keeps one error per stuck event. Each is retried on its own
// schedule, so the reporter must throttle them separately: a single joined
// message changes with every retry and would never match the previous one.
type eventFailures []error

func (f eventFailures) Error() string   { return errors.Join(f...).Error() }
func (f eventFailures) Unwrap() []error { return f }

func eventError(ev Event, operation string, err error) error {
	return fmt.Errorf("камера %s, событие %s, %s: %w", ev.CameraID, ev.ID, operation, err)
}
