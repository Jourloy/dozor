package dozor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, e := OpenStore(Guard{Root: t.TempDir(), Development: true})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func must(t *testing.T, e error) {
	t.Helper()
	if e != nil {
		t.Fatal(e)
	}
}
func fixturePart(t *testing.T, s *Store, ev Event, start, end int64) Part {
	t.Helper()
	p := Part{ID: ID(), EventID: ev.ID, CameraID: ev.CameraID, Start: start, End: end}
	p.Path = filepath.Join(eventDir(ev), p.ID+".mp4")
	must(t, AtomicWrite(filepath.Join(s.Root, p.Path), []byte("fixture-video"), 0600))
	var e error
	p.SHA256, p.MD5, p.Size, e = HashFile(filepath.Join(s.Root, p.Path))
	must(t, e)
	must(t, s.SavePart(p))
	must(t, s.Enqueue("part", p.ID))
	return p
}
func TestEventWindowAndExtension(t *testing.T) {
	s := testStore(t)
	engine := NewEngine(s, Binaries{})
	engine.assemble = func(_ context.Context, s *Store, _ Binaries, ev Event, segs []Segment) (Part, error) {
		return fixturePart(t, s, ev, segs[0].Start, segs[len(segs)-1].End), nil
	}
	base := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)
	cam := ID()
	for i := -13; i < 8; i++ {
		must(t, s.AddSegment(Segment{Path: filepath.Join("buffer", cam, time.UnixMilli(base.Add(time.Duration(i)*5*time.Second).UnixMilli()).Format("150405")+".mp4"), CameraID: cam, Start: base.Add(time.Duration(i) * 5 * time.Second).UnixMilli(), End: base.Add(time.Duration(i+1) * 5 * time.Second).UnixMilli()}))
	}
	engine.Signal(MotionSignal{cam, true, true, "local", base})
	must(t, engine.Tick(context.Background(), base))
	evs, e := s.Events("", 10)
	must(t, e)
	if len(evs) != 1 || evs[0].Start != base.Add(-time.Minute).UnixMilli() {
		t.Fatalf("pre-roll: %+v", evs)
	}
	engine.Signal(MotionSignal{cam, false, true, "local", base.Add(time.Second)})
	must(t, engine.Tick(context.Background(), base.Add(8*time.Second)))
	engine.Signal(MotionSignal{cam, true, true, "local", base.Add(9 * time.Second)})
	must(t, engine.Tick(context.Background(), base.Add(9*time.Second)))
	engine.Signal(MotionSignal{cam, false, true, "local", base.Add(10 * time.Second)})
	must(t, engine.Tick(context.Background(), base.Add(32*time.Second)))
	evs, e = s.Events("", 10)
	must(t, e)
	if len(evs) != 1 || evs[0].Status != "closed" || evs[0].End != base.Add(19*time.Second).UnixMilli() {
		t.Fatalf("extension: %+v", evs)
	}
	parts, e := s.Parts(evs[0].ID)
	must(t, e)
	if len(parts) != 2 || parts[0].Start != base.Add(-60*time.Second).UnixMilli() || parts[1].End < evs[0].End {
		t.Fatalf("parts: %+v", parts)
	}
}
func TestDetectorFailureAndRecovery(t *testing.T) {
	s := testStore(t)
	e := NewEngine(s, Binaries{})
	cam := ID()
	now := time.Now()
	e.Signal(MotionSignal{CameraID: cam, Healthy: false, At: now})
	must(t, e.Tick(context.Background(), now))
	events, err := s.Events("", 10)
	must(t, err)
	if len(events) != 1 || events[0].Source != "detector_failure" {
		t.Fatal(events)
	}
	e.Signal(MotionSignal{CameraID: cam, Healthy: true, At: now})
	must(t, e.Tick(context.Background(), now.Add(23*time.Second)))
	v, err := s.Event(events[0].ID)
	must(t, err)
	if v.Status != "closed" || !v.Incomplete {
		t.Fatal(v)
	}
}

func TestMotionAfterPostrollStartsSeparateEvent(t *testing.T) {
	s := testStore(t)
	e := NewEngine(s, Binaries{})
	cam := ID()
	base := time.Now()
	e.Signal(MotionSignal{CameraID: cam, Active: true, Healthy: true, Source: "local", At: base})
	must(t, e.Tick(context.Background(), base))
	e.Signal(MotionSignal{CameraID: cam, Healthy: true, Source: "local", At: base.Add(time.Second)})
	must(t, e.Tick(context.Background(), base.Add(5*time.Second)))
	e.Signal(MotionSignal{CameraID: cam, Active: true, Healthy: true, Source: "local", At: base.Add(11 * time.Second)})
	must(t, e.Tick(context.Background(), base.Add(11*time.Second)))
	events, err := s.Events(cam, 10)
	must(t, err)
	if len(events) != 2 || events[1].End != base.Add(10*time.Second).UnixMilli() {
		t.Fatalf("separate motion bursts were merged: %+v", events)
	}
	// A delayed worker tick during uninterrupted motion must keep the same event.
	e.Signal(MotionSignal{CameraID: cam, Active: true, Healthy: true, Source: "local", At: base.Add(25 * time.Second)})
	must(t, e.Tick(context.Background(), base.Add(25*time.Second)))
	events, err = s.Events(cam, 10)
	must(t, err)
	if len(events) != 2 {
		t.Fatalf("continuous motion was split: %+v", events)
	}
}
func TestMotionMasksAndWarmup(t *testing.T) {
	d := NewDetector(20, 20, 1, []Rect{{0, 0, .5, 1}})
	frame := make([]byte, 400)
	for i := 0; i < 20; i++ {
		if d.Frame(frame) {
			t.Fatal("static warmup")
		}
	}
	for y := 0; y < 20; y++ {
		for x := 0; x < 10; x++ {
			frame[y*20+x] = 255
		}
	}
	for i := 0; i < 3; i++ {
		if d.Frame(frame) {
			t.Fatal("mask ignored")
		}
	}
	for y := 0; y < 20; y++ {
		for x := 10; x < 20; x++ {
			frame[y*20+x] = 200
		}
	}
	d.Frame(frame)
	if !d.Frame(frame) {
		t.Fatal("movement not detected")
	}
}
func TestRetentionPrioritizesAgeAndReportsLoss(t *testing.T) {
	s := testStore(t)
	now := time.Now().UnixMilli()
	ev := Event{ID: ID(), CameraID: ID(), Start: now, End: now + 70000, Status: "closed"}
	must(t, s.SaveEvent(ev))
	first := fixturePart(t, s, ev, now, now+10000)
	second := fixturePart(t, s, ev, now+10000, now+20000)
	second.Uploaded = true
	must(t, s.SavePart(second))
	calls := 0
	must(t, s.PruneArchive(time.Now(), StoragePolicy{80}, func() (uint64, uint64, error) {
		calls++
		if calls == 1 {
			return 95, 100, nil
		}
		return 79, 100, nil
	}))
	p, _ := s.Part(second.ID)
	old, _ := s.Part(first.ID)
	if p.Deleted || !old.Deleted {
		t.Fatal("did not prefer oldest part")
	}
	calls = 0
	must(t, s.PruneArchive(time.Now(), StoragePolicy{80}, func() (uint64, uint64, error) {
		calls++
		if calls == 1 {
			return 95, 100, nil
		}
		return 79, 100, nil
	}))
	old, _ = s.Part(first.ID)
	if !old.Lost || !old.Deleted || len(s.Notices()) == 0 {
		t.Fatal("lost upload not reported")
	}
}
func TestCrashRecoveryAndManifest(t *testing.T) {
	root := t.TempDir()
	g := Guard{Root: root, Development: true}
	s, e := OpenStore(g)
	must(t, e)
	ev := Event{ID: ID(), CameraID: ID(), Start: 100000, End: 200000, Cursor: 100000, Status: "open", AssemblyFailures: 2}
	must(t, s.SaveEvent(ev))
	p := fixturePart(t, s, ev, 100000, 160000)
	must(t, WriteJSON(filepath.Join(root, eventDir(ev), "manifest.json"), map[string]string{"event": "ignored for catalog recovery"}))
	must(t, s.Close())
	s, e = OpenStore(g)
	must(t, e)
	defer s.Close()
	must(t, s.ReplayOperations(context.Background()))
	must(t, s.RecoverPending(context.Background()))
	actual, e := s.Event(ev.ID)
	must(t, e)
	if actual.Cursor != p.End || actual.Status != "closing" || !actual.Incomplete || actual.AssemblyFailures != 0 || s.QueueCount() != 1 {
		t.Fatalf("%+v queue=%d", actual, s.QueueCount())
	}
}
func TestGuardNeverUsesUnverifiedDirectory(t *testing.T) {
	g := Guard{Root: t.TempDir(), UUID: ""}
	if g.Check() == nil {
		t.Fatal("unguarded root accepted")
	}
	g.Development = true
	must(t, g.Check())
}

func TestRecoveryReconcilesExistingCatalogAndUnfinishedFiles(t *testing.T) {
	s := testStore(t)
	ev := Event{ID: ID(), CameraID: ID(), Start: 100000, End: 300000, Status: "closed"}
	must(t, s.SaveEvent(ev))
	missing := fixturePart(t, s, ev, 100000, 160000)
	pending := fixturePart(t, s, ev, 160000, 220000)
	deleted := fixturePart(t, s, ev, 220000, 280000)
	must(t, os.Remove(filepath.Join(s.Root, missing.Path)))
	must(t, os.Rename(filepath.Join(s.Root, pending.Path), filepath.Join(s.Root, pending.Path+".partial")))
	deleted.Deleted = true // Simulate power loss between the tombstone and unlink.
	deleted.Lost = true
	must(t, s.SavePart(deleted))
	orphan := filepath.Join(s.Root, eventDir(ev), "orphan.mp4.partial")
	must(t, os.WriteFile(orphan, []byte("unfinished"), 0600))
	must(t, s.Recover())
	v, err := s.Part(missing.ID)
	must(t, err)
	if !v.Deleted || !v.Lost {
		t.Fatalf("existing row was not reconciled: %+v", v)
	}
	if _, err = os.Stat(filepath.Join(s.Root, pending.Path)); err != nil {
		t.Fatal("complete part not recovered", err)
	}
	for _, path := range []string{orphan, filepath.Join(s.Root, deleted.Path)} {
		if _, err = os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("unfinished/deleted file remained: %s", path)
		}
	}
	if s.QueueCount() != 2 { // One recovered video and its event manifest.
		t.Fatalf("stale upload jobs: %d", s.QueueCount())
	}
}
func TestSecretRedaction(t *testing.T) {
	c := Config{PasswordHash: "HASH", S3: S3Config{SecretKey: "SECRET", AccessKey: "ACCESS"}, Cameras: []Camera{{Password: "CAMERA"}}}
	b, _ := json.Marshal(PublicConfig(c))
	for _, secret := range []string{"HASH", "SECRET", "ACCESS", "CAMERA"} {
		if bytes.Contains(b, []byte(secret)) {
			t.Fatalf("leaked %s", secret)
		}
	}
}
func TestAuthCSRFAndSetupToken(t *testing.T) {
	dir := t.TempDir()
	cf, e := LoadConfig(filepath.Join(dir, "config.json"))
	must(t, e)
	a, e := NewApp(cf, Binaries{}, true, dir, filepath.Join(dir, "s.sock"), "test")
	must(t, e)
	h := a.Handler()
	request := func(method, path, body string, cookie *http.Cookie, token string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		if cookie != nil {
			r.AddCookie(cookie)
		}
		r.Header.Set("X-CSRF-Token", token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := request("GET", "/api/v1/settings", "", nil, ""); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request("POST", "/api/v1/login", `{"password":"valid-test-password","token":"wrong"}`, nil, ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
	w := request("POST", "/api/v1/login", `{"password":"valid-test-password","token":"`+a.setupToken+`"}`, nil, "")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatal(cookies)
	}
	var res map[string]string
	must(t, json.Unmarshal(w.Body.Bytes(), &res))
	if w = request("POST", "/api/v1/logout", `{}`, cookies[0], ""); w.Code != 403 {
		t.Fatal("CSRF accepted")
	}
	if w = request("POST", "/api/v1/logout", `{}`, cookies[0], res["csrf"]); w.Code != 204 {
		t.Fatal(w.Code)
	}
}

type memoryRemote struct {
	fail  bool
	keys  []string
	after func()
}

func (m *memoryRemote) Put(_ context.Context, k, _, _, _ string, _ int64) error {
	if m.fail {
		return io.ErrUnexpectedEOF
	}
	m.keys = append(m.keys, k)
	if m.after != nil {
		m.after()
	}
	return nil
}

func TestUploadAcknowledgedAfterLocalPruning(t *testing.T) {
	s := testStore(t)
	ev := Event{ID: ID(), CameraID: ID(), Start: 1000, End: 9000, Status: "open"}
	must(t, s.SaveEvent(ev))
	p := fixturePart(t, s, ev, 1000, 9000)
	remote := &memoryRemote{after: func() {
		calls := 0
		must(t, s.PruneArchive(time.Now(), StoragePolicy{80}, func() (uint64, uint64, error) {
			calls++
			if calls == 1 {
				return 95, 100, nil
			}
			return 79, 100, nil
		}))
	}}
	_, err := UploadOne(context.Background(), s, S3Config{}, remote)
	must(t, err)
	part, err := s.Part(p.ID)
	must(t, err)
	actual, err := s.Event(ev.ID)
	must(t, err)
	if !part.Deleted || !part.Uploaded || part.Lost || actual.Lost {
		t.Fatalf("successful in-flight upload stayed lost: %+v %+v", part, actual)
	}
}
func TestUploadRetryAndManifestCompletion(t *testing.T) {
	s := testStore(t)
	ev := Event{ID: ID(), CameraID: ID(), Start: 1000, End: 9000, Status: "closed"}
	must(t, s.SaveEvent(ev))
	p := fixturePart(t, s, ev, 1000, 9000)
	must(t, s.Enqueue("event", ev.ID))
	remote := &memoryRemote{fail: true}
	cfg := S3Config{Prefix: "test"}
	_, e := UploadOne(context.Background(), s, cfg, remote)
	if e == nil {
		t.Fatal("expected upload failure")
	}
	actual, _ := s.Part(p.ID)
	if actual.Uploaded {
		t.Fatal("premature ack")
	}
	_, e = s.DB.Exec("UPDATE jobs SET next=0")
	must(t, e)
	remote.fail = false
	_, e = UploadOne(context.Background(), s, cfg, remote)
	must(t, e)
	_, e = UploadOne(context.Background(), s, cfg, remote)
	must(t, e)
	actualEvent, e := s.Event(ev.ID)
	must(t, e)
	if !actualEvent.Uploaded || len(remote.keys) != 2 || s.QueueCount() != 0 {
		t.Fatalf("%+v %v", actualEvent, remote.keys)
	}
}
func TestS3WireChecksumsAndIdempotence(t *testing.T) {
	data := map[string][]byte{}
	metas := map[string]string{}
	puts := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "" {
			t.Error("missing SigV4")
		}
		switch r.Method {
		case "HEAD":
			b, ok := data[r.URL.Path]
			if !ok {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Length", itoa(len(b)))
			w.Header().Set("X-Amz-Meta-Sha256", metas[r.URL.Path])
		case "PUT":
			b, _ := io.ReadAll(r.Body)
			if r.Header.Get("Content-MD5") == "" {
				t.Error("no wire checksum")
			}
			data[r.URL.Path] = b
			metas[r.URL.Path] = r.Header.Get("X-Amz-Meta-Sha256")
			puts++
		default:
			t.Error(r.Method)
		}
	}))
	defer server.Close()
	file := filepath.Join(t.TempDir(), "test.mp4")
	must(t, os.WriteFile(file, []byte("test video bytes"), 0600))
	sha, md, size, e := HashFile(file)
	must(t, e)
	client := NewS3(S3Config{Endpoint: server.URL, Region: "us-east-1", Bucket: "archive", AccessKey: "test", SecretKey: "test", PathStyle: true})
	for i := 0; i < 2; i++ {
		must(t, client.Put(context.Background(), "recording.mp4", file, sha, md, size))
	}
	if puts != 1 || string(data["/archive/recording.mp4"]) != "test video bytes" {
		t.Fatal(puts, data)
	}
}
func itoa(i int) string { b, _ := json.Marshal(i); return string(b) }
