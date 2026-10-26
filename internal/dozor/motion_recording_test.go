package dozor

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func addMotionSegment(t *testing.T, e *Engine, camera string, base time.Time, from, until int) {
	t.Helper()
	start := base.Add(time.Duration(from) * time.Second)
	path := filepath.Join("buffer", camera, start.Format("2006-01-02_15-04-05.000000")+".mp4")
	must(t, e.AddSegment(Segment{Path: path, CameraID: camera, Start: start.UnixMilli(), End: base.Add(time.Duration(until) * time.Second).UnixMilli()}))
}

func TestShortMotionBetweenTicksStartsEvent(t *testing.T) {
	s := testStore(t)
	e := recordingEngine(t, s)
	camera, base := ID(), time.Now().UTC().Truncate(time.Second)
	e.Signal(MotionSignal{CameraID: camera, Healthy: true, At: base})
	must(t, e.Tick(context.Background(), base))
	e.Signal(MotionSignal{CameraID: camera, Active: true, Healthy: true, At: base.Add(200 * time.Millisecond)})
	e.Signal(MotionSignal{CameraID: camera, Healthy: true, At: base.Add(800 * time.Millisecond)})
	if e.PrepareUpdate(false) {
		t.Fatal("automatic update ignored pending motion")
	}
	must(t, e.Tick(context.Background(), base.Add(time.Second)))
	events, err := s.Events(camera, 10)
	must(t, err)
	if len(events) != 1 {
		t.Fatalf("short motion was lost: %+v", events)
	}
	if events[0].Start != base.Add(-time.Minute+200*time.Millisecond).UnixMilli() || events[0].End != base.Add(10*time.Second+200*time.Millisecond).UnixMilli() {
		t.Fatalf("event did not use detection time: %+v", events[0])
	}
	must(t, e.Tick(context.Background(), base.Add(2*time.Second)))
	events, err = s.Events(camera, 10)
	must(t, err)
	if len(events) != 1 {
		t.Fatal("consumed motion was recorded twice")
	}
}

func TestRepeatedMotionBetweenTicksExtendsEvent(t *testing.T) {
	s := testStore(t)
	e := recordingEngine(t, s)
	camera, base := ID(), time.Now().UTC().Truncate(time.Second)
	for second := -60; second < 0; second += 5 {
		addMotionSegment(t, e, camera, base, second, second+5)
	}
	e.Signal(MotionSignal{CameraID: camera, Active: true, Healthy: true, At: base})
	must(t, e.Tick(context.Background(), base))
	for second := 1; second <= 25; second++ {
		at := base.Add(time.Duration(second) * time.Second)
		e.Signal(MotionSignal{CameraID: camera, Active: true, Healthy: true, At: at.Add(-800 * time.Millisecond)})
		e.Signal(MotionSignal{CameraID: camera, Healthy: true, At: at.Add(-200 * time.Millisecond)})
		if second%5 == 0 {
			addMotionSegment(t, e, camera, base, second-5, second)
		}
		must(t, e.Tick(context.Background(), at))
	}
	events, err := s.Events(camera, 10)
	must(t, err)
	if len(events) != 1 {
		t.Fatalf("events: %+v", events)
	}
	ev := events[0]
	parts, err := s.Parts(ev.ID)
	must(t, err)
	if ev.End < base.Add(34*time.Second).UnixMilli() {
		t.Fatalf("motion pulses continued through +24.8s, but event status=%s end=+%.1fs; %d saved parts end at +%.1fs", ev.Status, float64(ev.End-base.UnixMilli())/1000, len(parts), float64(parts[len(parts)-1].End-base.UnixMilli())/1000)
	}
}

func TestLateMotionTailIsSaved(t *testing.T) {
	for _, late := range []bool{false, true} {
		t.Run(fmt.Sprintf("late=%v", late), func(t *testing.T) {
			s := testStore(t)
			e := recordingEngine(t, s)
			camera, base := ID(), time.Now().UTC().Truncate(time.Second)
			for second := -60; second < 0; second += 5 {
				addMotionSegment(t, e, camera, base, second, second+5)
			}
			e.Signal(MotionSignal{CameraID: camera, Active: true, Healthy: true, At: base})
			must(t, e.Tick(context.Background(), base))
			e.Signal(MotionSignal{CameraID: camera, Healthy: true, At: base.Add(time.Second)})
			addMotionSegment(t, e, camera, base, 0, 5)
			if !late {
				addMotionSegment(t, e, camera, base, 5, 10)
			}
			must(t, e.Tick(context.Background(), base.Add(23*time.Second)))
			if late {
				addMotionSegment(t, e, camera, base, 5, 10)
			}
			must(t, e.Tick(context.Background(), base.Add(24*time.Second)))
			events, err := s.Events(camera, 10)
			must(t, err)
			ev := events[0]
			parts, err := s.Parts(ev.ID)
			must(t, err)
			end := parts[len(parts)-1].End
			if end < ev.End {
				t.Fatalf("valid +5..+10s tail arrived after closing; status=%s incomplete=%v, archive still ends at +%.1fs instead of +10s", ev.Status, ev.Incomplete, float64(end-base.UnixMilli())/1000)
			}
		})
	}
}

func TestTailAssemblyErrorsPreserveSavedPrefix(t *testing.T) {
	s := testStore(t)
	e := recordingEngine(t, s)
	camera, base := ID(), time.Now().UTC().Truncate(time.Second)
	ev := Event{ID: ID(), CameraID: camera, Start: base.Add(-time.Minute).UnixMilli(), Cursor: base.UnixMilli(), End: base.Add(5 * time.Second).UnixMilli(), Status: "closing"}
	must(t, s.SaveEvent(ev))
	prefix := fixturePart(t, s, ev, ev.Start, ev.Cursor)
	addMotionSegment(t, e, camera, base, 0, 5)
	e.assemble = func(context.Context, *Store, Binaries, Event, []Segment) (Part, error) {
		return Part{}, errors.Join(errVideoAssembly, context.DeadlineExceeded)
	}
	for _, second := range []int{0, 6, 17, 38, 79, 140} {
		if err := e.Tick(context.Background(), base.Add(time.Duration(second)*time.Second)); !errors.Is(err, errVideoAssembly) {
			t.Fatalf("expected retryable failure: %v", err)
		}
	}
	if _, err := s.Part(prefix.ID); err != nil {
		t.Fatalf("tail assembly timeouts removed the saved prefix: %v", err)
	}
	must(t, s.PruneBuffer(base.Add(10*time.Minute)))
	segments, err := s.Segments(camera, ev.Cursor, ev.End)
	must(t, err)
	if len(segments) != 1 {
		t.Fatal("retry input was pruned")
	}
	recovered := recordingEngine(t, s)
	must(t, recovered.Tick(context.Background(), base.Add(11*time.Minute)))
	actual, err := s.Event(ev.ID)
	must(t, err)
	parts, err := s.Parts(ev.ID)
	must(t, err)
	if actual.Status != "closed" || actual.Cursor != ev.End || len(parts) != 2 || parts[0].ID != prefix.ID {
		t.Fatalf("recovery lost video: %+v %+v", actual, parts)
	}
}

func TestDelayedWorkerPreservesSeparateMotionWindows(t *testing.T) {
	s := testStore(t)
	e := recordingEngine(t, s)
	camera, base := ID(), time.Now().UTC().Truncate(time.Second)
	for _, second := range []int{0, 20} {
		at := base.Add(time.Duration(second) * time.Second)
		e.Signal(MotionSignal{CameraID: camera, Active: true, Healthy: true, At: at})
		e.Signal(MotionSignal{CameraID: camera, Healthy: true, At: at.Add(time.Second)})
	}
	must(t, e.Tick(context.Background(), base.Add(25*time.Second)))
	events, err := s.Events(camera, 10)
	must(t, err)
	if len(events) != 2 || events[0].End != base.Add(30*time.Second).UnixMilli() || events[1].End != base.Add(10*time.Second).UnixMilli() || events[1].Start != base.Add(-time.Minute).UnixMilli() {
		t.Fatalf("delayed processing changed the motion windows: %+v", events)
	}
}

func TestMotionDuringAssemblyIsRetained(t *testing.T) {
	s := testStore(t)
	e := recordingEngine(t, s)
	camera, base := ID(), time.Now().UTC().Truncate(time.Second)
	for second := -60; second < 0; second += 5 {
		addMotionSegment(t, e, camera, base, second, second+5)
	}
	assemble := e.assemble
	e.assemble = func(ctx context.Context, s *Store, b Binaries, ev Event, segs []Segment) (Part, error) {
		e.Signal(MotionSignal{CameraID: camera, Healthy: true, At: base.Add(time.Second)})
		e.Signal(MotionSignal{CameraID: camera, Active: true, Healthy: true, At: base.Add(2 * time.Second)})
		e.Signal(MotionSignal{CameraID: camera, Healthy: true, At: base.Add(3 * time.Second)})
		return assemble(ctx, s, b, ev, segs)
	}
	e.Signal(MotionSignal{CameraID: camera, Active: true, Healthy: true, At: base})
	must(t, e.Tick(context.Background(), base))
	must(t, e.Tick(context.Background(), base.Add(5*time.Second)))
	events, err := s.Events(camera, 10)
	must(t, err)
	if len(events) != 1 || events[0].End != base.Add(12*time.Second).UnixMilli() {
		t.Fatalf("assembly erased a detected pulse: %+v", events)
	}
}

func TestLateMotionTailRepublishesManifestOnce(t *testing.T) {
	s := testStore(t)
	e := recordingEngine(t, s)
	camera, base := ID(), time.Now().UTC().Truncate(time.Second)
	ev := Event{ID: ID(), CameraID: camera, Start: base.UnixMilli(), Cursor: base.Add(5 * time.Second).UnixMilli(), End: base.Add(10 * time.Second).UnixMilli(), Status: "closed", Incomplete: true}
	must(t, s.saveEvent(ev, true))
	prefix := fixturePart(t, s, ev, ev.Start, ev.Cursor)
	remote := &memoryRemote{}
	cfg := S3Config{Prefix: "dozor"}
	drainUploads(t, s, cfg, remote)
	if len(remote.keys) != 2 {
		t.Fatalf("initial upload did not finish: %v", remote.keys)
	}
	addMotionSegment(t, e, camera, base, 5, 10)
	reopened, err := s.Event(ev.ID)
	must(t, err)
	if reopened.Status != "closing" || reopened.Uploaded {
		t.Fatalf("late motion tail kept a stale cloud acknowledgement: %+v", reopened)
	}
	// Reopening survives a worker restart and duplicate completion callbacks.
	e = recordingEngine(t, s)
	addMotionSegment(t, e, camera, base, 5, 10)
	must(t, e.Tick(context.Background(), base.Add(24*time.Second)))
	drainUploads(t, s, cfg, remote)
	parts, err := s.Parts(ev.ID)
	must(t, err)
	actual, err := s.Event(ev.ID)
	must(t, err)
	if len(parts) != 2 || parts[0].ID != prefix.ID || parts[1].End != ev.End || !parts[1].Uploaded || !actual.Uploaded || actual.Status != "closed" || len(remote.keys) != 4 {
		t.Fatalf("late tail was not published: %+v %+v %v", actual, parts, remote.keys)
	}
	addMotionSegment(t, e, camera, base, 5, 10)
	must(t, e.Tick(context.Background(), base.Add(25*time.Second)))
	drainUploads(t, s, cfg, remote)
	if len(remote.keys) != 4 || s.QueueCount() != 0 {
		t.Fatalf("duplicate tail uploaded twice: %v", remote.keys)
	}
}

func TestMotionTailArrivingDuringAssemblyIsDrained(t *testing.T) {
	s := testStore(t)
	e := recordingEngine(t, s)
	camera, base := ID(), time.Now().UTC().Truncate(time.Second)
	ev := Event{ID: ID(), CameraID: camera, Start: base.UnixMilli(), Cursor: base.UnixMilli(), End: base.Add(10 * time.Second).UnixMilli(), Status: "closing"}
	must(t, s.SaveEvent(ev))
	addMotionSegment(t, e, camera, base, 0, 5)
	assemble, calls := e.assemble, 0
	e.assemble = func(ctx context.Context, s *Store, b Binaries, ev Event, segs []Segment) (Part, error) {
		calls++
		if calls == 1 {
			addMotionSegment(t, e, camera, base, 5, 10)
		}
		return assemble(ctx, s, b, ev, segs)
	}
	must(t, e.Tick(context.Background(), base.Add(23*time.Second)))
	actual, err := s.Event(ev.ID)
	must(t, err)
	parts, err := s.Parts(ev.ID)
	must(t, err)
	if actual.Status != "closed" || actual.Incomplete || actual.Cursor != ev.End || len(parts) != 2 || parts[1].End != ev.End {
		t.Fatalf("closing missed a tail registered during assembly: %+v %+v", actual, parts)
	}
}
