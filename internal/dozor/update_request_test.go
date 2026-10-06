package dozor

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManualUpdateAPI(t *testing.T) {
	for _, tc := range []struct {
		name, state, csrf, origin string
		auth                      bool
		exit, want                int
	}{
		{name: "start with auto updates disabled", state: "inactive", csrf: "csrf", auth: true, want: 202},
		{name: "retry failed unit", state: "failed", csrf: "csrf", auth: true, want: 202},
		{name: "already starting", state: "activating", csrf: "csrf", auth: true, want: 409},
		{name: "already running", state: "active", csrf: "csrf", auth: true, want: 409},
		{name: "still stopping", state: "deactivating", csrf: "csrf", auth: true, want: 409},
		{name: "missing unit", csrf: "csrf", auth: true, want: 503},
		{name: "start denied", state: "inactive", csrf: "csrf", auth: true, exit: 1, want: 503},
		{name: "missing session", state: "inactive", csrf: "csrf", want: 401},
		{name: "missing CSRF", state: "inactive", auth: true, want: 403},
		{name: "cross origin", state: "inactive", csrf: "csrf", auth: true, origin: "https://elsewhere.example", want: 403},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "commands")
			script := "#!/bin/sh\nprintf '%s ' \"$@\" >> \"$DOZOR_TEST_UPDATE_LOG\"\nprintf '\\n' >> \"$DOZOR_TEST_UPDATE_LOG\"\nif [ \"$1\" = show ]; then\n  printf '%s\\n' \"$DOZOR_TEST_UPDATE_STATE\"\n  exit 0\nfi\nexit \"$DOZOR_TEST_UPDATE_EXIT\"\n"
			must(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte(script), 0700))
			t.Setenv("PATH", dir)
			t.Setenv("DOZOR_TEST_UPDATE_LOG", log)
			t.Setenv("DOZOR_TEST_UPDATE_STATE", tc.state)
			t.Setenv("DOZOR_TEST_UPDATE_EXIT", fmt.Sprint(tc.exit))
			cfg, err := LoadConfig(filepath.Join(dir, "config.json"))
			must(t, err)
			config := cfg.Get()
			config.AutoUpdate = false
			must(t, cfg.Save(config))
			a, err := NewApp(cfg, Binaries{}, true, dir, filepath.Join(dir, "s.sock"), "v1.0.0")
			must(t, err)
			a.sessions["test"] = Session{CSRF: "csrf", Expires: time.Now().Add(time.Hour)}
			r := httptest.NewRequest("POST", "/api/v1/updates/check", nil)
			if tc.auth {
				r.AddCookie(&http.Cookie{Name: "dozor_session", Value: "test"})
			}
			r.Header.Set("X-CSRF-Token", tc.csrf)
			r.Header.Set("Origin", tc.origin)
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
			commands, err := os.ReadFile(log)
			if tc.want == 401 || tc.want == 403 {
				if !os.IsNotExist(err) {
					t.Fatal("unauthorized request reached systemctl")
				}
				return
			}
			must(t, err)
			wantCommands := "show --property=ActiveState --value dozor-update.service \n"
			if tc.want == 202 || tc.exit != 0 {
				wantCommands += "--no-block start dozor-update.service \n"
			}
			if string(commands) != wantCommands {
				t.Fatalf("unexpected commands: %q", commands)
			}
			if cfg.Get().AutoUpdate {
				t.Fatal("manual update changed the automatic update setting")
			}
			_, err = os.Stat(filepath.Join(dir, "update-request"))
			if tc.want == 202 {
				must(t, err)
				if status := a.Status(); status["update_running"] != true || status["update_status"] != "Проверяем наличие обновлений" {
					t.Fatalf("missing progress: %+v", status)
				}
				// Only the private control socket can consume the request, once.
				client := &http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
					w := httptest.NewRecorder()
					a.internalHandler().ServeHTTP(w, r)
					return w.Result(), nil
				})}
				for _, want := range []bool{true, false} {
					immediate, err := readUpdateRequest(context.Background(), client)
					must(t, err)
					if immediate != want {
						t.Fatalf("immediate = %t, want %t", immediate, want)
					}
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("failed request left a manual update pending: %v", err)
			}
			if tc.exit != 0 && readUpdateStatus(dir).Running {
				t.Fatal("failed start left the update button busy")
			}
		})
	}
}

func TestUpdateStatusCompatibility(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "update-status.json")
	must(t, AtomicWrite(path, []byte("Установлена актуальная версия"), 0644))
	if status := readUpdateStatus(dir); status.Message != "Установлена актуальная версия" || status.Running {
		t.Fatalf("legacy status: %+v", status)
	}
	must(t, writeUpdateStatus(dir, "Скачиваем и проверяем обновление", true))
	if !readUpdateStatus(dir).Running {
		t.Fatal("active update was lost")
	}
	old, err := json.Marshal(updateStatus{Message: "Скачиваем и проверяем обновление", Running: true, At: time.Now().Add(-27 * time.Minute)})
	must(t, err)
	must(t, AtomicWrite(path, old, 0644))
	if status := readUpdateStatus(dir); status.Running || !strings.Contains(status.Message, "прервана") {
		t.Fatalf("interrupted updater left a busy status: %+v", status)
	}
	must(t, writeUpdateStatus(dir, "Установлена актуальная версия", false))
	if readUpdateStatus(dir).Running {
		t.Fatal("finished check left the update button busy")
	}
}

func TestManualUpdatePausesActiveRecording(t *testing.T) {
	for _, immediate := range []bool{false, true} {
		t.Run(fmt.Sprint(immediate), func(t *testing.T) {
			engine := NewEngine(testStore(t), Binaries{})
			engine.active["camera"] = "event"
			engine.states["camera"] = MotionState{Active: true}
			a := &App{runtime: &Runtime{Engine: engine}}
			r := httptest.NewRequest("POST", fmt.Sprintf("/prepare-update?immediate=%t", immediate), nil)
			w := httptest.NewRecorder()
			a.internalHandler().ServeHTTP(w, r)
			want := http.StatusConflict
			if immediate {
				want = http.StatusNoContent
			}
			if w.Code != want || engine.paused != immediate || engine.active["camera"] != "event" {
				t.Fatalf("prepare status=%d paused=%t active=%v", w.Code, engine.paused, engine.active)
			}
		})
	}
}
