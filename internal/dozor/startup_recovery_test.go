package dozor

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func finishRecovery(t *testing.T, s *Store) {
	t.Helper()
	for i := 0; i < 10000; i++ {
		worked, err := s.RecoveryStep(context.Background())
		must(t, err)
		if !worked {
			must(t, s.CompleteRecovery())
			return
		}
	}
	t.Fatal("recovery did not finish")
}
func TestStartupDoesNotVisitCompletedArchive(t *testing.T) {
	for _, count := range []int{1000, 100000} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			s := testStore(t)
			finishRecovery(t, s)
			tx, err := s.DB.Begin()
			must(t, err)
			stmt, err := tx.Prepare("INSERT INTO events VALUES(?,?,?,?,?,?,?)")
			must(t, err)
			for i := 0; i < count; i++ {
				ev := Event{ID: fmt.Sprintf("%032x", i), CameraID: fmt.Sprintf("%032x", count), Start: int64(i), End: int64(i + 1), Cursor: int64(i + 1), Status: "closed", Uploaded: true}
				data, _ := json.Marshal(ev)
				_, err = stmt.Exec(ev.ID, ev.CameraID, ev.Start, ev.End, ev.Cursor, ev.Status, string(data))
				must(t, err)
			}
			must(t, stmt.Close())
			must(t, tx.Commit())
			must(t, s.CompleteRecovery())
			_, err = s.DB.Exec(`CREATE TABLE changed_events(id TEXT); CREATE TRIGGER monitor_history AFTER UPDATE ON events BEGIN INSERT INTO changed_events VALUES(new.id); END;`)
			must(t, err)
			// A startup recovery would encounter this invalid historical manifest.
			history := filepath.Join(s.Root, "events", "historical", "event.json")
			must(t, AtomicWrite(history, []byte("broken historical json"), 0600))
			before, err := os.Stat(history)
			must(t, err)
			guard := s.Guard
			must(t, s.Close())
			archive := filepath.Join(guard.Root, "events")
			must(t, os.Chmod(archive, 0000))
			t.Cleanup(func() { _ = os.Chmod(archive, 0700) })
			started := time.Now()
			s, err = OpenStore(guard)
			must(t, err)
			defer s.Close()
			must(t, s.ReplayOperations(context.Background()))
			must(t, s.RecoverPending(context.Background()))
			worked, err := s.RecoveryStep(context.Background())
			must(t, err)
			if worked {
				t.Fatal("completed archive was scheduled again")
			}
			var changed int
			must(t, s.DB.QueryRow("SELECT COUNT(*) FROM changed_events").Scan(&changed))
			must(t, os.Chmod(archive, 0700))
			after, err := os.Stat(history)
			must(t, err)
			if changed != 0 || !before.ModTime().Equal(after.ModTime()) {
				t.Fatal("startup rewrote historical metadata", changed)
			}
			t.Logf("%d completed events: startup %s", count, time.Since(started))
		})
	}
}
func TestJournalReplaysPublicationBoundaries(t *testing.T) {
	for _, phase := range []string{"assembly", "prepared", "sidecar", "renamed"} {
		t.Run(phase, func(t *testing.T) {
			s := testStore(t)
			ev := Event{ID: ID(), CameraID: ID(), Start: 1000, End: 6000, Cursor: 1000, Status: "closing"}
			must(t, s.SaveEvent(ev))
			p := Part{ID: ID(), EventID: ev.ID, CameraID: ev.CameraID, Start: 1000, End: 6000}
			p.Path = filepath.Join(eventDir(ev), "1000-"+p.ID+".mp4")
			full := filepath.Join(s.Root, p.Path)
			must(t, AtomicWrite(full+".partial", []byte("published video bytes"), 0600))
			var err error
			p.SHA256, p.MD5, p.Size, err = HashFile(full + ".partial")
			must(t, err)
			kind := "publish"
			if phase == "assembly" {
				kind = "assemble"
			}
			must(t, s.queueFileOperation("publish:"+p.ID, fileOperation{Kind: kind, Part: &p, Event: &ev, Cursor: ev.Cursor}))
			if phase == "sidecar" || phase == "renamed" {
				must(t, WriteJSON(full+".json", p))
			}
			if phase == "renamed" {
				must(t, os.Rename(full+".partial", full))
			}
			must(t, s.ReplayOperations(context.Background()))
			must(t, s.ReplayOperations(context.Background()))
			var pending int
			must(t, s.DB.QueryRow("SELECT COUNT(*) FROM file_operations").Scan(&pending))
			if pending != 0 {
				t.Fatal("journal not drained")
			}
			actual, err := s.Event(ev.ID)
			must(t, err)
			if phase == "assembly" {
				if _, err = s.Part(p.ID); !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("unfinished assembly was published", err)
				}
				if actual.Cursor != ev.Cursor {
					t.Fatal("unfinished assembly moved cursor")
				}
			} else {
				actualPart, err := s.Part(p.ID)
				must(t, err)
				if actualPart.SHA256 != p.SHA256 || actual.Cursor != p.End || s.QueueCount() != 1 {
					t.Fatal("publication not recovered", actual, actualPart, s.QueueCount())
				}
				data, err := os.ReadFile(full)
				must(t, err)
				if string(data) != "published video bytes" {
					t.Fatal("video changed")
				}
			}
		})
	}
}
func TestJournalRetriesMetadataProjection(t *testing.T) {
	s := testStore(t)
	ev := Event{ID: ID(), CameraID: ID(), Status: "open"}
	path := filepath.Join(s.Root, eventDir(ev), "event.json")
	must(t, os.MkdirAll(path, 0700))
	if err := s.SaveEvent(ev); err == nil {
		t.Fatal("expected sidecar failure")
	}
	actual, err := s.Event(ev.ID)
	must(t, err)
	if actual.ID != ev.ID {
		t.Fatal(actual)
	}
	must(t, os.Remove(path))
	must(t, s.ReplayOperations(context.Background()))
	data, err := os.ReadFile(path)
	must(t, err)
	must(t, json.Unmarshal(data, &actual))
	if actual.ID != ev.ID {
		t.Fatal(actual)
	}
}
func TestRebuildResumesWhileNewEventsAreWritten(t *testing.T) {
	s := testStore(t)
	ev := Event{ID: ID(), CameraID: ID(), Start: 1000, End: 1000000, Status: "closed", Uploaded: true}
	must(t, s.SaveEvent(ev))
	for i := 0; i < 205; i++ {
		p := fixturePart(t, s, ev, int64(1000+i*1000), int64(2000+i*1000))
		p.Uploaded = true
		must(t, s.SavePart(p))
	}
	guard := s.Guard
	must(t, s.Close())
	for _, suffix := range []string{"", "-wal", "-shm"} {
		err := os.Remove(filepath.Join(guard.Root, "catalog.sqlite"+suffix))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	var err error
	s, err = OpenStore(guard)
	must(t, err)
	// Traverse parents and stage a partial batch, then interrupt the rebuild.
	for i := 0; i < 4; i++ {
		_, err = s.RecoveryStep(context.Background())
		must(t, err)
	}
	if _, err = s.Event(ev.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("incomplete historical event was exposed", err)
	}
	live := Event{ID: ID(), CameraID: ev.CameraID, Start: time.Now().UnixMilli(), Status: "open"}
	must(t, s.SaveEvent(live))
	part := fixturePart(t, s, live, live.Start, live.Start+5000)
	before := s.RecoveryProgress()
	must(t, s.Close())
	s, err = OpenStore(guard)
	must(t, err)
	defer s.Close()
	if s.RecoveryProgress().Processed != before.Processed {
		t.Fatal("recovery progress reset")
	}
	finishRecovery(t, s)
	parts, err := s.Parts(ev.ID)
	must(t, err)
	if len(parts) != 205 {
		t.Fatal("history incomplete", len(parts))
	}
	current, err := s.Event(live.ID)
	must(t, err)
	if current != live {
		t.Fatal("import replaced live event", current)
	}
	if _, err = s.Part(part.ID); err != nil {
		t.Fatal("lost new recording", err)
	}
	var historicalJobs int
	must(t, s.DB.QueryRow("SELECT COUNT(*) FROM jobs WHERE ref!=?", part.ID).Scan(&historicalJobs))
	if historicalJobs != 0 {
		t.Fatal("acknowledged historical parts were queued again", historicalJobs)
	}
}
func TestCorruptCatalogIsPreservedAndRebuilt(t *testing.T) {
	root := t.TempDir()
	raw := []byte("not a sqlite database: preserve me")
	must(t, os.WriteFile(filepath.Join(root, "catalog.sqlite"), raw, 0600))
	s, err := OpenStore(Guard{Root: root, Development: true})
	must(t, err)
	defer s.Close()
	backups, err := filepath.Glob(filepath.Join(root, ".catalog-backup-*", "catalog.sqlite"))
	must(t, err)
	if len(backups) != 1 {
		t.Fatal("missing quarantine", backups)
	}
	data, err := os.ReadFile(backups[0])
	must(t, err)
	if string(data) != string(raw) {
		t.Fatal("original catalog modified")
	}
	must(t, s.ProbeHealth(context.Background()))
	ev := Event{ID: ID(), CameraID: ID(), Status: "open"}
	must(t, s.SaveEvent(ev))
	finishRecovery(t, s)
	if _, err = s.Event(ev.ID); err != nil {
		t.Fatal(err)
	}
}
func TestControlRemainsAvailableDuringAssembly(t *testing.T) {
	s := testStore(t)
	cfg := &ConfigFile{value: Config{Archive: s.Root, Timezone: "Europe/Moscow", Listen: "127.0.0.1:8080"}}
	r := &Runtime{Store: s, Engine: NewEngine(s, Binaries{})}
	must(t, r.initStreams(nil, time.Now()))
	a := &App{Config: cfg, Development: true, StateDir: t.TempDir(), Version: "v1.2.3", runtime: r}
	a.publishStatus(r)
	entered := make(chan struct{})
	finished := make(chan error, 1)
	r.Engine.assemble = func(ctx context.Context, _ *Store, _ Binaries, _ Event, _ []Segment) (Part, error) {
		close(entered)
		<-ctx.Done()
		return Part{}, ctx.Err()
	}
	_, _ = pendingRecording(t, s, ID(), time.Now().Add(-time.Minute))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { finished <- r.Engine.Tick(ctx, time.Now()) }()
	<-entered
	healthDone := make(chan struct{})
	go func() { defer close(healthDone); a.runHealth(ctx) }()
	defer func() { cancel(); <-healthDone }()
	deadline := time.Now().Add(3 * time.Second)
	for !a.Health().Ready && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !a.Health().Ready {
		t.Fatal("healthy control plane waited for recorder", a.Health())
	}
	started := time.Now()
	w := httptest.NewRecorder()
	a.internalHandler().ServeHTTP(w, httptest.NewRequest("GET", "/health", nil))
	_ = a.Status()
	prepare := httptest.NewRecorder()
	a.internalHandler().ServeHTTP(prepare, httptest.NewRequest("POST", "/prepare-update?immediate=true", nil))
	if w.Code != 200 || prepare.Code != 204 || time.Since(started) > time.Second {
		t.Fatal("control blocked", w.Code, prepare.Code, time.Since(started))
	}
	select {
	case err := <-finished:
		must(t, err)
	case <-time.After(time.Second):
		t.Fatal("assembly was not cancelled")
	}
}
func TestUpdaterCanRepairUnavailableApplication(t *testing.T) {
	u, signed := releaseFixture(t, "")
	must(t, os.MkdirAll(filepath.Join(u.Root, "current", "bin"), 0755))
	must(t, AtomicWrite(filepath.Join(u.Root, "current", "bin", "dozor"), []byte(systemFixtureBinary("v1.0.0", "old")), 0755))
	metadata, _ := json.Marshal(signed)
	download := u.Client.Transport
	u.Client.Transport = updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "updates.example" {
			return updateResponse(r, 200, string(metadata)), nil
		}
		return download.RoundTrip(r)
	})
	unavailable := &http.Client{Transport: updateRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("application is stopped") })}
	starts, stops := 0, 0
	u.Start = func() error { starts++; return nil }
	u.Stop = func() error { stops++; return nil }
	u.Healthy = func(context.Context, string) bool { return true }
	must(t, u.updateRunning(context.Background(), "https://updates.example/release.json", unavailable, func(string) {}))
	if starts != 1 || stops != 1 {
		t.Fatal("corrective update was blocked", starts, stops)
	}
	health, err := u.installedHealth(context.Background(), unavailable)
	must(t, err)
	if health.Version != signed.Release.Version {
		t.Fatal(health)
	}
}
func TestUpdaterSettingsIgnoreCameraFailures(t *testing.T) {
	root := t.TempDir()
	config := filepath.Join(root, "config.json")
	backup := filepath.Join(root, "updater.json")
	must(t, os.WriteFile(config, []byte(`{"auto_update":false,"cameras":[{"id":"broken"}]}`), 0600))
	cfg, err := loadUpdaterSettings(config, backup)
	must(t, err)
	if cfg.AutoUpdate {
		t.Fatal("disabled auto updates were enabled")
	}
	must(t, os.WriteFile(config, []byte("broken json"), 0600))
	again, err := loadUpdaterSettings(config, backup)
	must(t, err)
	if cfg != again {
		t.Fatal("last good updater settings lost")
	}
}
func TestCandidateUpdaterChecksSignatureAndStorage(t *testing.T) {
	u, s := releaseFixture(t, "")
	root := t.TempDir()
	must(t, WriteJSON(filepath.Join(root, "release.json"), s))
	must(t, os.WriteFile(filepath.Join(root, "update.pub"), []byte(base64.StdEncoding.EncodeToString(u.PublicKey)), 0600))
	staged, err := u.Stage(context.Background(), s)
	must(t, err)
	must(t, os.Link(filepath.Join(staged, ".bundle.tar.gz"), filepath.Join(root, "bundle.tar.gz")))
	must(t, VerifyUpdaterFixture(root))
	s.Release.Version = "v9.0.0"
	must(t, WriteJSON(filepath.Join(root, "release.json"), s))
	if err := VerifyUpdaterFixture(root); err == nil || !strings.Contains(err.Error(), "подпись") {
		t.Fatal("invalid future update verifier was accepted", err)
	}
}

func TestSegmentQueueSurvivesUnavailableAPIAndBadNotification(t *testing.T) {
	s := testStore(t)
	camera := ID()
	file := filepath.Join(s.Root, "buffer", camera, "2026-10-08_10-00-00.000000.mp4")
	must(t, AtomicWrite(file, []byte("segment"), 0600))
	queued, err := QueueSegment(s.Root, file, 5)
	must(t, err)
	must(t, AtomicWrite(filepath.Join(s.Root, ".segment-queue", "bad.json"), []byte("broken"), 0600))
	must(t, DrainSegmentQueue(context.Background(), s, s.AddSegment))
	must(t, DrainSegmentQueue(context.Background(), s, s.AddSegment))
	var n int
	must(t, s.DB.QueryRow("SELECT COUNT(*) FROM segments").Scan(&n))
	if n != 1 {
		t.Fatal("duplicate or missing queued segment", n)
	}
	if _, err = os.Stat(queued); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(s.Root, ".segment-queue-invalid", "bad.json")); err != nil {
		t.Fatal(err)
	}
}

func TestJournalDeletionAndUploadAcknowledgement(t *testing.T) {
	s := testStore(t)
	ev, seg := pendingRecording(t, s, ID(), time.Now())
	must(t, s.queueFileOperation("segment:"+seg.Path, fileOperation{Kind: "delete_segment", Path: seg.Path}))
	// Power failed after unlink, before committing the catalog change.
	must(t, os.Remove(filepath.Join(s.Root, seg.Path)))
	must(t, s.ReplayOperations(context.Background()))
	if s.HasSegment(seg.Path) {
		t.Fatal("deleted segment survived replay")
	}
	p := fixturePart(t, s, ev, ev.Start, ev.End)
	p.Uploaded = true
	tx, err := s.DB.Begin()
	must(t, err)
	must(t, putPart(tx, p))
	must(t, putOperation(tx, "part:"+p.ID, fileOperation{Kind: "part", Part: &p}))
	_, err = tx.Exec("DELETE FROM jobs WHERE id=?", "part:"+p.ID)
	must(t, err)
	must(t, tx.Commit())
	// A committed acknowledgement precedes its JSON projection.
	must(t, s.ReplayOperations(context.Background()))
	actual, err := s.Part(p.ID)
	must(t, err)
	if !actual.Uploaded || s.QueueCount() != 0 {
		t.Fatal("acknowledgement reset", actual, s.QueueCount())
	}
	var backup Part
	data, err := os.ReadFile(filepath.Join(s.Root, p.Path+".json"))
	must(t, err)
	must(t, json.Unmarshal(data, &backup))
	if !backup.Uploaded {
		t.Fatal("acknowledgement not projected")
	}
}

func TestRecoverySkipsBadEventAndPreservesWorkingTombstone(t *testing.T) {
	s := testStore(t)
	ev := Event{ID: ID(), CameraID: ID(), Start: 1000, End: 2000, Cursor: 2000, Status: "closed"}
	must(t, s.SaveEvent(ev))
	p := fixturePart(t, s, ev, 1000, 2000)
	// The active catalog is newer than the sidecar seen by recovery.
	current := p
	current.Deleted = true
	current.Uploaded = true
	tx, err := s.DB.Begin()
	must(t, err)
	must(t, putPart(tx, current))
	must(t, tx.Commit())
	bad := Event{ID: ID(), CameraID: ev.CameraID, Start: 1000}
	must(t, AtomicWrite(filepath.Join(s.Root, eventDir(bad), "event.json"), []byte("broken"), 0600))
	finishRecovery(t, s)
	actual, err := s.Part(p.ID)
	must(t, err)
	if !actual.Deleted || !actual.Uploaded {
		t.Fatal("working tombstone overwritten", actual)
	}
	if s.RecoveryProgress().Errors != 1 {
		t.Fatal(s.RecoveryProgress())
	}
}

func TestSystemSnapshotsReadLegacyAndRestoreUpdaterUnit(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(fmt.Sprint(legacy), func(t *testing.T) {
			u, _ := releaseFixture(t, "")
			system := setupSystemFixture(t, u)
			unit := filepath.Join(system.Root, "etc/systemd/system/dozor-update.service")
			must(t, AtomicWrite(unit, []byte("old updater unit"), 0644))
			name, err := system.snapshot(u.Root)
			must(t, err)
			if legacy {
				file := filepath.Join(u.Root, name, "backup.json")
				data, err := os.ReadFile(file)
				must(t, err)
				var backup systemBackup
				must(t, json.Unmarshal(data, &backup))
				backup.Protocol = 1
				backup.Files = backup.Files[:2]
				must(t, WriteJSON(file, backup))
			}
			must(t, AtomicWrite(unit, []byte("new updater unit"), 0644))
			must(t, system.restore(u.Root, name))
			want := "old updater unit"
			if legacy {
				want = "new updater unit"
			}
			assertSystemFile(t, unit, want, 0644)
		})
	}
}

func TestUpdateHealthFailuresNameCriticalComponent(t *testing.T) {
	for _, component := range []string{"core", "database", "updater"} {
		t.Run(component, func(t *testing.T) {
			u, signed := releaseFixture(t, "")
			h := processHealth{Core: checkResult(nil), Database: checkResult(nil), Updater: checkResult(nil)}
			failed := checkResult(errors.New("injected " + component + " failure"))
			switch component {
			case "core":
				h.Core = failed
			case "database":
				h.Database = failed
			case "updater":
				h.Updater = failed
			}
			u.Healthy = func(context.Context, string) bool { u.HealthError = h.failure(); return false }
			err := u.Apply(context.Background(), signed)
			if err == nil || !strings.Contains(err.Error(), "injected "+component) {
				t.Fatal("missing rollback reason", err)
			}
			current, err := filepath.EvalSymlinks(filepath.Join(u.Root, "current"))
			must(t, err)
			if filepath.Base(current) != "v1.0.0" {
				t.Fatal("critical failure did not roll back", current)
			}
		})
	}
}

func TestLegacyReconciliationPreservesVersionWithoutApplicationBinary(t *testing.T) {
	u, _ := releaseFixture(t, "")
	setupSystemFixture(t, u)
	must(t, u.reconcileSystem(context.Background(), "v1.0.0"))
	must(t, os.Remove(filepath.Join(u.Root, "current", "bin", "dozor")))
	unavailable := &http.Client{Transport: updateRoundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("version lookup depended on API")
		return nil, errors.New("stopped")
	})}
	health, err := u.installedHealth(context.Background(), unavailable)
	must(t, err)
	if health.Version != "v1.0.0" {
		t.Fatal(health)
	}
}

func TestRecoveryDiagnosesCorruptVideoAndContinues(t *testing.T) {
	s := testStore(t)
	ev := Event{ID: ID(), CameraID: ID(), Start: 1000, End: 2000, Cursor: 2000, Status: "closed"}
	must(t, s.SaveEvent(ev))
	p := fixturePart(t, s, ev, 1000, 2000)
	_, err := s.DB.Exec("DELETE FROM parts; DELETE FROM events; DELETE FROM jobs")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(s.Root, p.Path), []byte("corrupt video"), 0600))
	finishRecovery(t, s)
	actual, err := s.Part(p.ID)
	must(t, err)
	recovered, err := s.Event(ev.ID)
	must(t, err)
	if !actual.Lost || !actual.Deleted || !recovered.Lost || s.RecoveryProgress().Errors != 1 {
		t.Fatal(actual, recovered, s.RecoveryProgress())
	}
	if _, err = os.Stat(filepath.Join(s.Root, p.Path)); err != nil {
		t.Fatal("damaged source not preserved", err)
	}
}

func TestStalePublicationDoesNotDuplicateOrResetAcknowledgement(t *testing.T) {
	for _, committed := range []bool{false, true} {
		t.Run(fmt.Sprint(committed), func(t *testing.T) {
			s := testStore(t)
			ev := Event{ID: ID(), CameraID: ID(), Start: 1000, End: 3000, Cursor: 1000, Status: "closing"}
			must(t, s.SaveEvent(ev))
			p := fixturePart(t, s, ev, 1000, 3000)
			if committed {
				latest := p
				latest.Uploaded = true
				must(t, s.SavePart(latest))
			} else {
				_, err := s.DB.Exec("DELETE FROM parts; DELETE FROM jobs")
				must(t, err)
			}
			must(t, s.queueFileOperation("publish:"+p.ID, fileOperation{Kind: "publish", Event: &ev, Part: &p, Cursor: ev.Cursor}))
			ev.Cursor = 3000
			must(t, s.SaveEvent(ev))
			must(t, s.ReplayOperations(context.Background()))
			actual, err := s.Part(p.ID)
			if committed {
				must(t, err)
				if !actual.Uploaded {
					t.Fatal("acknowledgement reset")
				}
			} else if !errors.Is(err, sql.ErrNoRows) {
				t.Fatal("stale part republished", actual, err)
			}
			if s.QueueCount() != 0 {
				t.Fatal("stale publication was queued")
			}
		})
	}
}

func TestUpdateAcceptsBrokenRecorderBinaries(t *testing.T) {
	u, signed := releaseFixture(t, "", map[string]string{"bin/mediamtx": "broken executable", "bin/ffmpeg": "broken executable", "bin/ffprobe": "broken executable"})
	staged, err := u.Stage(context.Background(), signed)
	must(t, err)
	must(t, preflightRelease(context.Background(), staged, signed.Release.Version))
	u.Healthy = func(context.Context, string) bool { return true }
	must(t, u.Apply(context.Background(), signed))
	current, err := filepath.EvalSymlinks(filepath.Join(u.Root, "current"))
	must(t, err)
	if filepath.Base(current) != signed.Release.Version {
		t.Fatal("recorder vetoed release")
	}
}

func TestCandidateRejectsBrokenUpdaterCommand(t *testing.T) {
	manifest, _ := json.Marshal(systemManifest{Protocol: 1, UpdaterProtocol: 1, Version: "v1.1.0", Polkit: "rules"})
	binary := "#!/bin/sh\nif [ \"$2\" = integration ]; then\nprintf '%s\\n' '" + string(manifest) + "'\nelse\nexit 1\nfi\n"
	u, signed := releaseFixture(t, "", map[string]string{"bin/dozor": binary})
	setupSystemFixture(t, u)
	staged, err := u.Stage(context.Background(), signed)
	must(t, err)
	if err = u.checkCandidateUpdater(context.Background(), staged, signed); err == nil || !strings.Contains(err.Error(), "самопроверка обновлятора") {
		t.Fatal("broken updater accepted", err)
	}
}

func TestLegacyTransitionResumesAndInvalidatesAfterRollback(t *testing.T) {
	s := testStore(t)
	finishRecovery(t, s)
	first := ID()
	must(t, s.requestRecovery(first))
	_, err := s.DB.Exec("INSERT OR REPLACE INTO metadata VALUES('recovery_processed','17')")
	must(t, err)
	must(t, s.requestRecovery(first))
	if s.RecoveryProgress().Processed != 17 {
		t.Fatal("transition restarted a checkpoint")
	}
	finishRecovery(t, s)
	must(t, s.requestRecovery(ID()))
	if p := s.RecoveryProgress(); p.State != "running" || p.Processed != 0 {
		t.Fatal("legacy rollback did not invalidate catalog marker", p)
	}
}

func TestBufferRecoveryDoesNotDelayNewEvents(t *testing.T) {
	s := testStore(t)
	e := recordingEngine(t, s)
	e.deferRecovered = true
	now := time.Now()
	old, _ := pendingRecording(t, s, ID(), now.Add(-time.Hour))
	old.Status = "closing"
	must(t, s.SaveEvent(old))
	camera := ID()
	e.Signal(MotionSignal{CameraID: camera, Active: true, Healthy: true, At: now})
	must(t, e.AddSegment(Segment{Path: "fresh.mp4", CameraID: camera, Start: now.Add(-60 * time.Second).UnixMilli(), End: now.UnixMilli()}))
	must(t, e.Tick(context.Background(), now))
	historical, err := s.Parts(old.ID)
	must(t, err)
	fresh, err := s.Events(camera, 1)
	must(t, err)
	if len(historical) != 0 || len(fresh) != 1 {
		t.Fatal("buffer recovery blocked or processed the wrong event", historical, fresh)
	}
	parts, err := s.Parts(fresh[0].ID)
	must(t, err)
	if len(parts) != 1 {
		t.Fatal("new recording waited for historical buffer", parts)
	}
}
