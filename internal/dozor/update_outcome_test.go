package dozor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// updateAttempt runs one attempt with the same bookkeeping SystemUpdate uses
// (updateRecorder): progress while it works, then one final record from the
// last message and the returned error.
func updateAttempt(t *testing.T, state string, u *Updater, run func(status func(string)) error) (updateStatus, error) {
	t.Helper()
	record := newUpdateRecorder(state, u)
	status := func(msg string) {
		record.status(msg)
		if progress := readUpdateStatus(state); !progress.Running || progress.Outcome != "" || progress.atMillis() != 0 {
			t.Fatalf("progress already has an outcome: %+v", progress)
		}
	}
	err := run(status)
	must(t, record.finish(err))
	return readUpdateStatus(state), err
}

// outcomeFixture offers v1.1.0 from a custom release source while the running
// application reports the given version.
func outcomeFixture(t *testing.T, running, afterDownload string, busy, unhealthy, offline bool) (*Updater, *http.Client) {
	t.Helper()
	u, signed := releaseFixture(t, "")
	manifest, err := json.Marshal(signed)
	must(t, err)
	download := u.Client.Transport
	u.Client.Transport = updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host != "updates.example" {
			return download.RoundTrip(r)
		}
		if offline {
			return nil, errors.New("offline")
		}
		return updateResponse(r, 200, string(manifest)), nil
	})
	reads := 0
	client := &http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.URL.Path {
		case "/health":
			reads++
			version := running
			if reads > 1 && afterDownload != "" {
				version = afterDownload
			}
			return updateResponse(r, 200, `{"version":"`+version+`","ready":true}`), nil
		case "/prepare-update":
			if busy {
				return updateResponse(r, 409, ""), nil
			}
			return updateResponse(r, 204, ""), nil
		}
		t.Fatalf("unexpected control request: %s", r.URL)
		return nil, errors.New("unexpected request")
	})}
	u.Start = func() error { return nil }
	u.Stop = func() error { return nil }
	u.Healthy = func(_ context.Context, version string) bool { return !unhealthy && version == signed.Release.Version }
	return u, client
}

func TestUpdateOutcomeMapping(t *testing.T) {
	for _, tc := range []struct {
		name, running, afterDownload string
		busy, unhealthy, offline     bool
		outcome, target, message     string
	}{
		{name: "installed", running: "v1.0.0", outcome: updateOutcomeOK, target: "v1.1.0", message: "Установлена v1.1.0 · "},
		{name: "already current", running: "v1.1.0", outcome: updateOutcomeNoop, message: "Установлена актуальная версия"},
		{name: "caught up during download", running: "v1.0.0", afterDownload: "v1.2.0", outcome: updateOutcomeNoop, message: "Установлена актуальная версия"},
		{name: "waiting for an idle window", running: "v1.0.0", busy: true, outcome: updateOutcomeDeferred, target: "v1.1.0", message: "Обновление загружено; ожидаем завершения событий"},
		{name: "release source unavailable", running: "v1.0.0", offline: true, outcome: updateOutcomeFailed},
		{name: "rolled back after failed health", running: "v1.0.0", unhealthy: true, outcome: updateOutcomeFailed, target: "v1.1.0", message: "обновление не прошло проверку; предыдущая версия восстановлена"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, client := outcomeFixture(t, tc.running, tc.afterDownload, tc.busy, tc.unhealthy, tc.offline)
			state := t.TempDir()
			before := time.Now().UnixMilli()
			got, err := updateAttempt(t, state, u, func(status func(string)) error {
				return u.updateRunning(context.Background(), "https://updates.example/release.json", client, status)
			})
			after := time.Now().UnixMilli()
			if (err != nil) != (tc.outcome == updateOutcomeFailed) {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.Running || got.Outcome != tc.outcome || got.Target != tc.target {
				t.Fatalf("status = %+v, want outcome %q target %q", got, tc.outcome, tc.target)
			}
			if err != nil && got.Message != err.Error() {
				t.Fatalf("message = %q, want the error text %q", got.Message, err)
			}
			if !strings.Contains(got.Message, tc.message) {
				t.Fatalf("message = %q, want it to contain %q", got.Message, tc.message)
			}
			if at := got.atMillis(); at < before || at > after {
				t.Fatalf("update_at = %d, want a time of this attempt (%d..%d)", at, before, after)
			}
			if again := readUpdateStatus(state); again != got {
				t.Fatalf("status changed between reads: %+v, then %+v", got, again)
			}
		})
	}
}

// Runs that stop before the release check also finish with an outcome.
func TestUpdateOutcomeSkippedRuns(t *testing.T) {
	for _, tc := range []struct {
		name, outcome, message string
		finish                 func(*Updater, func(string))
	}{
		{"auto update off", updateOutcomeDisabled, "Автообновление выключено", (*Updater).finishDisabled},
		{"helper brought up to date", updateOutcomeNoop, "Служба обновления готова", (*Updater).finishReconciled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			u := &Updater{}
			got, err := updateAttempt(t, state, u, func(status func(string)) error {
				tc.finish(u, status)
				return nil
			})
			must(t, err)
			if got.Running || got.Outcome != tc.outcome || got.Target != "" || got.Message != tc.message || got.atMillis() == 0 {
				t.Fatalf("status = %+v, want outcome %q and message %q", got, tc.outcome, tc.message)
			}
		})
	}
}

// An outcome of a previous attempt must not leak into the next one.
func TestUpdateOutcomeResetsBetweenAttempts(t *testing.T) {
	u, client := outcomeFixture(t, "v1.0.0", "", false, true, false)
	state := t.TempDir()
	run := func(status func(string)) error {
		return u.updateRunning(context.Background(), "https://updates.example/release.json", client, status)
	}
	if got, err := updateAttempt(t, state, u, run); err == nil || got.Outcome != updateOutcomeFailed || got.Target != "v1.1.0" {
		t.Fatalf("first attempt: %+v, %v", got, err)
	}
	u.Healthy = func(_ context.Context, version string) bool { return version == "v1.1.0" }
	if got, err := updateAttempt(t, state, u, run); err != nil || got.Outcome != updateOutcomeOK || got.Target != "v1.1.0" {
		t.Fatalf("second attempt: %+v, %v", got, err)
	}
	// A failure before the target is known must not keep the earlier target.
	u.Client.Transport = updateRoundTripFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("offline") })
	if got, err := updateAttempt(t, state, u, run); err == nil || got.Outcome != updateOutcomeFailed || got.Target != "" {
		t.Fatalf("third attempt: %+v, %v", got, err)
	}
}

func TestInterruptedUpdateIsFailedWithStableTime(t *testing.T) {
	state := t.TempDir()
	path := filepath.Join(state, "update-status.json")
	started := time.Now().Add(-27 * time.Minute).UTC().Truncate(time.Millisecond)
	record, err := json.Marshal(updateStatus{Message: "Скачиваем и проверяем обновление", Running: true, At: started, Target: "v1.1.0"})
	must(t, err)
	must(t, AtomicWrite(path, record, 0644))
	first := readUpdateStatus(state)
	if first.Running || first.Outcome != updateOutcomeFailed || first.Target != "v1.1.0" || !strings.Contains(first.Message, "прервана") {
		t.Fatalf("interrupted update: %+v", first)
	}
	// The time is derived from the stored record: the moment it went stale.
	if want := started.Add(26 * time.Minute).UnixMilli(); first.atMillis() != want {
		t.Fatalf("update_at = %d, want %d", first.atMillis(), want)
	}
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if again := readUpdateStatus(state); again != first {
			t.Fatalf("read %d re-stamped the result: %+v, then %+v", i, first, again)
		}
	}
	if after, err := os.ReadFile(path); err != nil || string(after) != string(record) {
		t.Fatalf("a read rewrote the status file: %q, %v", after, err)
	}
	// A running attempt that is not stale yet has no outcome.
	must(t, recordUpdateProgress(state, "Скачиваем и проверяем обновление", &Updater{Target: "v1.1.0"}))
	if fresh := readUpdateStatus(state); !fresh.Running || fresh.Outcome != "" || fresh.atMillis() != 0 || fresh.Target != "v1.1.0" {
		t.Fatalf("running attempt: %+v", fresh)
	}
}

// The final write is repeated once; a second failure is reported, not retried again.
func TestUpdateRecorderRetriesFinalWrite(t *testing.T) {
	for _, tc := range []struct {
		name     string
		failures int
		calls    int
		failed   bool
	}{
		{"first write fails", 1, 2, false},
		{"both writes fail", 2, 2, true},
		{"no failure", 0, 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			record := newUpdateRecorder(t.TempDir(), &Updater{Outcome: updateOutcomeNoop})
			calls := 0
			write := record.write
			record.write = func(state, message string, u *Updater, err error) error {
				calls++
				if calls <= tc.failures {
					return errors.New("disk error")
				}
				return write(state, message, u, err)
			}
			err := record.finish(nil)
			if calls != tc.calls || (err != nil) != tc.failed {
				t.Fatalf("calls = %d, err = %v; want %d calls, failure %t", calls, err, tc.calls, tc.failed)
			}
			if got := readUpdateStatus(record.state); !tc.failed && (got.Running || got.Outcome != updateOutcomeNoop || got.atMillis() == 0) {
				t.Fatalf("final record was not stored: %+v", got)
			}
		})
	}
}

func TestLegacyUpdateStatusHasNoOutcome(t *testing.T) {
	for name, content := range map[string]string{
		"finished":   `{"message":"Установлена актуальная версия","running":false,"at":"2026-10-01T06:12:00Z"}`,
		"failed":     `{"message":"сбой скачивания","running":false,"at":"2026-10-01T06:12:00Z"}`,
		"running":    `{"message":"Проверяем наличие обновлений","running":true,"at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`,
		"plain text": "Установлена актуальная версия",
	} {
		t.Run(name, func(t *testing.T) {
			a, dir := newUpdateStatusApp(t)
			must(t, AtomicWrite(filepath.Join(dir, "update-status.json"), []byte(content), 0644))
			got := readUpdateStatus(dir)
			if got.Outcome != "" || got.Target != "" || got.atMillis() != 0 {
				t.Fatalf("legacy file got an outcome: %+v", got)
			}
			status := a.Status()
			if status["update_outcome"] != "" || status["update_target"] != "" || status["update_at"] != int64(0) {
				t.Fatalf("legacy status: %+v", status)
			}
		})
	}
}

// A killed attempt is a real failure whoever wrote its record, so a legacy
// "running" record without outcome and target fields also ends as failed once
// it is stale. Like for new records the time is derived from the file.
func TestStaleLegacyUpdateStatusIsFailed(t *testing.T) {
	a, dir := newUpdateStatusApp(t)
	started := time.Now().Add(-27 * time.Minute).UTC().Truncate(time.Second)
	content := `{"message":"Проверяем наличие обновлений","running":true,"at":"` + started.Format(time.RFC3339) + `"}`
	must(t, AtomicWrite(filepath.Join(dir, "update-status.json"), []byte(content), 0644))
	got := readUpdateStatus(dir)
	if want := started.Add(26 * time.Minute).UnixMilli(); got.Running || got.Outcome != updateOutcomeFailed || got.Target != "" || got.atMillis() != want || !strings.Contains(got.Message, "прервана") {
		t.Fatalf("stale legacy status: %+v, want update_at %d", got, want)
	}
	status := a.Status()
	if status["update_outcome"] != "failed" || status["update_target"] != "" || status["update_running"] != false || status["update_at"] != started.Add(26*time.Minute).UnixMilli() {
		t.Fatalf("stale legacy status: %+v", status)
	}
	if again := a.Status(); again["update_at"] != status["update_at"] {
		t.Fatalf("update_at changed between reads: %v, %v", status["update_at"], again["update_at"])
	}
}

func newUpdateStatusApp(t *testing.T) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	cfg, err := LoadConfig(filepath.Join(dir, "config.json"))
	must(t, err)
	a, err := NewApp(cfg, Binaries{}, true, dir, filepath.Join(dir, "s.sock"), "v1.0.0")
	must(t, err)
	return a, dir
}

func TestStatusExposesUpdateOutcome(t *testing.T) {
	a, dir := newUpdateStatusApp(t)
	// Nothing is known before the first attempt.
	var empty map[string]any
	data, err := json.Marshal(a.Status())
	must(t, err)
	must(t, json.Unmarshal(data, &empty))
	if empty["update_outcome"] != "" || empty["update_target"] != "" || empty["update_at"] != float64(0) {
		t.Fatalf("empty status: %+v", empty)
	}
	before := time.Now().UnixMilli()
	must(t, writeUpdateOutcome(dir, "Установлена v1.1.0", updateOutcomeOK, "v1.1.0"))
	data, err = json.Marshal(a.Status())
	must(t, err)
	var got map[string]any
	must(t, json.Unmarshal(data, &got))
	at, _ := got["update_at"].(float64)
	if got["update_outcome"] != "ok" || got["update_target"] != "v1.1.0" || got["update_running"] != false || got["update_status"] != "Установлена v1.1.0" || int64(at) < before {
		t.Fatalf("status = %+v", got)
	}
	if again, _ := a.Status()["update_at"].(int64); int64(at) != again {
		t.Fatalf("update_at changed between reads: %v, %v", at, again)
	}
}
