package dozor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCameraFailuresSurviveJournalPruningAndRestart(t *testing.T) {
	s := testStore(t)
	cam := Camera{ID: ID(), Name: "Двор", URL: "rtsp://camera/main", Username: "admin", Password: "secret", Enabled: true}
	other := Camera{ID: ID(), Name: "Дорога", Enabled: true}
	now := time.Now().Truncate(time.Second)
	r := &Runtime{Store: s, Engine: NewEngine(s, Binaries{})}
	must(t, r.initStreams([]Camera{cam, other}, now.Add(-time.Minute)))
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	defer log.SetOutput(previous)

	for second := range 61 {
		r.reportCameraFailure(cam, "connection", "RTSP: rtsp://admin:secret@camera/main: 401 Unauthorized", now.Add(time.Duration(second)*time.Second))
	}
	if len(s.Notices()) != 2 || strings.Count(output.String(), "401 Unauthorized") != 2 {
		t.Fatal("identical connection retries flooded the journal", s.Notices(), output.String())
	}
	r.reportCameraFailure(cam, "detector", "ffmpeg: 404 Not Found", now.Add(61*time.Second))
	r.reportCameraFailure(other, "connection", "dial tcp: connection refused", now.Add(62*time.Second))
	connection, err := s.LastCameraFailure(cam.ID, "connection")
	must(t, err)
	if connection == nil || connection.At != now.Add(time.Minute).UnixMilli() || !strings.Contains(connection.Message, "401 Unauthorized") {
		t.Fatal("lost latest attempt or mixed cameras/detector with main stream", connection)
	}
	must(t, s.SaveCameraFailure(cam.ID, "connection", CameraFailure{At: now.UnixMilli(), Message: "old failure"}))
	must(t, r.setStream(context.Background(), cam.ID, true, now.Add(63*time.Second).UnixMilli()))
	r.mediaSince.Store(now.Unix())
	// Expiring journal entries must not expire camera diagnostics.
	_, err = s.DB.Exec("DELETE FROM notices")
	must(t, err)
	app := &App{Config: &ConfigFile{value: Config{Cameras: []Camera{cam, other}}}, runtime: r, StateDir: t.TempDir()}
	app.publishStatus(r)
	status := app.Status()
	camera := status["cameras"].([]any)[0].(map[string]any)
	if camera["online"] != true || camera["last_connection_error"].(*CameraFailure).Message != connection.Message || camera["last_detector_error"].(*CameraFailure).Message != "ffmpeg: 404 Not Found" {
		t.Fatal("status lost diagnostics or diagnostics changed online state", camera)
	}
	encoded, err := json.Marshal(status)
	must(t, err)
	for _, secret := range []string{"admin", "secret"} {
		if bytes.Contains(encoded, []byte(secret)) || strings.Contains(output.String(), secret) {
			t.Fatalf("credentials exposed in status or log: %s %s", encoded, output.String())
		}
	}
	root := s.Root
	must(t, s.Close())
	if failure, err := s.LastCameraFailure(cam.ID, "connection"); err == nil || failure != nil {
		t.Fatal("catalog failure returned a fabricated diagnostic", failure, err)
	}
	s, err = OpenStore(Guard{Root: root, Development: true})
	must(t, err)
	defer s.Close()
	r = &Runtime{Store: s, Engine: NewEngine(s, Binaries{})}
	must(t, r.initStreams([]Camera{cam, other}, now.Add(2*time.Minute)))
	actual, err := s.LastCameraFailure(cam.ID, "connection")
	must(t, err)
	if actual == nil || *actual != *connection {
		t.Fatal("restart cleared last connection error", actual)
	}
	missing, err := s.LastCameraFailure(other.ID, "detector")
	must(t, err)
	if missing != nil {
		t.Fatal("invented detector error", missing)
	}
}

func TestCameraDiagnosticsMigrateWithoutChangingAvailabilitySchema(t *testing.T) {
	s := testStore(t)
	_, err := s.DB.Exec("DROP TABLE camera_diagnostics")
	must(t, err)
	root := s.Root
	must(t, s.Close())
	s, err = OpenStore(Guard{Root: root, Development: true})
	must(t, err)
	defer s.Close()
	cam := ID()
	must(t, s.SaveCameraFailure(cam, "connection", CameraFailure{At: 123, Message: "connection refused"}))
	// Older releases use positional INSERTs; adding diagnostics must not break rollback.
	_, err = s.DB.Exec("INSERT INTO availability VALUES(?,?,?,?)", cam, 100, 200, "offline")
	must(t, err)
}

func TestMediaDiagnosticsAttributeOnlyRTSPSourceErrors(t *testing.T) {
	cam := Camera{ID: ID(), URL: "rtsp://camera/main", Username: "admin", Password: "p@ss:/?word", Enabled: true}
	disabled := Camera{ID: ID()}
	var failures []string
	var notices []string
	writer, global := mediaDiagnostics(Config{Cameras: []Camera{cam, disabled}}, func(message string) {
		notices = append(notices, message)
	}, func(camera Camera, message string) {
		if camera.ID != cam.ID {
			t.Error("error assigned to an unknown or disabled camera", camera.ID)
		}
		failures = append(failures, message)
	})
	lines := "2026/10/07 10:00:00 ERR [path " + cam.ID + "] [RTSP source] " + CameraURL(cam, false) + ": bad status code: 401 (Unauthorized)\n" +
		"2026/10/07 10:00:01 ERR [path " + disabled.ID + "] [RTSP source] ignored\n" +
		"2026/10/07 10:00:02 ERR [path " + ID() + "] [RTSP source] ignored\n" +
		"2026/10/07 10:00:03 ERR [path " + cam.ID + "] [recorder] disk full\n" +
		"ERR listen tcp 127.0.0.1:8554: bind: address already in use"
	// Exercise arbitrary writes and a final line without a newline.
	for _, part := range []string{lines[:31], lines[31:87], lines[87:]} {
		n, err := writer.Write([]byte(part))
		must(t, err)
		if n != len(part) {
			t.Fatal("did not drain process output")
		}
	}
	writer.Flush()
	if len(failures) != 1 || !strings.Contains(failures[0], "401 (Unauthorized)") || strings.Contains(failures[0], "admin") || strings.Contains(failures[0], "p@ss") || strings.Contains(failures[0], "p%40ss") {
		t.Fatal("missing or unsafe RTSP diagnostic", failures)
	}
	if len(notices) != 2 || !strings.Contains(string(global.data), "address already in use") || strings.Contains(string(global.data), "disk full") {
		t.Fatal("path error contaminated process failure", notices, string(global.data))
	}
}

func TestCameraDiagnosticsBoundAndRedactBeforeTruncation(t *testing.T) {
	input := "rtsp://admin:password%3Awith%40symbols@camera/main?token=query-secret"
	var output diagnosticBuffer
	writer := cameraCommandOutput(&output, input)
	line := strings.Repeat("x", diagnosticLimit-10) + " rtsp://admin:password%3Awith%40symbols@camera/main?token=query-secret\n"
	_, err := writer.Write([]byte(line))
	must(t, err)
	writer.Flush()
	message := cameraCommandError(context.Background(), "ffprobe", errors.New("exit status 1"), &output, input).Error()
	for _, secret := range []string{"admin", "password", "query-secret", "symbols"} {
		if strings.Contains(message, secret) {
			t.Fatalf("truncation exposed credential %q: %s", secret, message)
		}
	}
	if len(message) > diagnosticLimit+10 || !utf8.ValidString(message) {
		t.Fatal("unbounded or invalid UTF-8 diagnostic")
	}
	message = cameraDiagnostic("401 Unauthorized password:with@symbols password%3Awith%40symbols admin", input)
	if message != "401 Unauthorized [скрыто] [скрыто] [скрыто]" {
		t.Fatal("standalone credentials not redacted", message)
	}
	message = cameraDiagnostic("rtsp://user name:p a s s@camera/main 401 Unauthorized user name p a s s", "rtsp://user%20name:p%20a%20s%20s@camera/main")
	if message != "[адрес камеры] 401 Unauthorized [скрыто] [скрыто]" {
		t.Fatal("decoded whitespace exposed partial credentials", message)
	}
	message = cameraDiagnostic("[in#0 @ 0xdeadbeef] 401 Unauthorized")
	if message != "[in#0] 401 Unauthorized" {
		t.Fatal("unstable FFmpeg object address prevents retry deduplication", message)
	}
	var lines []string
	large := &diagnosticLines{emit: func(line string) { lines = append(lines, line) }}
	_, err = large.Write([]byte(strings.Repeat("secret", 20000)))
	must(t, err)
	_, err = large.Write([]byte("\n401 Unauthorized\nlast line"))
	must(t, err)
	large.Flush()
	if len(lines) != 3 || strings.Contains(lines[0], "secret") || lines[1] != "401 Unauthorized" || lines[2] != "last line" {
		t.Fatal("oversized line stopped draining or leaked credentials", lines)
	}
}

func TestCameraToolsKeepFailureDetailsAndCancellation(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "media-tool")
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' 'rtsp://admin:secret@camera/main: 401 Unauthorized' >&2\nexit 1\n"), 0700))
	camera := Camera{ID: ID(), URL: "rtsp://camera/main", SubURL: "rtsp://camera/sub", Username: "admin", Password: "secret"}
	for _, tool := range []string{"probe", "detector"} {
		t.Run(tool, func(t *testing.T) {
			var err error
			if tool == "probe" {
				_, err = Probe(context.Background(), bin, CameraURL(camera, false), true)
			} else {
				err = (LocalMotion{FFmpeg: bin}).Run(context.Background(), camera, func(MotionSignal) { t.Error("failed detector emitted a frame") })
			}
			if err == nil || !strings.Contains(err.Error(), "401 Unauthorized") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "admin") {
				t.Fatal("lost or unsafe camera tool error", err)
			}
		})
	}
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 5\n"), 0700))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := Probe(ctx, bin, CameraURL(camera, false), true)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatal("timeout lost its cause", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	err = (LocalMotion{FFmpeg: bin}).Run(ctx, camera, func(MotionSignal) {})
	if !errors.Is(err, context.Canceled) {
		t.Fatal("shutdown lost its cancellation", err)
	}
}

func TestMediaStartupFailureIdentifiesWhyAllCamerasAreOffline(t *testing.T) {
	cam := Camera{ID: ID(), Enabled: true}
	disabled := Camera{ID: ID()}
	s := testStore(t)
	r := &Runtime{Store: s}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	bin := filepath.Join(t.TempDir(), "missing-mediamtx")
	RunMedia(ctx, Config{Cameras: []Camera{cam, disabled}}, Binaries{MediaMTX: bin}, "", filepath.Join(t.TempDir(), "media.json"), s.Notice, func(ready bool) {
		if ready {
			t.Error("missing recorder marked ready")
		}
	}, func(camera Camera, message string) {
		r.reportCameraFailure(camera, "connection", message, time.Now())
		cancel()
	})
	failure, err := s.LastCameraFailure(cam.ID, "connection")
	must(t, err)
	if failure == nil || !strings.Contains(failure.Message, "no such file or directory") {
		t.Fatal("startup error was discarded", failure)
	}
	failure, err = s.LastCameraFailure(disabled.ID, "connection")
	must(t, err)
	if failure != nil {
		t.Fatal("disabled camera received a connection failure", failure)
	}
}
