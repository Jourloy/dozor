package dozor

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestMP4AssemblyWithRealFFmpeg(t *testing.T) {
	ff, e := exec.LookPath("ffmpeg")
	if e != nil {
		t.Skip("ffmpeg not installed")
	}
	probe, e := exec.LookPath("ffprobe")
	if e != nil {
		t.Skip("ffprobe not installed")
	}
	s := testStore(t)
	cam := ID()
	now := time.Now().UTC().Truncate(time.Second)
	dir := filepath.Join(s.Root, "buffer", cam)
	must(t, os.MkdirAll(dir, 0700))
	input := filepath.Join(dir, now.Format("2006-01-02_15-04-05.000000")+".mp4")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, e := exec.CommandContext(ctx, ff, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=5", "-t", "2", "-c:v", "mpeg4", "-movflags", "frag_keyframe+empty_moov", input).CombinedOutput()
	if e != nil {
		t.Fatalf("%v %s", e, out)
	}
	seg, e := ParseSegment(s.Root, input, 2)
	must(t, e)
	must(t, s.AddSegment(seg))
	ev := Event{ID: ID(), CameraID: cam, Start: seg.Start, End: seg.End, Status: "closing"}
	must(t, s.SaveEvent(ev))
	p, e := Assemble(ctx, s, Binaries{FFmpeg: ff, FFprobe: probe}, ev, []Segment{seg})
	must(t, e)
	result, e := Probe(ctx, probe, filepath.Join(s.Root, p.Path), false)
	must(t, e)
	if result.Duration < 1.8 || result.Video != "mpeg4" || s.QueueCount() != 1 {
		t.Fatalf("%+v", result)
	}
}
func TestRTSPPipeline(t *testing.T) {
	if os.Getenv("DOZOR_INTEGRATION") != "1" {
		t.Skip("set DOZOR_INTEGRATION=1 for the ~2 minute RTSP pipeline")
	}
	mtx := os.Getenv("DOZOR_MEDIAMTX")
	self := os.Getenv("DOZOR_BINARY")
	if mtx == "" || self == "" {
		t.Fatal("DOZOR_MEDIAMTX and DOZOR_BINARY are required")
	}
	ff, e := exec.LookPath("ffmpeg")
	must(t, e)
	probe, e := exec.LookPath("ffprobe")
	must(t, e)
	dir, e := os.MkdirTemp("/tmp", "dz-e2e-")
	must(t, e)
	defer os.RemoveAll(dir)
	archive := filepath.Join(dir, "archive")
	must(t, os.MkdirAll(archive, 0700))
	cfg, e := LoadConfig(filepath.Join(dir, "config.json"))
	must(t, e)
	c := cfg.Get()
	c.Archive = archive
	cam := Camera{ID: ID(), Name: "Test camera", URL: "rtsp://127.0.0.1:18554/test", SubURL: "rtsp://127.0.0.1:18554/test", Motion: "local", Sensitivity: .8, Enabled: true}
	c.Cameras = []Camera{cam}
	var objects sync.Map
	cloud := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ") {
			w.WriteHeader(403)
			return
		}
		switch r.Method {
		case "PUT":
			b, err := io.ReadAll(r.Body)
			md := md5.Sum(b)
			sha := sha256.Sum256(b)
			if err != nil || base64.StdEncoding.EncodeToString(md[:]) != r.Header.Get("Content-MD5") || hex.EncodeToString(sha[:]) != r.Header.Get("X-Amz-Meta-Sha256") {
				w.WriteHeader(400)
				return
			}
			objects.Store(r.URL.Path, b)
		case "HEAD":
			v, ok := objects.Load(r.URL.Path)
			if !ok {
				w.WriteHeader(404)
				return
			}
			b := v.([]byte)
			sha := sha256.Sum256(b)
			w.Header().Set("Content-Length", itoa(len(b)))
			w.Header().Set("X-Amz-Meta-Sha256", hex.EncodeToString(sha[:]))
		default:
			w.WriteHeader(405)
		}
	}))
	defer cloud.Close()
	c.S3 = S3Config{Enabled: true, Endpoint: cloud.URL, Region: "us-east-1", Bucket: "archive", Prefix: "pipeline", AccessKey: "fixture", SecretKey: "fixture", PathStyle: true}
	must(t, cfg.Save(c))
	ctx, cancel := context.WithTimeout(context.Background(), 155*time.Second)
	defer cancel()
	sourceConfig := filepath.Join(dir, "source.yml")
	must(t, os.WriteFile(sourceConfig, []byte("logLevel: error\nrtspAddress: 127.0.0.1:18554\nrtspTransports: [tcp]\nmoq: false\nrtmp: false\nhls: false\nwebrtc: false\nsrt: false\npaths:\n  test:\n    source: publisher\n"), 0600))
	source := exec.CommandContext(ctx, mtx, sourceConfig)
	var sourceLog limitedBuffer
	sourceLog.limit = 1 << 20
	source.Stderr = &sourceLog
	source.Stdout = &sourceLog
	must(t, source.Start())
	defer func() { source.Process.Kill(); source.Wait() }()
	pause(ctx, time.Second)
	// Keep every phase on one input clock: separate -re inputs can catch up
	// in a burst when concat switches to them, ending the RTSP stream early.
	video := "testsrc2=s=320x180:r=5:d=136,drawbox=x=0:y=0:w=iw:h=ih:color=gray:t=fill:enable='lt(t,72)+gte(t,76)'"
	publisher := exec.CommandContext(ctx, ff, "-nostdin", "-v", "error", "-re", "-f", "lavfi", "-i", video, "-c:v", "libx264", "-preset", "ultrafast", "-tune", "zerolatency", "-pix_fmt", "yuv420p", "-g", "5", "-f", "rtsp", "-rtsp_transport", "tcp", cam.URL)
	var publisherLog limitedBuffer
	publisherLog.limit = 1 << 20
	publisher.Stderr = &publisherLog
	must(t, publisher.Start())
	defer func() { publisher.Process.Kill(); publisher.Wait() }()
	app, e := NewApp(cfg, Binaries{mtx, ff, probe, self}, true, filepath.Join(dir, "state"), filepath.Join(dir, "control.sock"), "test")
	must(t, e)
	done := make(chan error, 1)
	go func() { done <- app.Run(ctx) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(25 * time.Second):
			t.Error("application did not shut down")
		}
	}()
	iterations := 0
	for ctx.Err() == nil {
		pause(ctx, 2*time.Second)
		iterations++
		if iterations%10 == 0 {
			paths, _ := filepath.Glob(filepath.Join(archive, "buffer", cam.ID, "*.mp4"))
			t.Logf("buffer files=%d status=%v", len(paths), app.Status())
			if len(paths) > 1 {
				p, err := Probe(ctx, probe, paths[0], false)
				seg, parseErr := ParseSegment(archive, paths[0], p.Duration)
				t.Logf("probe=%+v err=%v segment=%+v parse=%v", p, err, seg, parseErr)
			}
		}
		app.mu.RLock()
		r := app.runtime
		if r != nil {
			events, err := r.Store.Events(cam.ID, 20)
			if err == nil {
				for _, ev := range events {
					if ev.Source == "local" && ev.Status == "closed" && ev.Uploaded {
						parts, err := r.Store.Parts(ev.ID)
						if err != nil {
							app.mu.RUnlock()
							t.Fatal(err)
						}
						if ev.ShortPrebuffer || len(parts) < 2 {
							app.mu.RUnlock()
							t.Fatalf("missing pre-roll: %+v parts=%d", ev, len(parts))
						}
						for _, p := range parts {
							if _, ok := objects.Load("/archive/pipeline/" + filepath.ToSlash(p.Path)); !ok || !p.Uploaded {
								app.mu.RUnlock()
								t.Fatal("video missing from S3")
							}
							if _, err = Probe(ctx, probe, filepath.Join(archive, p.Path), false); err != nil {
								app.mu.RUnlock()
								t.Fatal(err)
							}
						}
						app.mu.RUnlock()
						if _, ok := objects.Load("/archive/pipeline/" + filepath.ToSlash(filepath.Join(eventDir(ev), "manifest.json"))); !ok {
							t.Fatal("manifest missing from S3")
						}
						t.Logf("RTSP → buffer → event → %d playable MP4 parts → S3 + manifest", len(parts))
						return
					}
				}
			}
		}
		app.mu.RUnlock()
	}
	t.Fatalf("pipeline timeout; source=%s publisher=%s status=%v", sourceLog.data, publisherLog.data, app.Status())
}
