package dozor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func emptyVideoBinaries(t *testing.T) Binaries {
	t.Helper()
	dir := t.TempDir()
	ffmpeg := filepath.Join(dir, "ffmpeg")
	probe := filepath.Join(dir, "ffprobe")
	must(t, os.WriteFile(ffmpeg, []byte("#!/bin/sh\nexit 0\n"), 0700))
	must(t, os.WriteFile(probe, []byte("#!/bin/sh\nprintf '%s' '{\"streams\":[],\"format\":{}}'\n"), 0700))
	return Binaries{MediaMTX: ffmpeg, FFmpeg: ffmpeg, FFprobe: probe, Self: ffmpeg}
}

func pendingRecording(t *testing.T, s *Store, camera string, start time.Time) (Event, Segment) {
	t.Helper()
	path := filepath.Join(s.Root, "buffer", camera, start.UTC().Format("2006-01-02_15-04-05.000000")+".mp4")
	must(t, AtomicWrite(path, []byte("unfinished video"), 0600))
	seg, err := ParseSegment(s.Root, path, 5)
	must(t, err)
	must(t, s.AddSegment(seg))
	ev := Event{ID: ID(), CameraID: camera, Start: seg.Start, Cursor: seg.Start, End: seg.End, Status: "closing"}
	must(t, s.SaveEvent(ev))
	return ev, seg
}

func TestVideoRecoveryFailureDoesNotBlockOtherEvents(t *testing.T) {
	s := testStore(t)
	bins := emptyVideoBinaries(t)
	engine := NewEngine(s, bins)
	now := time.Now().Truncate(time.Second)
	failed, seg := pendingRecording(t, s, ID(), now.Add(-2*time.Minute))
	// Both the same camera and other cameras must keep making progress.
	sameCamera, _ := pendingRecording(t, s, failed.CameraID, now.Add(-time.Minute))
	otherCamera, _ := pendingRecording(t, s, ID(), now.Add(-30*time.Second))
	recovered := false
	engine.assemble = func(ctx context.Context, s *Store, b Binaries, ev Event, segs []Segment) (Part, error) {
		if ev.ID == failed.ID && !recovered {
			return Assemble(ctx, s, b, ev, segs)
		}
		return fixturePart(t, s, ev, segs[0].Start, segs[len(segs)-1].End), nil
	}
	if err := engine.Tick(context.Background(), now); err == nil || !strings.Contains(err.Error(), "в потоке нет видео") {
		t.Fatalf("expected the empty-video error, got %v", err)
	}
	for _, ev := range []Event{sameCamera, otherCamera} {
		actual, err := s.Event(ev.ID)
		must(t, err)
		parts, err := s.Parts(ev.ID)
		must(t, err)
		if actual.Status != "closed" || len(parts) != 1 {
			t.Errorf("failed recording blocked another event: %+v, parts=%d", actual, len(parts))
		}
	}
	actual, err := s.Event(failed.ID)
	must(t, err)
	if actual.Cursor != failed.Cursor || actual.Status != "closing" || !s.HasSegment(seg.Path) {
		t.Fatal("failed recording must retain its cursor and pinned buffer for retry", actual)
	}
	_, err = os.Stat(filepath.Join(s.Root, seg.Path))
	must(t, err)
	recovered = true
	must(t, engine.Tick(context.Background(), now.Add(time.Second)))
	actual, err = s.Event(failed.ID)
	must(t, err)
	parts, err := s.Parts(failed.ID)
	must(t, err)
	if actual.Status != "closed" || actual.Cursor != seg.End || len(parts) != 1 {
		t.Fatal("failed recording was not retried", actual, parts)
	}
}

func TestSensitivityReloadStartsDespiteUnfinishedVideo(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(filepath.Join(dir, "config.json"))
	must(t, err)
	c := cfg.Get()
	c.Archive = filepath.Join(dir, "archive")
	c.DiskUUID = "12345678-1234-1234-1234-123456789abc"
	must(t, os.MkdirAll(c.Archive, 0700))
	for range 4 {
		c.Cameras = append(c.Cameras, Camera{ID: ID(), Name: "Camera", URL: "rtsp://camera/main", SubURL: "rtsp://camera/sub", Motion: "local", Sensitivity: .7, Enabled: true})
	}
	must(t, cfg.Save(c))
	app, err := NewApp(cfg, emptyVideoBinaries(t), true, filepath.Join(dir, "state"), filepath.Join(dir, "control.sock"), "test")
	must(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer app.stop()
	must(t, app.start(ctx))
	ev, seg := pendingRecording(t, app.runtime.Store, c.Cameras[0].ID, time.Now().Add(-time.Minute))
	for _, camera := range c.Cameras {
		camera.Sensitivity = .95
		body, err := json.Marshal(camera)
		must(t, err)
		req := httptest.NewRequest("PUT", "/api/v1/cameras/"+camera.ID, bytes.NewReader(body))
		req.SetPathValue("id", camera.ID)
		w := httptest.NewRecorder()
		app.saveCamera(w, req)
		if w.Code != 200 {
			t.Fatalf("saving sensitivity: %d %s", w.Code, w.Body.String())
		}
		select {
		case <-app.reload:
		default:
			t.Fatal("camera settings did not request a reload")
		}
		app.stop()
		must(t, app.start(ctx))
	}
	status := app.Status()
	if status["disk_ready"] != true || status["storage_error"] != "" || len(status["cameras"].([]any)) != 4 {
		t.Fatalf("video recovery was reported as a disk failure: %+v", status)
	}
	if cfg.Get().DiskUUID != c.DiskUUID || cfg.Get().Archive != c.Archive {
		t.Fatal("camera changes altered the selected disk")
	}
	for _, camera := range cfg.Get().Cameras {
		if camera.Sensitivity != .95 {
			t.Fatal("sensitivity was not saved", camera.Sensitivity)
		}
	}
	actual, err := app.runtime.Store.Event(ev.ID)
	must(t, err)
	if actual.Cursor != ev.Cursor || !app.runtime.Store.HasSegment(seg.Path) {
		t.Fatal("startup discarded the unfinished recording", actual)
	}
	reported := false
	for _, notice := range app.runtime.Store.Notices() {
		if strings.Contains(notice.Message, "Не удалось восстановить часть записей") {
			reported = true
		}
	}
	if !reported {
		t.Fatal("recovery failure was not reported")
	}
}

func TestStartupStillRejectsStorageFailures(t *testing.T) {
	for _, failure := range []string{"missing disk", "unwritable catalog"} {
		t.Run(failure, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "archive")
			if failure == "unwritable catalog" {
				must(t, os.MkdirAll(filepath.Join(root, "catalog.sqlite"), 0700))
			}
			app := &App{Config: &ConfigFile{value: Config{Archive: root}}, Development: true}
			defer app.stop()
			err := app.start(context.Background())
			if err == nil || errors.Is(err, errVideoAssembly) || app.runtime != nil {
				t.Fatalf("storage failure was ignored: %v", err)
			}
		})
	}
}
