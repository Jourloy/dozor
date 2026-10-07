package dozor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveAssemblyDiagnostics(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe", "missing ffmpeg"} {
		t.Run(tool, func(t *testing.T) {
			s := testStore(t)
			bins := emptyVideoBinaries(t)
			failedBin := bins.FFmpeg
			if tool == "ffprobe" {
				failedBin = bins.FFprobe
			}
			if tool == "missing ffmpeg" {
				must(t, os.Remove(failedBin))
			} else {
				must(t, os.WriteFile(failedBin, []byte("#!/bin/sh\nprintf 'moov atom not found\\nInvalid data found\\n' >&2\nexit 1\n"), 0700))
			}
			now := time.Now().Truncate(time.Second)
			ev, seg := pendingRecording(t, s, ID(), now.Add(-time.Minute))
			r := &Runtime{Store: s, Engine: NewEngine(s, bins)}
			err := r.Engine.Tick(context.Background(), now)
			if !errors.Is(err, errVideoAssembly) {
				t.Fatalf("lost retryable assembly error: %v", err)
			}
			r.reportArchiveError("Ошибка обработки архива", err, now)
			notices := s.Notices()
			if len(notices) != 1 {
				t.Fatalf("expected one diagnostic notice: %+v", notices)
			}
			want := []string{ev.CameraID, ev.ID, seg.Path, "повтор через 5 с"}
			if tool == "missing ffmpeg" {
				want = append(want, failedBin, "no such file or directory")
			} else {
				want = append(want, tool, "exit status 1", "moov atom not found Invalid data found")
			}
			for _, detail := range want {
				if !strings.Contains(notices[0].Message, detail) {
					t.Errorf("missing %q: %s", detail, notices[0].Message)
				}
			}
		})
	}
}

func TestArchiveAssemblyRetryBackoff(t *testing.T) {
	s := testStore(t)
	engine := NewEngine(s, Binaries{})
	now := time.Now().Truncate(time.Second)
	ev, seg := pendingRecording(t, s, ID(), now.Add(-time.Minute))
	attempts := 0
	engine.assemble = func(context.Context, *Store, Binaries, Event, []Segment) (Part, error) {
		attempts++
		return Part{}, errVideoAssembly
	}
	for i, seconds := range []int{5, 10, 20, 40, 60, 60} {
		err := engine.Tick(context.Background(), now)
		if !errors.Is(err, errVideoAssembly) || !strings.Contains(err.Error(), fmt.Sprintf("повтор через %d с", seconds)) {
			t.Fatalf("attempt %d: %v", i+1, err)
		}
		delay := time.Duration(seconds) * time.Second
		must(t, engine.Tick(context.Background(), now.Add(delay-time.Second)))
		if attempts != i+1 {
			t.Fatalf("retried before backoff elapsed: %d", attempts)
		}
		actual, err := s.Event(ev.ID)
		must(t, err)
		if actual.Status != "closing" || actual.Cursor != ev.Cursor || !s.HasSegment(seg.Path) {
			t.Fatal("retry discarded unfinished video", actual)
		}
		_, err = os.Stat(filepath.Join(s.Root, seg.Path))
		must(t, err)
		now = now.Add(delay + time.Second)
	}
}

func TestArchiveAssemblyRetryResetsAfterProgress(t *testing.T) {
	s := testStore(t)
	engine := NewEngine(s, Binaries{})
	now := time.Now().Truncate(time.Second)
	ev := Event{ID: ID(), CameraID: ID(), Start: now.Add(-3 * time.Minute).UnixMilli(), End: now.Add(-time.Minute).UnixMilli(), Status: "closing"}
	ev.Cursor = ev.Start
	must(t, s.SaveEvent(ev))
	for i := range 2 {
		start := ev.Start + int64(i)*60000
		must(t, s.AddSegment(Segment{Path: fmt.Sprintf("buffer/%s/%d.mp4", ev.CameraID, start), CameraID: ev.CameraID, Start: start, End: start + 60000}))
	}
	attempts := 0
	engine.assemble = func(_ context.Context, s *Store, _ Binaries, ev Event, segs []Segment) (Part, error) {
		attempts++
		if attempts != 3 {
			return Part{}, fmt.Errorf("%w: %w", errVideoAssembly, errNoVideo)
		}
		return fixturePart(t, s, ev, segs[0].Start, segs[len(segs)-1].End), nil
	}
	for _, offset := range []time.Duration{0, 6 * time.Second} {
		if err := engine.Tick(context.Background(), now.Add(offset)); !errors.Is(err, errVideoAssembly) {
			t.Fatal(err)
		}
	}
	err := engine.Tick(context.Background(), now.Add(17*time.Second))
	if attempts != 4 || err == nil || !strings.Contains(err.Error(), "повтор через 5 с") {
		t.Fatalf("successful part did not reset the retry delay: attempts=%d err=%v", attempts, err)
	}
	actual, err := s.Event(ev.ID)
	must(t, err)
	if actual.Cursor != ev.Start+60000 || actual.Status != "closing" || actual.AssemblyFailures != 1 {
		t.Fatal("completed part progress was not preserved", actual)
	}
}

func TestArchiveErrorNoticeThrottling(t *testing.T) {
	var output bytes.Buffer
	previous := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previous) })
	s := testStore(t)
	r := &Runtime{Store: s}
	now := time.Now()
	failure := errors.New("очистка буфера: permission denied")
	for second := range 61 {
		r.reportArchiveError("Ошибка обработки архива", failure, now.Add(time.Duration(second)*time.Second))
	}
	if len(s.Notices()) != 2 || strings.Count(output.String(), failure.Error()) != 2 {
		t.Fatal("identical errors should be reported only once per minute", s.Notices(), output.String())
	}
	r.reportArchiveError("Ошибка обработки архива", errors.New("database is locked"), now.Add(61*time.Second))
	r.reportArchiveError("Ошибка обработки архива", context.Canceled, now.Add(62*time.Second))
	if len(s.Notices()) != 3 {
		t.Fatal("changed errors must appear immediately; shutdown cancellation must be silent", s.Notices())
	}
	must(t, s.Close())
	r.reportArchiveError("Ошибка обработки архива", errors.New("catalog unavailable"), now.Add(63*time.Second))
	if !strings.Contains(output.String(), "catalog unavailable") {
		t.Fatal("catalog failure hid diagnostics from the service log")
	}
}

func TestLocalProbeDiagnosticsAreBounded(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ffprobe")
	script := "#!/bin/sh\nprintf '%s' '" + strings.Repeat("invalid frame\n", 1000) + "' >&2\n"
	must(t, os.WriteFile(bin, []byte(script+"exit 1\n"), 0700))
	_, err := Probe(context.Background(), bin, "recording.mp4", false)
	if err == nil || !strings.Contains(err.Error(), "ffprobe: exit status 1: invalid frame") || !strings.HasSuffix(err.Error(), "…") || len(err.Error()) > diagnosticLimit+100 || strings.Contains(err.Error(), "\n") {
		t.Fatalf("expected a bounded single-line diagnostic: %v", err)
	}
	// Excess stderr must be drained without breaking an otherwise successful probe.
	must(t, os.WriteFile(bin, []byte(script+"printf '%s' '{\"streams\":[{\"codec_type\":\"video\",\"codec_name\":\"h264\"}],\"format\":{\"duration\":\"5\"}}'\n"), 0700))
	result, err := Probe(context.Background(), bin, "recording.mp4", false)
	must(t, err)
	if result.Video != "h264" || result.Duration != 5 {
		t.Fatal(result)
	}
}

func TestProbeDiagnosticsDoNotExposeCameraCredentials(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "ffprobe")
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\nprintf 'rtsp://admin:secret@camera: unauthorized\\n' >&2\nexit 1\n"), 0700))
	_, err := Probe(context.Background(), bin, "rtsp://admin:secret@camera", true)
	if err == nil || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "admin") {
		t.Fatalf("camera credentials exposed: %v", err)
	}
}
