package dozor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func recordingEngine(t *testing.T, s *Store) *Engine {
	t.Helper()
	e := NewEngine(s, Binaries{})
	e.assemble = func(_ context.Context, s *Store, _ Binaries, ev Event, segs []Segment) (Part, error) {
		return fixturePart(t, s, ev, segs[0].Start, segs[len(segs)-1].End), nil
	}
	return e
}

func TestDisconnectFlushesShortTailAndSeparatesReconnection(t *testing.T) {
	s := testStore(t)
	e := recordingEngine(t, s)
	cam, other := ID(), ID()
	now := time.Now().Truncate(time.Second)
	e.SetOnline(cam, true)
	e.SetOnline(other, true)
	e.Signal(MotionSignal{CameraID: cam, Active: true, Healthy: true, At: now})
	e.Signal(MotionSignal{CameraID: other, Active: true, Healthy: true, At: now})
	must(t, e.AddSegment(Segment{Path: "short.mp4", CameraID: cam, Start: now.Add(-5 * time.Second).UnixMilli(), End: now.UnixMilli()}))
	must(t, e.Tick(context.Background(), now))
	events, err := s.Events(cam, 10)
	must(t, err)
	parts, err := s.Parts(events[0].ID)
	must(t, err)
	if len(parts) != 0 {
		t.Fatal("tail should initially wait for more video")
	}
	disconnect := now.Add(time.Second)
	e.Signal(MotionSignal{CameraID: cam, Healthy: false, At: disconnect})
	must(t, e.Disconnect(cam, disconnect))
	must(t, e.Tick(context.Background(), disconnect))
	ev, err := s.Event(events[0].ID)
	must(t, err)
	parts, err = s.Parts(ev.ID)
	must(t, err)
	if ev.Status != "closed" || ev.End != disconnect.UnixMilli() || ev.DisconnectedAt != disconnect.UnixMilli() || len(parts) != 1 || parts[0].DisconnectedAt != ev.DisconnectedAt {
		t.Fatalf("disconnect did not immediately save and mark the tail: %+v %+v", ev, parts)
	}
	otherEvents, err := s.Events(other, 10)
	must(t, err)
	if len(otherEvents) != 1 || otherEvents[0].Status != "open" || otherEvents[0].DisconnectedAt != 0 {
		t.Fatal("disconnect affected another camera", otherEvents)
	}
	must(t, e.Tick(context.Background(), now.Add(30*time.Second)))
	events, err = s.Events(cam, 10)
	must(t, err)
	if len(events) != 1 || events[0].End != ev.End {
		t.Fatal("detector failure extended the offline event", events)
	}
	e.SetOnline(cam, true)
	e.Signal(MotionSignal{CameraID: cam, Active: true, Healthy: true, At: now.Add(31 * time.Second)})
	must(t, e.Tick(context.Background(), now.Add(31*time.Second)))
	events, err = s.Events(cam, 10)
	must(t, err)
	if len(events) != 2 || events[0].ID == ev.ID || events[0].Start < disconnect.UnixMilli() || events[0].DisconnectedAt != 0 {
		t.Fatal("reconnection reused the interrupted recording", events)
	}
}

func TestDisconnectDrainsAllPartsAndAcceptsLateCompletion(t *testing.T) {
	s := testStore(t)
	e := recordingEngine(t, s)
	cam := ID()
	now := time.Now().Truncate(time.Second)
	start := now.Add(-150 * time.Second).UnixMilli()
	ev := Event{ID: ID(), CameraID: cam, Start: start, Cursor: start, End: now.Add(10 * time.Second).UnixMilli(), Status: "open"}
	must(t, s.SaveEvent(ev))
	for i := 0; i < 29; i++ {
		must(t, e.AddSegment(Segment{Path: itoa(i) + ".mp4", CameraID: cam, Start: start + int64(i)*5000, End: start + int64(i+1)*5000}))
	}
	must(t, e.Disconnect(cam, now))
	must(t, e.Tick(context.Background(), now))
	parts, err := s.Parts(ev.ID)
	must(t, err)
	actual, err := s.Event(ev.ID)
	must(t, err)
	if len(parts) != 3 || actual.Status != "closed" || parts[0].DisconnectedAt != 0 || parts[1].DisconnectedAt != 0 || parts[2].DisconnectedAt != now.UnixMilli() {
		t.Fatalf("did not drain/mark only the final part: %+v %+v", actual, parts)
	}
	// The completion notification of the final five seconds arrives later.
	last := Segment{Path: "late.mp4", CameraID: cam, Start: now.Add(-5 * time.Second).UnixMilli(), End: now.UnixMilli()}
	must(t, e.AddSegment(last))
	must(t, e.Tick(context.Background(), now.Add(time.Second)))
	must(t, e.AddSegment(last)) // Duplicate hook must not duplicate video.
	must(t, e.Tick(context.Background(), now.Add(2*time.Second)))
	parts, err = s.Parts(ev.ID)
	must(t, err)
	actual, err = s.Event(ev.ID)
	must(t, err)
	if len(parts) != 4 || parts[2].DisconnectedAt != 0 || parts[3].DisconnectedAt != now.UnixMilli() || actual.Cursor != last.End || actual.Status != "closed" {
		t.Fatalf("late tail was lost or mislabelled: %+v %+v", actual, parts)
	}
}

func TestDisconnectMarksClosedRecordingAndRefreshesManifest(t *testing.T) {
	s := testStore(t)
	e := recordingEngine(t, s)
	now := time.Now().UnixMilli()
	ev := Event{ID: ID(), CameraID: ID(), Start: now - 10000, End: now - 5000, Cursor: now - 5000, Status: "closed"}
	must(t, s.SaveEvent(ev))
	p := fixturePart(t, s, ev, ev.Start, ev.End)
	remote := &memoryRemote{}
	_, err := UploadOne(context.Background(), s, S3Config{}, remote)
	must(t, err)
	must(t, s.Enqueue("event", ev.ID))
	remote.after = func() { must(t, e.Disconnect(ev.CameraID, time.UnixMilli(now))) }
	_, err = UploadOne(context.Background(), s, S3Config{}, remote)
	must(t, err)
	actual, err := s.Event(ev.ID)
	must(t, err)
	if actual.Uploaded || s.QueueCount() != 1 {
		t.Fatal("old in-flight manifest acknowledged the disconnect", actual)
	}
	remote.after = nil
	_, err = UploadOne(context.Background(), s, S3Config{}, remote)
	must(t, err)
	var manifest struct {
		Event Event
		Parts []Part
	}
	data, err := os.ReadFile(filepath.Join(s.Root, eventDir(ev), "manifest.json"))
	must(t, err)
	must(t, json.Unmarshal(data, &manifest))
	if manifest.Event.DisconnectedAt != now || len(manifest.Parts) != 1 || manifest.Parts[0].ID != p.ID || manifest.Parts[0].DisconnectedAt != now || s.QueueCount() != 0 {
		t.Fatalf("manifest lost disconnect marker: %+v", manifest)
	}
	// Marker and history survive reopening the catalog.
	must(t, s.Recover())
	actual, err = s.Event(ev.ID)
	must(t, err)
	if actual.DisconnectedAt != now {
		t.Fatal("disconnect marker lost after recovery", actual)
	}
}

func TestAvailabilityRetentionAndRestartGap(t *testing.T) {
	s := testStore(t)
	cam, other := Camera{ID: ID(), Name: "Двор", Enabled: true}, Camera{ID: ID(), Name: "Склад"}
	now := time.Now().Truncate(time.Second)
	cutoff := now.Add(-24 * time.Hour).UnixMilli()
	must(t, s.SaveAvailability(cam.ID, AvailabilityInterval{Start: cutoff - 10000, End: cutoff - 1, State: "offline"}))
	must(t, s.SaveAvailability(cam.ID, AvailabilityInterval{Start: cutoff - 1, End: cutoff + 10000, State: "online"}))
	must(t, s.SaveAvailability(cam.ID, AvailabilityInterval{Start: cutoff + 10000, End: cutoff + 20000, State: "offline"}))
	must(t, s.PruneAvailability(now))
	r := &Runtime{Store: s, Engine: recordingEngine(t, s)}
	must(t, r.initStreams([]Camera{cam, other}, now.Add(-time.Minute)))
	r.mediaSince.Store(now.Unix())
	r.streamSignals <- StreamSignal{CameraID: cam.ID, Online: true, At: now.Add(-50 * time.Second).UnixMilli()}
	must(t, r.tickStreams(context.Background(), now.Add(-45*time.Second)))
	r.streamSignals <- StreamSignal{CameraID: cam.ID, Online: false, At: now.Add(-40 * time.Second).UnixMilli()}
	must(t, r.tickStreams(context.Background(), now))
	h, err := s.Availability([]Camera{cam, other}, "", now)
	must(t, err)
	if h.Start != cutoff || len(h.Cameras) != 2 || len(h.Cameras[0].Intervals) != 5 || h.Cameras[0].Intervals[0].Start != cutoff || h.Cameras[0].Intervals[1].End >= h.Cameras[0].Intervals[2].Start || h.Cameras[1].Intervals[0].State != "disabled" {
		t.Fatalf("incorrect history or restart gap: %+v", h)
	}
	filtered, err := s.Availability([]Camera{cam, other}, other.ID, now)
	must(t, err)
	if len(filtered.Cameras) != 1 || filtered.Cameras[0].CameraID != other.ID {
		t.Fatal("camera filter ignored", filtered)
	}
	var old int
	must(t, s.DB.QueryRow("SELECT count(*) FROM availability WHERE end<=?", cutoff).Scan(&old))
	if old != 0 {
		t.Fatal("history older than 24h retained")
	}
	root := s.Root
	must(t, s.Close())
	s, err = OpenStore(Guard{Root: root, Development: true})
	must(t, err)
	defer s.Close()
	reloaded, err := s.Availability([]Camera{cam, other}, "", now)
	must(t, err)
	b1, _ := json.Marshal(h)
	b2, _ := json.Marshal(reloaded)
	if string(b1) != string(b2) {
		t.Fatal("history did not survive restart")
	}
}

func TestAvailabilityWatchdogAndDelayedTail(t *testing.T) {
	s := testStore(t)
	cam := Camera{ID: ID(), Enabled: true}
	now := time.Now().Truncate(time.Second)
	r := &Runtime{Store: s, Engine: recordingEngine(t, s)}
	must(t, r.initStreams([]Camera{cam}, now))
	r.mediaSince.Store(now.Unix())
	// An offline detector cannot create phantom recordings before connection.
	r.Engine.Signal(MotionSignal{CameraID: cam.ID, Healthy: false, At: now})
	must(t, r.Engine.Tick(context.Background(), now))
	events, err := s.Events(cam.ID, 10)
	must(t, err)
	if len(events) != 0 {
		t.Fatal("offline camera created an event")
	}
	seg := Segment{Path: "fresh.mp4", CameraID: cam.ID, Start: now.UnixMilli(), End: now.Add(5 * time.Second).UnixMilli()}
	must(t, r.Engine.AddSegment(seg))
	must(t, r.tickStreams(context.Background(), now.Add(5*time.Second)))
	if r.streams[cam.ID].State != "online" {
		t.Fatal("lost online hook was not recovered")
	}
	must(t, r.Engine.Tick(context.Background(), now.Add(5*time.Second)))
	must(t, r.tickStreams(context.Background(), now.Add(26*time.Second)))
	must(t, r.Engine.Tick(context.Background(), now.Add(26*time.Second)))
	events, err = s.Events(cam.ID, 10)
	must(t, err)
	if r.streams[cam.ID].State != "offline" || events[0].Status != "closed" || events[0].DisconnectedAt == 0 {
		t.Fatal("lost offline hook was not recovered", events)
	}
	must(t, r.Engine.AddSegment(Segment{Path: "tail.mp4", CameraID: cam.ID, Start: seg.End, End: seg.End + 5000}))
	must(t, r.tickStreams(context.Background(), now.Add(27*time.Second)))
	if r.streams[cam.ID].State != "offline" {
		t.Fatal("late tail made offline camera online")
	}
	if !r.Engine.PrepareUpdate() {
		t.Fatal("offline detector blocked update")
	}
}

func TestDisablingCameraFlushesItsRecording(t *testing.T) {
	s := testStore(t)
	cam, other := Camera{ID: ID(), Enabled: true}, Camera{ID: ID(), Enabled: true}
	now := time.Now()
	r := &Runtime{Store: s, Engine: recordingEngine(t, s)}
	must(t, r.initStreams([]Camera{cam, other}, now))
	for _, c := range []Camera{cam, other} {
		must(t, r.setStream(context.Background(), c.ID, true, now.UnixMilli()))
		r.Engine.Signal(MotionSignal{CameraID: c.ID, Active: true, Healthy: true, At: now})
	}
	must(t, r.Engine.AddSegment(Segment{Path: "short.mp4", CameraID: cam.ID, Start: now.UnixMilli(), End: now.Add(5 * time.Second).UnixMilli()}))
	must(t, r.Engine.Tick(context.Background(), now.Add(5*time.Second)))
	cam.Enabled = false
	must(t, r.finishDisabledStreams(context.Background(), []Camera{cam, other}, now.Add(6*time.Second)))
	events, err := s.Events(cam.ID, 1)
	must(t, err)
	parts, err := s.Parts(events[0].ID)
	must(t, err)
	if events[0].Status != "closed" || len(parts) != 1 || parts[0].DisconnectedAt == 0 {
		t.Fatal("disabled camera tail was not finalized", events, parts)
	}
	events, err = s.Events(other.ID, 1)
	must(t, err)
	if events[0].DisconnectedAt != 0 {
		t.Fatal("configuration reload labelled another camera as disconnected", events)
	}
}

func TestOfflineReconciliationRecoversNewestSegmentWithoutHook(t *testing.T) {
	s := testStore(t)
	e := recordingEngine(t, s)
	now := time.Now().UTC().Truncate(time.Second)
	cam, online := ID(), ID()
	ev := Event{ID: ID(), CameraID: cam, Start: now.Add(-time.Minute).UnixMilli(), Cursor: now.Add(-5 * time.Second).UnixMilli(), End: now.UnixMilli(), Status: "closed", DisconnectedAt: now.UnixMilli()}
	must(t, s.SaveEvent(ev))
	for _, camera := range []string{cam, online} {
		path := filepath.Join(s.Root, "buffer", camera, now.Add(-5*time.Second).Format("2006-01-02_15-04-05.000000")+".mp4")
		must(t, AtomicWrite(path, []byte("fixture"), 0600))
	}
	probe := filepath.Join(t.TempDir(), "ffprobe")
	must(t, os.WriteFile(probe, []byte("#!/bin/sh\nprintf '%s' '{\"streams\":[{\"codec_type\":\"video\",\"codec_name\":\"h264\"}],\"format\":{\"duration\":\"5\"}}'\n"), 0700))
	e.Bins.FFprobe = probe
	r := &Runtime{Store: s, Engine: e, streams: map[string]AvailabilityInterval{
		cam:    {Start: now.UnixMilli(), State: "offline"},
		online: {Start: now.UnixMilli(), State: "online"},
	}}
	r.mediaSince.Store(now.Unix())
	must(t, r.reconcileSegments(context.Background()))
	must(t, e.Tick(context.Background(), now))
	parts, err := s.Parts(ev.ID)
	must(t, err)
	if len(parts) != 1 || parts[0].End != now.UnixMilli() || parts[0].DisconnectedAt != ev.DisconnectedAt {
		t.Fatal("reconciliation dropped the final closed file", parts)
	}
	segments, err := s.Segments(online, 0, now.UnixMilli())
	must(t, err)
	if len(segments) != 0 {
		t.Fatal("reconciliation read a camera's active file", segments)
	}
	// Still exclude a new stream's active file when its online hook is lost.
	path := filepath.Join(s.Root, "buffer", cam, now.Add(time.Second).Format("2006-01-02_15-04-05.000000")+".mp4")
	must(t, AtomicWrite(path, []byte("open fixture"), 0600))
	must(t, r.reconcileSegments(context.Background()))
	segments, err = s.Segments(cam, now.UnixMilli(), now.Add(time.Minute).UnixMilli())
	must(t, err)
	if len(segments) != 0 {
		t.Fatal("lost online hook allowed reconciliation of an open file", segments)
	}
}

func TestAvailabilityClipsLongRunningIntervalWithoutDuplication(t *testing.T) {
	s := testStore(t)
	cam := Camera{ID: ID()}
	now := time.Now()
	r := &Runtime{Store: s, Engine: recordingEngine(t, s)}
	must(t, r.initStreams([]Camera{cam}, now.Add(-48*time.Hour)))
	must(t, r.tickStreams(context.Background(), now.Add(-time.Second)))
	must(t, r.tickStreams(context.Background(), now))
	var count int
	var start int64
	must(t, s.DB.QueryRow("SELECT count(*),MIN(start) FROM availability WHERE camera=?", cam.ID).Scan(&count, &start))
	if count != 1 || start != now.Add(-24*time.Hour).UnixMilli() {
		t.Fatalf("long-running interval retained old or duplicate data: count=%d start=%d", count, start)
	}
}

func TestAvailabilityAPIAndPrivateStreamHook(t *testing.T) {
	s := testStore(t)
	now := time.Now().Add(-time.Second)
	cam := Camera{ID: ID(), Name: "Двор", Enabled: true}
	a := &App{Config: &ConfigFile{value: Config{Cameras: []Camera{cam}}}, runtime: &Runtime{Store: s, Engine: recordingEngine(t, s)}, sessions: map[string]Session{"test": {Expires: time.Now().Add(time.Hour)}}}
	must(t, a.runtime.initStreams([]Camera{cam}, now))
	must(t, a.runtime.tickStreams(context.Background(), time.Now()))
	for _, tc := range []struct {
		path          string
		authenticated bool
		status        int
	}{
		{"/api/v1/availability", false, 401},
		{"/api/v1/availability", true, 200},
		{"/api/v1/availability?camera=" + cam.ID, true, 200},
		{"/api/v1/availability?camera=" + ID(), true, 404},
	} {
		r := httptest.NewRequest("GET", tc.path, nil)
		if tc.authenticated {
			r.AddCookie(&http.Cookie{Name: "dozor_session", Value: "test"})
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %s", tc.path, w.Code, w.Body.String())
		}
		if w.Code == 200 {
			var h AvailabilityHistory
			must(t, json.Unmarshal(w.Body.Bytes(), &h))
			if len(h.Cameras) != 1 || h.Cameras[0].Name != cam.Name {
				t.Fatal(h)
			}
		}
	}
	b, _ := json.Marshal(StreamSignal{CameraID: cam.ID, Online: true, At: time.Now().UnixMilli()})
	w := httptest.NewRecorder()
	a.internalHandler().ServeHTTP(w, httptest.NewRequest("POST", "/stream", strings.NewReader(string(b))))
	if w.Code != 204 || len(a.runtime.streamSignals) != 1 {
		t.Fatal(w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.Handler().ServeHTTP(w, httptest.NewRequest("POST", "/stream", strings.NewReader(string(b))))
	if w.Code == 204 {
		t.Fatal("stream hook publicly exposed")
	}
	a.runtime = nil
	w = httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/api/v1/availability", nil)
	r.AddCookie(&http.Cookie{Name: "dozor_session", Value: "test"})
	a.Handler().ServeHTTP(w, r)
	if w.Code != 503 {
		t.Fatal(w.Code)
	}
}
