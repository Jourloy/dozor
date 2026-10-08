package dozor

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestStoragePolicyPersistenceAndFailedSave(t *testing.T) {
	dir := t.TempDir()
	f, err := loadStoragePolicy(dir)
	must(t, err)
	if f.Get().MaxDiskUsagePercent != 80 {
		t.Fatal("missing default")
	}
	must(t, f.Save(StoragePolicy{65}))
	restored, err := loadStoragePolicy(dir)
	must(t, err)
	if restored.Get().MaxDiskUsagePercent != 65 {
		t.Fatal("restart lost policy")
	}
	for _, value := range []int{-1, 0, 100, 1000} {
		if err := f.Save(StoragePolicy{value}); err == nil {
			t.Fatal("invalid policy accepted", value)
		}
	}
	must(t, os.Remove(f.path))
	must(t, os.Mkdir(f.path, 0700))
	if f.Save(StoragePolicy{70}) == nil || f.Get().MaxDiskUsagePercent != 65 {
		t.Fatal("failed save changed effective policy")
	}
}

func TestStoragePolicyRejectsInvalidSavedData(t *testing.T) {
	for _, value := range []string{`{}`, `null`, `{"max_disk_usage_percent":null}`, `{"max_disk_usage_percent":0}`, `{"max_disk_usage_percent":80.5}`, `broken`} {
		dir := t.TempDir()
		must(t, os.WriteFile(filepath.Join(dir, "storage-policy.json"), []byte(value), 0600))
		if _, err := loadStoragePolicy(dir); err == nil {
			t.Fatal("invalid persisted policy accepted", value)
		}
	}
}

func TestStoragePolicyConcurrentReaders(t *testing.T) {
	f, err := loadStoragePolicy(t.TempDir())
	must(t, err)
	var wg sync.WaitGroup
	for i := 1; i <= 5; i++ {
		wg.Add(1)
		go func(value int) {
			defer wg.Done()
			if err := f.Save(StoragePolicy{value}); err != nil {
				t.Error(err)
			}
			if err := f.Get().validate(); err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
}

func TestStoragePolicyAPI(t *testing.T) {
	dir := t.TempDir()
	c, err := LoadConfig(filepath.Join(dir, "config.json"))
	must(t, err)
	a, err := NewApp(c, Binaries{}, true, dir, filepath.Join(dir, "s.sock"), "test")
	must(t, err)
	a.sessions["test"] = Session{CSRF: "csrf", Expires: time.Now().Add(time.Hour)}
	for _, tc := range []struct {
		body, csrf, origin string
		auth               bool
		want               int
	}{
		{`{"max_disk_usage_percent":1}`, "csrf", "", true, 200},
		{`{"max_disk_usage_percent":99}`, "csrf", "", true, 200},
		{`{"max_disk_usage_percent":65}`, "csrf", "", true, 200},
		{`{"max_disk_usage_percent":0}`, "csrf", "", true, 400},
		{`{"max_disk_usage_percent":100}`, "csrf", "", true, 400},
		{`{"max_disk_usage_percent":-1}`, "csrf", "", true, 400},
		{`{"max_disk_usage_percent":80.5}`, "csrf", "", true, 400},
		{`{"max_disk_usage_percent":null}`, "csrf", "", true, 400},
		{`{"max_disk_usage_percent":"80"}`, "csrf", "", true, 400},
		{`{"max_disk_usage_percent":80,"extra":true}`, "csrf", "", true, 400},
		{`{}`, "csrf", "", true, 400},
		{`null`, "csrf", "", true, 400},
		{`{}`, "csrf", "", false, 401},
		{`{}`, "", "", true, 403},
		{`{}`, "csrf", "https://other.example", true, 403},
	} {
		before := a.storage.Get()
		r := httptest.NewRequest("PUT", "/api/v1/storage-policy", strings.NewReader(tc.body))
		if tc.auth {
			r.AddCookie(&http.Cookie{Name: "dozor_session", Value: "test"})
		}
		r.Header.Set("X-CSRF-Token", tc.csrf)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s: %d %s", tc.body, w.Code, w.Body.String())
		}
		if tc.want != 200 && a.storage.Get() != before {
			t.Fatal("rejected request changed policy")
		}
	}
	for _, auth := range []bool{false, true} {
		r := httptest.NewRequest("GET", "/api/v1/storage-policy", nil)
		if auth {
			r.AddCookie(&http.Cookie{Name: "dozor_session", Value: "test"})
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if auth {
			var policy StoragePolicy
			must(t, json.Unmarshal(w.Body.Bytes(), &policy))
			if w.Code != 200 || policy.MaxDiskUsagePercent != 65 {
				t.Fatal(w.Body.String())
			}
		} else if w.Code != 401 {
			t.Fatal("unprotected read")
		}
	}
	select {
	case <-a.reload:
		t.Fatal("policy change restarted video")
	default:
	}
	must(t, os.Remove(a.storage.path))
	must(t, os.Mkdir(a.storage.path, 0700))
	r := httptest.NewRequest("PUT", "/api/v1/storage-policy", strings.NewReader(`{"max_disk_usage_percent":60}`))
	r.AddCookie(&http.Cookie{Name: "dozor_session", Value: "test"})
	r.Header.Set("X-CSRF-Token", "csrf")
	w := httptest.NewRecorder()
	a.Handler().ServeHTTP(w, r)
	if w.Code != 500 || a.storage.Get().MaxDiskUsagePercent != 65 {
		t.Fatal("false save confirmation")
	}
}

func pruneOne(t *testing.T, s *Store, now time.Time) {
	t.Helper()
	calls := 0
	must(t, s.PruneArchive(now, StoragePolicy{80}, func() (uint64, uint64, error) {
		calls++
		if calls == 1 {
			return 80, 100, nil
		}
		return 79, 100, nil
	}))
	if calls != 2 {
		t.Fatalf("expected one deletion, usage reads=%d", calls)
	}
}

func pruneEvent(t *testing.T, s *Store, start, disconnected int64) Event {
	t.Helper()
	ev := Event{ID: ID(), CameraID: ID(), Start: start, End: start + 10000, Status: "closed", DisconnectedAt: disconnected}
	must(t, s.SaveEvent(ev))
	return ev
}

func assertDeleted(t *testing.T, s *Store, p Part, want bool) {
	t.Helper()
	actual, err := s.Part(p.ID)
	must(t, err)
	if actual.Deleted != want {
		t.Fatalf("part %s: deleted=%v want=%v", p.ID, actual.Deleted, want)
	}
	_, err = os.Stat(filepath.Join(s.Root, p.Path))
	if want != errors.Is(err, os.ErrNotExist) {
		t.Fatalf("file and catalog disagree: %v", err)
	}
}

func TestArchivePruneThresholdsAndChangedPolicy(t *testing.T) {
	for _, tc := range []struct {
		used, total uint64
		percent     int
		want        bool
	}{
		{79, 100, 80, false}, {80, 100, 80, true}, {81, 100, 80, true},
		{50, 100, 80, false}, {50, 100, 50, true}, {266, 333, 80, false}, {267, 333, 80, true},
		{0, 100, 1, false}, {1, 100, 1, true}, {98, 100, 99, false}, {99, 100, 99, true},
		{^uint64(0), ^uint64(0), 99, true}, {0, ^uint64(0), 1, false},
	} {
		s := testStore(t)
		ev := pruneEvent(t, s, 1000, 0)
		p := fixturePart(t, s, ev, 1000, 2000)
		calls := 0
		must(t, s.PruneArchive(time.Now(), StoragePolicy{tc.percent}, func() (uint64, uint64, error) {
			calls++
			if calls == 1 {
				return tc.used, tc.total, nil
			}
			return 0, tc.total, nil
		}))
		assertDeleted(t, s, p, tc.want)
	}
}

func TestArchivePruneProtectsWholeEventAndFallsBackOldest(t *testing.T) {
	s := testStore(t)
	now := time.Now()
	protected := pruneEvent(t, s, 1000, now.Add(-time.Hour).UnixMilli())
	first := fixturePart(t, s, protected, 1000, 2000)
	last := fixturePart(t, s, protected, 2000, 3000)
	last.DisconnectedAt = protected.DisconnectedAt
	must(t, s.SavePart(last))
	expired := pruneEvent(t, s, 3000, now.Add(-disconnectPriorityDuration).UnixMilli())
	old := fixturePart(t, s, expired, 3000, 4000)
	normal := pruneEvent(t, s, 4000, 0)
	recent := fixturePart(t, s, normal, 4000, 5000)
	recent.Uploaded = true
	must(t, s.SavePart(recent))
	for i, p := range []Part{old, recent, first, last} {
		pruneOne(t, s, now)
		assertDeleted(t, s, p, true)
		if i < 2 {
			assertDeleted(t, s, first, false)
			assertDeleted(t, s, last, false)
		}
		if i == 2 {
			assertDeleted(t, s, last, false)
		}
	}
	found := false
	for _, notice := range s.Notices() {
		if strings.Contains(notice.Message, "младше 90 дней") {
			found = true
		}
	}
	if !found {
		t.Fatal("priority deletion was not reported")
	}
	if err := s.PruneArchive(now, StoragePolicy{80}, func() (uint64, uint64, error) { return 90, 100, nil }); !errors.Is(err, errArchiveLimit) {
		t.Fatal("exhausted archive not reported", err)
	}
}

func TestArchivePruneNinetyDayBoundaryAndNoAgeOnlyDeletion(t *testing.T) {
	now := time.Now()
	for _, offset := range []time.Duration{-time.Millisecond, 0, time.Millisecond} {
		s := testStore(t)
		ev := pruneEvent(t, s, 1000, now.Add(-disconnectPriorityDuration+offset).UnixMilli())
		old := fixturePart(t, s, ev, 1000, 2000)
		normal := pruneEvent(t, s, 3000, 0)
		young := fixturePart(t, s, normal, 3000, 4000)
		must(t, s.PruneArchive(now, StoragePolicy{80}, func() (uint64, uint64, error) { return 10, 100, nil }))
		assertDeleted(t, s, old, false)
		pruneOne(t, s, now)
		assertDeleted(t, s, old, offset <= 0)
		assertDeleted(t, s, young, offset > 0)
	}
}

func TestArchivePruneErrorsAndUnrelatedFiles(t *testing.T) {
	s := testStore(t)
	ev := pruneEvent(t, s, 1000, 0)
	p := fixturePart(t, s, ev, 1000, 2000)
	other := fixturePart(t, s, ev, 2000, 3000)
	for _, rel := range []string{"other.txt", "buffer/current.mp4", filepath.Join(eventDir(ev), "in-progress.mp4.partial")} {
		must(t, AtomicWrite(filepath.Join(s.Root, rel), []byte("keep"), 0600))
	}
	for _, measurement := range []func() (uint64, uint64, error){
		func() (uint64, uint64, error) { return 0, 0, errors.New("statfs failed") },
		func() (uint64, uint64, error) { return 0, 0, nil },
	} {
		if s.PruneArchive(time.Now(), StoragePolicy{80}, measurement) == nil {
			t.Fatal("ignored measurement error")
		}
		assertDeleted(t, s, p, false)
	}
	calls := 0
	err := s.PruneArchive(time.Now(), StoragePolicy{80}, func() (uint64, uint64, error) {
		calls++
		if calls == 1 {
			return 80, 100, nil
		}
		return 0, 0, errors.New("disk disappeared")
	})
	if err == nil {
		t.Fatal("ignored later statfs error")
	}
	assertDeleted(t, s, p, true)
	assertDeleted(t, s, other, false)
	for _, rel := range []string{"other.txt", "buffer/current.mp4", filepath.Join(eventDir(ev), "in-progress.mp4.partial")} {
		if _, err := os.Stat(filepath.Join(s.Root, rel)); err != nil {
			t.Fatal("unrelated file removed", err)
		}
	}
	s.Guard.Root = filepath.Join(s.Root, "missing")
	if s.PruneArchive(time.Now(), StoragePolicy{80}, func() (uint64, uint64, error) { t.Fatal("measured missing disk"); return 80, 100, nil }) == nil {
		t.Fatal("ignored guard failure")
	}
}

func TestArchivePruneRetriesUnlinkAndRecoversAfterRestart(t *testing.T) {
	for _, restart := range []bool{false, true} {
		s := testStore(t)
		ev := pruneEvent(t, s, 1000, 0)
		p := fixturePart(t, s, ev, 1000, 2000)
		full := filepath.Join(s.Root, p.Path)
		must(t, os.Remove(full))
		must(t, os.Mkdir(full, 0700))
		must(t, os.WriteFile(filepath.Join(full, "block"), []byte("fixture"), 0600))
		if s.PruneArchive(time.Now(), StoragePolicy{80}, func() (uint64, uint64, error) { return 80, 100, nil }) == nil {
			t.Fatal("ignored unlink failure")
		}
		var pending string
		must(t, s.DB.QueryRow("SELECT value FROM metadata WHERE key='archive_prune_pending'").Scan(&pending))
		must(t, os.Remove(filepath.Join(full, "block")))
		must(t, os.Remove(full))
		must(t, AtomicWrite(full, []byte("fixture-video"), 0600))
		if restart {
			must(t, s.Close())
			var err error
			s, err = OpenStore(s.Guard)
			must(t, err)
			defer s.Close()
		}
		must(t, s.PruneArchive(time.Now(), StoragePolicy{99}, func() (uint64, uint64, error) { return 10, 100, nil }))
		assertDeleted(t, s, p, true)
		actual, err := s.Event(ev.ID)
		must(t, err)
		if !actual.Lost {
			t.Fatal("lost upload not reflected in event")
		}
		var n int
		must(t, s.DB.QueryRow("SELECT COUNT(*) FROM jobs WHERE id=?", "part:"+p.ID).Scan(&n))
		if n != 0 {
			t.Fatal("deleted part remains queued")
		}
		must(t, s.DB.QueryRow("SELECT COUNT(*) FROM metadata WHERE key='archive_prune_pending'").Scan(&n))
		if n != 0 {
			t.Fatal("completed deletion remains pending")
		}
	}
}
