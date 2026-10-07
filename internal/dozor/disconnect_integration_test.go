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

func TestRTSPDisconnect(t *testing.T) {
	if os.Getenv("DOZOR_INTEGRATION") != "1" {
		t.Skip("set DOZOR_INTEGRATION=1 to test a camera disconnection with real MediaMTX")
	}
	mtx, self := os.Getenv("DOZOR_MEDIAMTX"), os.Getenv("DOZOR_BINARY")
	if mtx == "" || self == "" {
		t.Fatal("DOZOR_MEDIAMTX and DOZOR_BINARY are required")
	}
	ff, err := exec.LookPath("ffmpeg")
	must(t, err)
	probe, err := exec.LookPath("ffprobe")
	must(t, err)
	dir, err := os.MkdirTemp("/tmp", "dz-disconnect-")
	must(t, err)
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	must(t, err)
	address := listener.Addr().String()
	must(t, listener.Close())
	sourceConfig := filepath.Join(dir, "source.json")
	must(t, WriteJSON(sourceConfig, map[string]any{"logLevel": "error", "rtspAddress": address, "rtspTransports": []string{"tcp"}, "moq": false, "rtmp": false, "hls": false, "webrtc": false, "srt": false, "paths": map[string]any{"test": map[string]any{"source": "publisher"}}}))
	source := exec.CommandContext(ctx, mtx, sourceConfig)
	must(t, source.Start())
	defer func() { _ = source.Process.Kill(); _ = source.Wait() }()
	for deadline := time.Now().Add(5 * time.Second); ; {
		conn, err := net.DialTimeout("tcp", address, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("source did not start")
		}
		pause(ctx, 100*time.Millisecond)
	}
	url := "rtsp://" + address + "/test"
	publisher := exec.CommandContext(ctx, ff, "-nostdin", "-v", "error", "-re", "-f", "lavfi", "-i", "testsrc2=s=320x180:r=10", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-g", "10", "-f", "rtsp", "-rtsp_transport", "tcp", url)
	must(t, publisher.Start())
	defer func() { _ = publisher.Process.Kill(); _ = publisher.Wait() }()
	cf, err := LoadConfig(filepath.Join(dir, "config.json"))
	must(t, err)
	c := cf.Get()
	c.Archive = filepath.Join(dir, "archive")
	must(t, os.MkdirAll(c.Archive, 0700))
	cam := Camera{ID: ID(), Name: "Test", URL: url, SubURL: "rtsp://" + address + "/missing", Enabled: true, Motion: "local", Sensitivity: .7}
	c.Cameras = []Camera{cam} // Unavailable detector: continuous fallback while the main stream is online.
	must(t, cf.Save(c))
	a, err := NewApp(cf, Binaries{mtx, ff, probe, self}, true, filepath.Join(dir, "state"), filepath.Join(dir, "control.sock"), "test")
	must(t, err)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(25 * time.Second):
			t.Error("application did not stop")
		}
	}()
	var eventID string
	for ctx.Err() == nil {
		pause(ctx, 100*time.Millisecond)
		a.mu.RLock()
		if r := a.runtime; r != nil {
			events, err := r.Store.Events(cam.ID, 1)
			if err == nil && len(events) > 0 {
				segments, err := r.Store.Segments(cam.ID, 0, time.Now().UnixMilli())
				if err == nil && len(segments) > 0 {
					eventID = events[0].ID
				}
			}
		}
		a.mu.RUnlock()
		if eventID != "" {
			break
		}
	}
	if eventID == "" {
		t.Fatal("recording never started")
	}
	// Leave a partial five-second recorder segment before abruptly losing RTSP.
	pause(ctx, 1500*time.Millisecond)
	disconnected := time.Now()
	must(t, publisher.Process.Kill())
	for ctx.Err() == nil && time.Since(disconnected) < 12*time.Second {
		pause(ctx, 100*time.Millisecond)
		a.mu.RLock()
		r := a.runtime
		if r == nil {
			a.mu.RUnlock()
			continue
		}
		ev, err := r.Store.Event(eventID)
		if err != nil || ev.Status != "closed" || ev.DisconnectedAt == 0 {
			a.mu.RUnlock()
			continue
		}
		parts, err := r.Store.Parts(ev.ID)
		state := r.streams[cam.ID].State
		root := r.Store.Root
		h, historyErr := r.Store.Availability(c.Cameras, "", time.Now())
		connectionError, diagnosticErr := r.Store.LastCameraFailure(cam.ID, "connection")
		detectorError, detectorErr := r.Store.LastCameraFailure(cam.ID, "detector")
		a.mu.RUnlock()
		must(t, err)
		must(t, historyErr)
		must(t, diagnosticErr)
		must(t, detectorErr)
		if connectionError == nil || connectionError.At < disconnected.UnixMilli() || !strings.HasPrefix(connectionError.Message, "RTSP:") {
			t.Fatal("disconnect lost its RTSP cause", connectionError)
		}
		if detectorError == nil || !strings.Contains(detectorError.Message, "Server returned") {
			t.Fatal("detector failure lost its separate cause", detectorError)
		}
		if len(parts) == 0 || parts[len(parts)-1].DisconnectedAt == 0 || state != "offline" {
			t.Fatalf("unmarked/discarded tail: %+v %+v %s", ev, parts, state)
		}
		p, err := Probe(ctx, probe, filepath.Join(root, parts[len(parts)-1].Path), false)
		must(t, err)
		if p.Duration < 5 || p.Duration >= 60 {
			t.Fatalf("short tail is not playable: %+v", p)
		}
		foundOnline, foundOffline := false, false
		for _, v := range h.Cameras[0].Intervals {
			foundOnline = foundOnline || v.State == "online"
			foundOffline = foundOffline || v.State == "offline" && v.Start >= ev.DisconnectedAt
		}
		if !foundOnline || !foundOffline {
			t.Fatal("missing online/offline history", h)
		}
		t.Logf("saved %.1fs MP4 %.1fs after disconnect, marked final part and persisted availability", p.Duration, time.Since(disconnected).Seconds())
		return
	}
	t.Fatal("disconnected recording did not close promptly", a.Status())
}
