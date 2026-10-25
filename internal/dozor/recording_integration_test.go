package dozor

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// Exercise the real MediaMTX API and recorder: two source patches must actually
// restart one existing pull, preserve its closed tail, and leave its peer alive.
func TestRTSPRecordingRecovery(t *testing.T) {
	if os.Getenv("DOZOR_INTEGRATION") != "1" {
		t.Skip("set DOZOR_INTEGRATION=1 to test recording recovery with real MediaMTX")
	}
	mtx := os.Getenv("DOZOR_MEDIAMTX")
	if mtx == "" {
		t.Fatal("DOZOR_MEDIAMTX is required")
	}
	ff, err := exec.LookPath("ffmpeg")
	must(t, err)
	probe, err := exec.LookPath("ffprobe")
	must(t, err)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	must(t, err)
	rtsp := listener.Addr().String()
	must(t, listener.Close())
	control, err := newMediaControl()
	must(t, err)
	defer control.client.CloseIdleConnections()
	s := testStore(t)
	camera := Camera{ID: ID(), Name: "Recovery", URL: "rtsp://" + rtsp + "/synthetic", Enabled: true}
	peer := Camera{ID: ID(), Name: "Peer", URL: camera.URL, Enabled: true}
	data, err := mediaConfig(Config{Archive: s.Root, Cameras: []Camera{camera, peer}}, Binaries{Self: "/usr/bin/true"}, "/tmp/unused.sock", control)
	must(t, err)
	var config map[string]any
	must(t, json.Unmarshal(data, &config))
	config["rtspAddress"], config["hls"], config["logLevel"] = rtsp, false, "info"
	config["paths"].(map[string]any)["synthetic"] = map[string]any{"source": "publisher", "record": false}
	reader := config["authInternalUsers"].([]any)[0].(map[string]any)
	reader["permissions"] = append(reader["permissions"].([]any), map[string]any{"action": "publish", "path": "synthetic"})
	path := filepath.Join(t.TempDir(), "mediamtx.json")
	must(t, WriteJSON(path, config))
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	start := func(command *exec.Cmd) {
		command.Env = append(os.Environ(), "TZ=UTC")
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
	waitFor := func(description string, condition func() bool) {
		t.Helper()
		for ctx.Err() == nil {
			if condition() {
				return
			}
			pause(ctx, 100*time.Millisecond)
		}
		t.Fatal("timed out: " + description)
	}
	waitFor("API listener", func() bool {
		conn, err := net.DialTimeout("tcp", control.address, 100*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	})
	res, err := control.client.Get("http://" + control.address + "/v3/paths/list")
	must(t, err)
	res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous API access: HTTP %d", res.StatusCode)
	}
	start(exec.CommandContext(ctx, ff, "-nostdin", "-v", "error", "-re", "-f", "lavfi", "-i", "testsrc2=size=320x180:rate=10", "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-g", "10", "-f", "rtsp", "-rtsp_transport", "tcp", camera.URL))
	readyTime := func(id string) string {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+control.address+"/v3/paths/get/"+id, nil)
		must(t, err)
		req.SetBasicAuth("dozor-recorder", control.password)
		res, err := control.client.Do(req)
		if err != nil {
			return ""
		}
		defer res.Body.Close()
		var state struct {
			Ready     bool   `json:"ready"`
			ReadyTime string `json:"readyTime"`
		}
		if json.NewDecoder(res.Body).Decode(&state) != nil || !state.Ready {
			return ""
		}
		return state.ReadyTime
	}
	segments := func(id string) []Segment {
		must(t, ScanSegments(ctx, s, probe, true))
		v, err := s.Segments(id, 0, time.Now().Add(time.Minute).UnixMilli())
		must(t, err)
		return v
	}
	waitFor("recorded fragments from both cameras", func() bool {
		return len(segments(camera.ID)) > 0 && len(segments(peer.ID)) > 0
	})
	original, peerOriginal := readyTime(camera.ID), readyTime(peer.ID)
	if original == "" || peerOriginal == "" {
		t.Fatal("paths not ready")
	}
	before, peerBefore := segments(camera.ID), segments(peer.ID)
	// Leave a real partial tail for the reconnect to close and recover.
	pause(ctx, 1500*time.Millisecond)
	priorFiles, err := filepath.Glob(filepath.Join(s.Root, "buffer", camera.ID, "*.mp4"))
	must(t, err)
	r := &Runtime{Store: s, mediaControl: control, bufferWake: make(chan struct{}, 1)}
	r.mediaSince.Store(time.Now().Add(-time.Minute).Unix())
	r.workerReady.Store(true)
	watch := &recordingWatch{}
	probeFile := func(ctx context.Context, path string) (ProbeResult, error) { return Probe(ctx, probe, path, false) }
	r.checkRecording(ctx, camera, watch, time.Now(), probeFile)
	// Simulate the watchdog's elapsed deadline without pausing the synthetic
	// source. This exercises reconnecting a path whose URL has not changed.
	watch.lastProgress = time.Now().Add(-recordingStallTimeout)
	r.checkRecording(ctx, camera, watch, time.Now(), probeFile)
	if !watch.failed || watch.restore {
		t.Fatalf("reconnect failed or reported recovery without a fragment: %+v", watch)
	}
	waitFor("new session and usable fragment", func() bool {
		if readyTime(peer.ID) != peerOriginal {
			t.Fatal("restarting one camera interrupted its peer")
		}
		v := segments(camera.ID)
		r.checkRecording(ctx, camera, watch, time.Now(), probeFile)
		return readyTime(camera.ID) != "" && readyTime(camera.ID) != original && len(v) > len(before) && !watch.failed
	})
	waitFor("peer kept recording", func() bool { return len(segments(peer.ID)) > len(peerBefore) })
	for _, path := range priorFiles {
		p, err := Probe(ctx, probe, path, false)
		must(t, err)
		if p.Duration <= 0 {
			t.Fatal("existing recording was lost")
		}
	}
	t.Log("reconnected one camera, verified fresh playable video, kept its prior fragments and uninterrupted peer session")
}
