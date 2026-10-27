package dozor

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCloudUploadPersistence(t *testing.T) {
	dir := t.TempDir()
	control, err := loadCloudUpload(dir)
	must(t, err)
	if !control.Get().Enabled {
		t.Fatal("uploads must start enabled")
	}
	paused, err := control.Save(context.Background(), false)
	must(t, err)
	same, err := control.Save(context.Background(), false)
	must(t, err)
	if paused != same {
		t.Fatal("repeated pause changed revision")
	}
	restored, err := loadCloudUpload(dir)
	must(t, err)
	if restored.Get() != paused {
		t.Fatal("restart lost pause")
	}
	resumed, err := restored.Save(context.Background(), true)
	must(t, err)
	if !resumed.Enabled || resumed.ChangedAt <= paused.ChangedAt {
		t.Fatal("resume did not advance revision")
	}
	must(t, os.Remove(restored.path))
	must(t, os.Mkdir(restored.path, 0700))
	if _, err = restored.Save(context.Background(), false); err == nil || restored.Get() != resumed {
		t.Fatal("failed write changed live state")
	}
}

func TestCloudUploadInvalidStateFailsClosed(t *testing.T) {
	for _, value := range []string{`{}`, `null`, `{"enabled":null,"changed_at":0}`, `{"enabled":true}`, `{"enabled":true,"changed_at":-1}`, `broken`} {
		dir := t.TempDir()
		must(t, os.WriteFile(filepath.Join(dir, "cloud-upload.json"), []byte(value), 0600))
		if _, err := loadCloudUpload(dir); err == nil {
			t.Fatal("invalid state accepted", value)
		}
	}
}

type cancelledUpload struct{ started chan struct{} }

func (remote *cancelledUpload) Put(ctx context.Context, _, _, _, _ string, _ int64) error {
	close(remote.started)
	<-ctx.Done()
	return ctx.Err()
}

func TestCloudUploadPauseCancelsTransferAndPreservesQueue(t *testing.T) {
	control, err := loadCloudUpload(t.TempDir())
	must(t, err)
	s := testStore(t)
	ev := Event{ID: ID(), CameraID: ID(), Start: 1000, End: 9000, Status: "closed"}
	must(t, s.SaveEvent(ev))
	part := fixturePart(t, s, ev, 1000, 9000)
	remote := &cancelledUpload{started: make(chan struct{})}
	finished := make(chan error, 1)
	go func() { _, err := control.uploadOne(context.Background(), s, S3Config{}, remote); finished <- err }()
	select {
	case <-remote.started:
	case <-time.After(5 * time.Second):
		t.Fatal("upload did not start")
	}
	_, err = control.Save(context.Background(), false)
	must(t, err)
	if err := <-finished; err != context.Canceled {
		t.Fatal("transfer not cancelled", err)
	}
	queued, err := s.NextJob(time.Now())
	must(t, err)
	if queued.Ref != part.ID {
		t.Fatal("paused upload lost its queue entry")
	}
	stored, err := s.Part(part.ID)
	must(t, err)
	if stored.Uploaded {
		t.Fatal("cancelled upload acknowledged")
	}
	completed := &memoryRemote{}
	worked, err := control.uploadOne(context.Background(), s, S3Config{}, completed)
	must(t, err)
	if worked || len(completed.keys) != 0 {
		t.Fatal("paused worker sent a file")
	}
	_, err = control.Save(context.Background(), true)
	must(t, err)
	worked, err = control.uploadOne(context.Background(), s, S3Config{}, completed)
	must(t, err)
	if !worked || len(completed.keys) != 1 {
		t.Fatal("resume did not retry queued file")
	}
}

func TestCloudUploadAPI(t *testing.T) {
	dir := t.TempDir()
	config, err := LoadConfig(filepath.Join(dir, "config.json"))
	must(t, err)
	app, err := NewApp(config, Binaries{}, true, dir, filepath.Join(dir, "s.sock"), "test")
	must(t, err)
	app.sessions["test"] = Session{CSRF: "csrf", Expires: time.Now().Add(time.Hour)}
	for _, tc := range []struct {
		body, csrf string
		auth       bool
		code       int
	}{
		{`{"enabled":false}`, "csrf", true, 200}, {`{"enabled":true}`, "csrf", true, 200},
		{`{}`, "csrf", true, 400}, {`null`, "csrf", true, 400}, {`{"enabled":null}`, "csrf", true, 400},
		{`{"enabled":"false"}`, "csrf", true, 400}, {`{"enabled":false,"extra":true}`, "csrf", true, 400},
		{`{"enabled":false}`, "", true, 403}, {`{"enabled":false}`, "csrf", false, 401},
	} {
		before := app.uploads.Get()
		r := httptest.NewRequest("PUT", "/api/v1/cloud-upload", strings.NewReader(tc.body))
		if tc.auth {
			r.AddCookie(&http.Cookie{Name: "dozor_session", Value: "test"})
		}
		r.Header.Set("X-CSRF-Token", tc.csrf)
		w := httptest.NewRecorder()
		app.Handler().ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("%s: %d %s", tc.body, w.Code, w.Body.String())
		}
		if tc.code != 200 && app.uploads.Get() != before {
			t.Fatal("rejected write changed state")
		}
	}
	if app.Status()["cloud_upload"] == nil {
		t.Fatal("status omitted switch")
	}
	select {
	case <-app.reload:
		t.Fatal("upload switch restarted cameras")
	default:
	}
}
