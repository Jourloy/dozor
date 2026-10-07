package dozor

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRTSPConnectionDiagnostics(t *testing.T) {
	if os.Getenv("DOZOR_INTEGRATION") != "1" {
		t.Skip("set DOZOR_INTEGRATION=1 to test connection errors with real MediaMTX")
	}
	mtx := os.Getenv("DOZOR_MEDIAMTX")
	if mtx == "" {
		t.Fatal("DOZOR_MEDIAMTX is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	freeAddress := func() string {
		listener, err := net.Listen("tcp4", "127.0.0.1:0")
		must(t, err)
		address := listener.Addr().String()
		must(t, listener.Close())
		return address
	}
	address, unavailable := freeAddress(), freeAddress()
	dir := t.TempDir()
	config := filepath.Join(dir, "source.json")
	must(t, WriteJSON(config, map[string]any{
		"logLevel": "error", "rtspAddress": address, "rtspTransports": []string{"tcp"},
		"moq": false, "rtmp": false, "hls": false, "webrtc": false, "srt": false,
		"authInternalUsers": []any{map[string]any{"user": "viewer", "pass": "correct-password", "permissions": []any{map[string]any{"action": "read"}}}},
		"paths":             map[string]any{"test": map[string]any{"source": "publisher"}},
	}))
	source := exec.CommandContext(ctx, mtx, config)
	must(t, source.Start())
	defer func() { _ = source.Process.Kill(); _ = source.Wait() }()
	for deadline := time.Now().Add(5 * time.Second); ; {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("test camera did not start")
		}
		pause(ctx, 50*time.Millisecond)
	}
	cameras := []Camera{
		{ID: ID(), Name: "Wrong password", URL: "rtsp://" + address + "/test", Username: "viewer", Password: "wrong-password", Enabled: true},
		{ID: ID(), Name: "Unavailable", URL: "rtsp://" + unavailable + "/test", Enabled: true},
	}
	s := testStore(t)
	r := &Runtime{Store: s}
	done := make(chan struct{})
	go func() {
		defer close(done)
		RunMedia(ctx, Config{Archive: s.Root, Cameras: cameras}, Binaries{MediaMTX: mtx, Self: os.Getenv("DOZOR_BINARY")}, filepath.Join(dir, "control.sock"), filepath.Join(dir, "recorder.json"), s.Notice, func(bool) {}, func(camera Camera, message string) {
			r.reportCameraFailure(camera, "connection", message, time.Now())
		})
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("recorder did not stop")
		}
	}()
	for ctx.Err() == nil {
		first, err := s.LastCameraFailure(cameras[0].ID, "connection")
		must(t, err)
		second, err := s.LastCameraFailure(cameras[1].ID, "connection")
		must(t, err)
		if first != nil && second != nil {
			if !strings.Contains(first.Message, "401") || !strings.Contains(second.Message, "connection refused") {
				t.Fatalf("incorrect per-camera diagnostics: %+v %+v", first, second)
			}
			if strings.Contains(first.Message, "viewer") || strings.Contains(first.Message, "wrong-password") {
				t.Fatal("camera credentials leaked", first.Message)
			}
			return
		}
		pause(ctx, 50*time.Millisecond)
	}
	t.Fatal("failed connections produced no persisted diagnostics", s.Notices())
}
