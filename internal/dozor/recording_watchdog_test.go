package dozor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRecordingWatchdogRequiresUsableProgressAndIsolatesCamera(t *testing.T) {
	s := testStore(t)
	camera, other := Camera{ID: ID(), Name: "Test", URL: "rtsp://camera/main"}, ID()
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/v3/config/paths/patch/"+camera.ID {
			t.Error("restarted another camera", req.URL.Path)
		}
		user, password, ok := req.BasicAuth()
		if !ok || user != "dozor-recorder" || password != "fixture" {
			t.Error("missing control authentication")
		}
		var body map[string]string
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		requests = append(requests, body["source"])
	}))
	defer server.Close()
	r := &Runtime{Store: s, Engine: NewEngine(s, Binaries{}), bufferWake: make(chan struct{}, 1), mediaControl: &mediaControl{address: strings.TrimPrefix(server.URL, "http://"), password: "fixture", client: server.Client()}}
	now := time.Now().Truncate(time.Second)
	r.mediaSince.Store(now.Unix())
	r.workerReady.Store(true)
	watch := recordingWatch{}
	probeCalls := 0
	probe := func(_ context.Context, path string) (ProbeResult, error) {
		probeCalls++
		if strings.Contains(path, "broken") {
			return ProbeResult{}, errors.New("invalid video")
		}
		return ProbeResult{Video: "h264", Duration: 5}, nil
	}
	check := func(second int) {
		r.checkRecording(context.Background(), camera, &watch, now.Add(time.Duration(second)*time.Second), probe)
	}
	check(0)
	// Another camera's valid recordings and a damaged file cannot mask this outage.
	must(t, s.AddSegment(Segment{Path: "healthy-other.mp4", CameraID: other, Start: now.UnixMilli(), End: now.Add(time.Minute).UnixMilli()}))
	must(t, s.AddSegment(Segment{Path: "broken.mp4", CameraID: camera.ID, Start: now.UnixMilli(), End: now.Add(time.Hour).UnixMilli()}))
	check(29)
	if len(requests) != 0 {
		t.Fatal("restarted before grace period")
	}
	check(30)
	if len(requests) != 2 || requests[0] != "publisher" || requests[1] != camera.URL || !watch.failed {
		t.Fatal(requests, watch)
	}
	if len(r.bufferWake) != 1 {
		t.Fatal("closed-tail recovery was not requested")
	}
	must(t, s.AddSegment(Segment{Path: "closed-tail.mp4", CameraID: camera.ID, Start: now.UnixMilli(), End: now.Add(31 * time.Second).UnixMilli()}))
	check(35)
	if !watch.failed {
		t.Fatal("flushed tail was mistaken for resumed recording")
	}
	check(89)
	if len(requests) != 2 || probeCalls != 2 {
		t.Fatal("retry flooded camera or trusted repeated file", requests, probeCalls)
	}
	check(90)
	if len(requests) != 4 || watch.retryDelay != 2*time.Minute {
		t.Fatal("missing bounded retry", watch)
	}
	// A new valid file recovers even if the previous one had a future timestamp.
	must(t, s.AddSegment(Segment{Path: "good.mp4", CameraID: camera.ID, Start: now.Add(90 * time.Second).UnixMilli(), End: now.Add(95 * time.Second).UnixMilli()}))
	check(95)
	if watch.failed || watch.retryDelay != 0 {
		t.Fatal("valid video did not recover recording", watch)
	}
	check(100)
	if len(requests) != 4 {
		t.Fatal("healthy camera was restarted")
	}
	found := false
	for _, notice := range s.Notices() {
		found = found || strings.Contains(notice.Message, "восстановлена")
	}
	if !found {
		t.Fatal("recovery was not reported")
	}
}

func TestRecordingWatchdogRepairsPartialReconnect(t *testing.T) {
	s := testStore(t)
	camera := Camera{ID: ID(), Name: "Test", URL: "rtsp://camera/main", Username: "user", Password: "secret"}
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		calls++
		var body map[string]string
		_ = json.NewDecoder(req.Body).Decode(&body)
		if calls == 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("rtsp://user:secret@camera/main"))
		}
		if calls == 3 && body["source"] != CameraURL(camera, false) {
			t.Error("did not restore source directly")
		}
	}))
	defer server.Close()
	r := &Runtime{Store: s, mediaControl: &mediaControl{address: strings.TrimPrefix(server.URL, "http://"), password: "fixture", client: server.Client()}}
	now := time.Now().Truncate(time.Second)
	r.mediaSince.Store(now.Unix())
	r.workerReady.Store(true)
	watch := recordingWatch{}
	probe := func(context.Context, string) (ProbeResult, error) {
		return ProbeResult{Video: "h264", Duration: 5}, nil
	}
	r.checkRecording(context.Background(), camera, &watch, now, probe)
	r.checkRecording(context.Background(), camera, &watch, now.Add(30*time.Second), probe)
	if !watch.restore || calls != 2 {
		t.Fatal("failed restoration was forgotten", watch, calls)
	}
	must(t, s.AddSegment(Segment{Path: "late-tail.mp4", CameraID: camera.ID, Start: now.UnixMilli(), End: now.Add(5 * time.Second).UnixMilli()}))
	r.checkRecording(context.Background(), camera, &watch, now.Add(40*time.Second), probe)
	if watch.restore || calls != 3 || !watch.failed {
		t.Fatal("late tail masked unfinished reconnect", watch, calls)
	}
	for _, notice := range s.Notices() {
		if strings.Contains(notice.Message, "secret") || strings.Contains(notice.Message, "rtsp://") {
			t.Fatal("leaked camera credentials")
		}
	}
}

func TestRecordingWatchdogWaitsForRecorderAndDoesNotTrustOldFiles(t *testing.T) {
	s := testStore(t)
	camera := Camera{ID: ID()}
	now := time.Now().Truncate(time.Second)
	must(t, s.AddSegment(Segment{Path: "old.mp4", CameraID: camera.ID, Start: now.Add(-time.Minute).UnixMilli(), End: now.Add(-55 * time.Second).UnixMilli()}))
	r := &Runtime{Store: s}
	watch := recordingWatch{}
	probe := func(context.Context, string) (ProbeResult, error) {
		t.Fatal("old file was treated as fresh recording")
		return ProbeResult{}, nil
	}
	r.checkRecording(context.Background(), camera, &watch, now, probe)
	r.mediaSince.Store(now.Unix())
	r.checkRecording(context.Background(), camera, &watch, now.Add(time.Minute), probe)
	r.workerReady.Store(true)
	r.checkRecording(context.Background(), camera, &watch, now.Add(2*time.Minute), probe)
	if watch.failed || !watch.lastProgress.Equal(now.Add(2*time.Minute)) {
		t.Fatal(watch)
	}
}

func TestRecordingRetryBackoffIsBounded(t *testing.T) {
	w := recordingWatch{}
	now := time.Now()
	for i := range 10 {
		w.attempted(now)
		if w.retryDelay > recordingRetryMax || w.nextAttempt.Sub(now) != w.retryDelay {
			t.Fatal(w)
		}
		if i >= 3 && w.retryDelay != recordingRetryMax {
			t.Fatal(w)
		}
	}
}

func TestRecordingControlConfigIsPrivate(t *testing.T) {
	control, err := newMediaControl()
	must(t, err)
	defer control.client.CloseIdleConnections()
	data, err := mediaConfig(Config{Archive: t.TempDir()}, Binaries{Self: "/bin/dozor"}, filepath.Join(t.TempDir(), "control.sock"), control)
	must(t, err)
	var config map[string]any
	must(t, json.Unmarshal(data, &config))
	if config["api"] != true || !strings.HasPrefix(config["apiAddress"].(string), "127.0.0.1:") || len(config["apiAllowOrigins"].([]any)) != 0 {
		t.Fatal("control listener exposed")
	}
	users := config["authInternalUsers"].([]any)
	for i, user := range users {
		u := user.(map[string]any)
		for _, item := range u["permissions"].([]any) {
			action := item.(map[string]any)["action"]
			if action == "publish" || i == 0 && action == "api" {
				t.Fatal("anonymous privilege escalation")
			}
		}
	}
	if users[1].(map[string]any)["pass"] != control.password || len(control.password) < 32 {
		t.Fatal("missing per-runtime credential")
	}
}
