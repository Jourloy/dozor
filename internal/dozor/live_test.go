package dozor

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type liveRoundTripper func(*http.Request) (*http.Response, error)

func (f liveRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func liveTestApp() (*App, Camera) {
	camera := Camera{ID: ID(), Name: "Двор", URL: "rtsp://camera/main", Enabled: true}
	a := &App{
		Config:   &ConfigFile{value: Config{Cameras: []Camera{camera}}},
		runtime:  &Runtime{},
		sessions: map[string]Session{"test": {Expires: time.Now().Add(time.Hour)}},
	}
	a.runtime.mediaSince.Store(time.Now().Unix())
	return a, camera
}

func TestLiveAccess(t *testing.T) {
	for _, tc := range []struct {
		name, id, file string
		status         int
		setup          func(*App)
	}{
		{name: "unauthenticated", status: 401, setup: func(a *App) { a.sessions = nil }},
		{name: "expired", status: 401, setup: func(a *App) { a.sessions["test"] = Session{Expires: time.Now().Add(-time.Second)} }},
		{name: "unknown", id: ID(), status: 404},
		{name: "invalid id", id: "other", status: 404},
		{name: "encoded traversal", file: "%2e%2e%2findex.m3u8", status: 404},
		{name: "upstream player", file: "index.html", status: 404},
		{name: "invalid media session", file: "index.m3u8?session=invalid", status: 400},
		{name: "disabled", status: 409, setup: func(a *App) { a.Config.value.Cameras[0].Enabled = false }},
		{name: "no disk", status: 503, setup: func(a *App) { a.runtime = nil }},
		{name: "stopped recorder", status: 503, setup: func(a *App) { a.runtime.mediaSince.Store(0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a, camera := liveTestApp()
			if tc.setup != nil {
				tc.setup(a)
			}
			id, file := tc.id, tc.file
			if id == "" {
				id = camera.ID
			}
			if file == "" {
				file = "index.m3u8"
			}
			r := httptest.NewRequest("GET", "/api/v1/cameras/"+id+"/live/"+file, nil)
			r.AddCookie(&http.Cookie{Name: "dozor_session", Value: "test"})
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, r)
			if w.Code != tc.status || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestLiveProxy(t *testing.T) {
	for _, method := range []string{"GET", "HEAD"} {
		t.Run(method, func(t *testing.T) {
			a, camera := liveTestApp()
			const session = "0672db74-db00-4fbf-aee7-1a1945ea3e8a"
			transport := liveRoundTripper(func(r *http.Request) (*http.Response, error) {
				if r.Method != method || r.URL.String() != "http://"+liveAddress+"/"+camera.ID+"/video_seg1.mp4?cookieCheck=1&session="+session {
					t.Fatalf("unexpected upstream: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Cookie") != "" || r.Header.Get("Authorization") != "" || r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Range") != "bytes=0-2" {
					t.Fatalf("unsafe or missing headers: %v", r.Header)
				}
				if deadline, ok := r.Context().Deadline(); !ok || time.Until(deadline) > 30*time.Second {
					t.Fatal("upstream request has no bounded deadline")
				}
				if !a.mu.TryLock() {
					t.Fatal("streaming holds the application lock")
				}
				a.mu.Unlock()
				body := "mp4"
				if method == "HEAD" {
					body = ""
				}
				return &http.Response{StatusCode: 206, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{
					"Content-Type": {"video/mp4"}, "Content-Range": {"bytes 0-2/12"},
					"Access-Control-Allow-Origin": {"*"}, "Set-Cookie": {"leak=secret"}, "Cache-Control": {"public, max-age=3600"},
				}}, nil
			})
			mux := http.NewServeMux()
			mux.HandleFunc("GET /api/v1/cameras/{id}/live/{file}", a.protected(a.liveHandler(transport)))
			r := httptest.NewRequest(method, "/api/v1/cameras/"+camera.ID+"/live/video_seg1.mp4?url=http://elsewhere&session="+session, nil)
			r.AddCookie(&http.Cookie{Name: "dozor_session", Value: "test"})
			r.Header.Set("Range", "bytes=0-2")
			r.Header.Set("Authorization", "Bearer secret")
			r.Header.Set("X-Forwarded-For", "192.0.2.1")
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			if w.Code != 206 || w.Header().Get("Content-Range") != "bytes 0-2/12" || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("unexpected response: %d %v", w.Code, w.Header())
			}
			if w.Header().Get("Set-Cookie") != "" || w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal("upstream security headers escaped")
			}
			if method == "GET" && w.Body.String() != "mp4" {
				t.Fatal("media body changed")
			}
		})
	}
}

func TestLiveUpstreamFailure(t *testing.T) {
	for _, status := range []int{0, 302, 404, 500} {
		a, camera := liveTestApp()
		transport := liveRoundTripper(func(*http.Request) (*http.Response, error) {
			if status == 0 {
				return nil, errors.New("rtsp://admin:secret@camera")
			}
			return &http.Response{StatusCode: status, Header: http.Header{"Location": {"rtsp://admin:secret@camera"}}, Body: io.NopCloser(strings.NewReader("secret"))}, nil
		})
		r := httptest.NewRequest("GET", "/", nil)
		r.SetPathValue("id", camera.ID)
		r.SetPathValue("file", "index.m3u8")
		w := httptest.NewRecorder()
		a.liveHandler(transport)(w, r)
		if w.Code != 502 || strings.Contains(w.Body.String(), "secret") || w.Header().Get("Location") != "" {
			t.Fatalf("upstream %d leaked: %d %s", status, w.Code, w.Body.String())
		}
	}
}

func TestLiveMediaConfig(t *testing.T) {
	a, camera := liveTestApp()
	c := a.Config.Get()
	c.Cameras = append(c.Cameras, Camera{ID: ID(), Enabled: false})
	data, err := MediaConfig(c, Binaries{Self: "/bin/dozor"}, "/tmp/dozor.sock")
	must(t, err)
	var v map[string]any
	must(t, json.Unmarshal(data, &v))
	if v["hls"] != true || v["hlsAddress"] != liveAddress || v["hlsVariant"] != "fmp4" || v["hlsAlwaysRemux"] != false || v["hlsDirectory"] != "" {
		t.Fatalf("unsafe or incompatible live config: %s", data)
	}
	paths := v["paths"].(map[string]any)
	if len(paths) != 1 || paths[camera.ID].(map[string]any)["record"] != true {
		t.Fatal("live configuration changed recording or enabled a disabled camera")
	}
}
