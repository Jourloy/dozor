package dozor

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func rebootTime(t *testing.T, value string) time.Time {
	t.Helper()
	at, err := time.Parse(time.RFC3339, value)
	must(t, err)
	return at
}

func TestRebootScheduleDefaultsAndCadence(t *testing.T) {
	start := rebootTime(t, "2026-10-07T00:00:00Z")
	s, err := loadRebootScheduler(t.TempDir(), start)
	must(t, err)
	state := s.Get()
	if !state.Enabled || state.IntervalDays != 1 || state.Time != "03:15" || state.Timezone != "Europe/Moscow" || *state.NextRebootAt != start.Add(15*time.Minute).UnixMilli() {
		t.Fatalf("unexpected defaults: %+v", state)
	}
	for _, tc := range []struct {
		now, want string
		days      int
	}{
		{"2026-10-07T00:15:00Z", "2026-10-08T00:15:00Z", 1},
		{"2026-10-07T00:14:59Z", "2026-10-07T00:15:00Z", 1},
		{"2026-12-31T22:00:00Z", "2027-01-01T00:15:00Z", 7},
		{"2028-02-28T23:30:00Z", "2028-02-29T00:15:00Z", 365},
	} {
		schedule := RebootSchedule{Enabled: true, IntervalDays: tc.days, Time: "03:15"}
		if got := schedule.firstAfter(rebootTime(t, tc.now)); got != rebootTime(t, tc.want).UnixMilli() {
			t.Fatalf("%s: got %v", tc.now, time.UnixMilli(got))
		}
	}
	state, err = s.Save(RebootSchedule{Enabled: true, IntervalDays: 7, Time: "04:20"}, start)
	must(t, err)
	first := *state.NextRebootAt
	state, err = s.Save(state.RebootSchedule, start.Add(48*time.Hour))
	must(t, err)
	if *state.NextRebootAt != first {
		t.Fatal("idempotent PUT moved the next occurrence")
	}
	// Downtime skips missed occurrences but retains the original seven-day cadence.
	restored, err := loadRebootScheduler(filepath.Dir(s.path), start.Add(9*24*time.Hour))
	must(t, err)
	if *restored.Get().NextRebootAt != rebootTime(t, "2026-10-21T01:20:00Z").UnixMilli() {
		t.Fatal("cadence changed across downtime")
	}
	state, err = restored.Save(RebootSchedule{Enabled: false, IntervalDays: 7, Time: "04:20"}, start)
	must(t, err)
	if state.NextRebootAt != nil {
		t.Fatal("disabled schedule has a pending reboot")
	}
	restored, err = loadRebootScheduler(filepath.Dir(s.path), start)
	must(t, err)
	if restored.Get().Enabled {
		t.Fatal("restart enabled a disabled schedule")
	}
}

func TestRebootTickPersistsBeforeCommandAndNeverReplays(t *testing.T) {
	for _, failure := range []bool{false, true} {
		now := rebootTime(t, "2026-10-07T00:00:00Z")
		s, err := loadRebootScheduler(t.TempDir(), now)
		must(t, err)
		at := time.UnixMilli(*s.Get().NextRebootAt)
		calls := 0
		reboot := func() error {
			calls++
			bytes, err := os.ReadFile(s.path)
			must(t, err)
			var disk RebootState
			must(t, json.Unmarshal(bytes, &disk))
			if *disk.NextRebootAt <= at.UnixMilli() {
				t.Fatal("command ran before durable next occurrence")
			}
			if failure {
				return errors.New("blocked")
			}
			return nil
		}
		must(t, s.tick(at.Add(-time.Second), reboot))
		if calls != 0 {
			t.Fatal("rebooted early")
		}
		if err = s.tick(at, reboot); (err != nil) != failure {
			t.Fatalf("failure=%v, err=%v", failure, err)
		}
		must(t, s.tick(at.Add(10*time.Second), reboot))
		// A process restarting within the same minute cannot reboot again.
		s, err = loadRebootScheduler(filepath.Dir(s.path), at.Add(20*time.Second))
		must(t, err)
		must(t, s.tick(at.Add(30*time.Second), reboot))
		if calls != 1 {
			t.Fatalf("reboot calls=%d", calls)
		}
		if (s.Get().LastError != "") != failure {
			t.Fatal("last error not persisted")
		}
	}
}

func TestRebootTickSkipsMissedSlotsAndRequiresPersistence(t *testing.T) {
	now := rebootTime(t, "2026-10-07T00:00:00Z")
	s, err := loadRebootScheduler(t.TempDir(), now)
	must(t, err)
	at := time.UnixMilli(*s.Get().NextRebootAt)
	never := func() error { t.Fatal("must not reboot"); return nil }
	must(t, s.tick(at.Add(time.Minute), never))
	must(t, os.Remove(s.path))
	must(t, os.Mkdir(s.path, 0700))
	if s.tick(time.UnixMilli(*s.Get().NextRebootAt), never) == nil {
		t.Fatal("ignored a persistence failure")
	}
}

func TestRebootScheduleAPI(t *testing.T) {
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
		{`{"enabled":true,"interval_days":7,"time":"05:45"}`, "csrf", "", true, 200},
		{`{"enabled":false,"interval_days":7,"time":"05:45"}`, "csrf", "", true, 200},
		{`{"enabled":true,"interval_days":0,"time":"03:15"}`, "csrf", "", true, 400},
		{`{"enabled":true,"interval_days":366,"time":"03:15"}`, "csrf", "", true, 400},
		{`{"enabled":true,"interval_days":1.5,"time":"03:15"}`, "csrf", "", true, 400},
		{`{"enabled":true,"interval_days":1,"time":"3:15"}`, "csrf", "", true, 400},
		{`{"enabled":true,"interval_days":1,"time":"24:00"}`, "csrf", "", true, 400},
		{`{"enabled":true,"interval_days":1,"time":"03:60"}`, "csrf", "", true, 400},
		{`{"interval_days":1,"time":"03:15"}`, "csrf", "", true, 400},
		{`{"enabled":false,"interval_days":7,"time":"05:45","timezone":"UTC"}`, "csrf", "", true, 400},
		{`{}`, "csrf", "", false, 401},
		{`{}`, "", "", true, 403},
		{`{}`, "csrf", "https://other.example", true, 403},
	} {
		before := a.reboots.Get()
		r := httptest.NewRequest("PUT", "/api/v1/reboot-schedule", strings.NewReader(tc.body))
		if tc.auth {
			r.AddCookie(&http.Cookie{Name: "dozor_session", Value: "test"})
		}
		r.Header.Set("X-CSRF-Token", tc.csrf)
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("%s: status=%d, body=%s", tc.body, w.Code, w.Body.String())
		}
		if tc.want != 200 && a.reboots.Get().RebootSchedule != before.RebootSchedule {
			t.Fatal("invalid request changed schedule")
		}
	}
	for _, auth := range []bool{false, true} {
		r := httptest.NewRequest("GET", "/api/v1/reboot-schedule", nil)
		if auth {
			r.AddCookie(&http.Cookie{Name: "dozor_session", Value: "test"})
		}
		w := httptest.NewRecorder()
		a.Handler().ServeHTTP(w, r)
		if auth && w.Code != 200 || !auth && w.Code != 401 {
			t.Fatalf("GET auth=%v: %d", auth, w.Code)
		}
	}
	if c.Get().Timezone != "Europe/Moscow" || len(c.Get().Cameras) != 0 {
		t.Fatal("schedule changed unrelated settings")
	}
}

func TestScheduledRebootCommandAndUpdateGuard(t *testing.T) {
	for _, active := range []string{"inactive", "active", "activating"} {
		dir := t.TempDir()
		script := "#!/bin/sh\nif [ \"$1\" = show ]; then echo \"$DOZOR_TEST_REBOOT_ACTIVE\"; else printf '%s ' \"$@\" > \"$DOZOR_TEST_REBOOT_LOG\"; fi\n"
		must(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0700))
		t.Setenv("PATH", dir)
		t.Setenv("DOZOR_TEST_REBOOT_ACTIVE", active)
		t.Setenv("DOZOR_TEST_REBOOT_LOG", filepath.Join(dir, "command"))
		a := &App{StateDir: dir}
		err := a.reboot(context.Background())
		b, readErr := os.ReadFile(filepath.Join(dir, "command"))
		if active == "inactive" {
			must(t, err)
			must(t, readErr)
			if string(b) != "--no-ask-password --no-block reboot " {
				t.Fatalf("unexpected command: %s", b)
			}
		} else if err == nil || !os.IsNotExist(readErr) {
			t.Fatal("reboot ran during update")
		}
	}
}
