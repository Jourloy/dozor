package dozor

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestLiveRTSPStream(t *testing.T) {
	if os.Getenv("DOZOR_INTEGRATION") != "1" {
		t.Skip("set DOZOR_INTEGRATION=1 to decode real RTSP through the authenticated HLS proxy")
	}
	mtx := os.Getenv("DOZOR_MEDIAMTX")
	if mtx == "" {
		t.Fatal("DOZOR_MEDIAMTX is required")
	}
	ff, err := exec.LookPath("ffmpeg")
	must(t, err)
	freeAddress := func() string {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		must(t, err)
		address := listener.Addr().String()
		must(t, listener.Close())
		return address
	}
	rtsp, hls := freeAddress(), freeAddress()
	a, camera := liveTestApp()
	c := a.Config.Get()
	c.Archive = t.TempDir()
	data, err := MediaConfig(c, Binaries{Self: "/usr/bin/true"}, "/tmp/unused.sock")
	must(t, err)
	var config map[string]any
	must(t, json.Unmarshal(data, &config))
	config["rtspAddress"] = rtsp
	config["hlsAddress"] = hls
	config["logLevel"] = "info"
	// Publish a synthetic camera directly, keeping the production HLS and
	// recording configuration. The longer pipeline test covers RTSP pulling.
	config["paths"].(map[string]any)[camera.ID].(map[string]any)["source"] = "publisher"
	user := config["authInternalUsers"].([]any)[0].(map[string]any)
	user["permissions"] = append(user["permissions"].([]any), map[string]any{"action": "publish"})
	path := filepath.Join(t.TempDir(), "mediamtx.json")
	must(t, WriteJSON(path, config))
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	start := func(command *exec.Cmd) {
		logPath := filepath.Join(t.TempDir(), "process.log")
		log, err := os.Create(logPath)
		must(t, err)
		command.Stdout, command.Stderr = log, log
		must(t, command.Start())
		t.Cleanup(func() {
			_ = command.Process.Kill()
			_ = command.Wait()
			_ = log.Close()
			if t.Failed() {
				data, _ := os.ReadFile(logPath)
				t.Log(string(data))
			}
		})
	}
	start(exec.CommandContext(ctx, mtx, path))
	ready := false
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		conn, err := net.DialTimeout("tcp", rtsp, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			ready = true
			break
		}
		pause(ctx, 100*time.Millisecond)
	}
	if !ready {
		t.Fatal("MediaMTX did not start")
	}
	start(exec.CommandContext(ctx, ff, "-nostdin", "-v", "error", "-re", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-g", "10", "-c:a", "aac", "-f", "rtsp", "-rtsp_transport", "tcp", "rtsp://"+rtsp+"/"+camera.ID))
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, hls)
	}}
	defer transport.CloseIdleConnections()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v1/cameras/{id}/live/{file}", a.protected(a.liveHandler(transport)))
	server := httptest.NewServer(mux)
	defer server.Close()
	liveURL := server.URL + "/api/v1/cameras/" + camera.ID + "/live/index.m3u8"
	ready = false
	for deadline := time.Now().Add(20 * time.Second); time.Now().Before(deadline) && ctx.Err() == nil; {
		r, err := http.NewRequestWithContext(ctx, "GET", liveURL, nil)
		must(t, err)
		r.AddCookie(&http.Cookie{Name: "dozor_session", Value: "test"})
		response, err := server.Client().Do(r)
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				ready = true
				break
			}
		}
		pause(ctx, 200*time.Millisecond)
	}
	if !ready {
		t.Fatal("HLS did not become ready")
	}
	// Decode video, not just a playlist: follows the relative playlist, init
	// and segment URLs and proves every resource passes through authentication.
	out, err := exec.CommandContext(ctx, ff, "-nostdin", "-v", "error", "-headers", "Cookie: dozor_session=test\r\n", "-i", liveURL, "-map", "0:v:0", "-map", "0:a:0", "-t", "2", "-f", "null", "-").CombinedOutput()
	if err != nil {
		t.Fatalf("could not decode live video: %v\n%s", err, out)
	}
	files, err := filepath.Glob(filepath.Join(c.Archive, "buffer", camera.ID, "*.mp4"))
	must(t, err)
	if len(files) == 0 {
		t.Fatal("recording did not continue alongside live viewing")
	}
}
