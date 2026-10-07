package dozor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestArchiveDiscardsInvalidSegmentsAfterThreeFailures(t *testing.T) {
	for _, failure := range []string{"no video", "corrupt header", "missing input"} {
		t.Run(failure, func(t *testing.T) {
			s := testStore(t)
			bins := emptyVideoBinaries(t)
			now := time.Now().Truncate(time.Second)
			ev, seg := pendingRecording(t, s, ID(), now.Add(-30*time.Second))
			if failure == "corrupt header" {
				script := []byte("#!/bin/sh\nprintf 'moov atom not found\\nInvalid data found when processing input\\n' >&2\nexit 1\n")
				must(t, os.WriteFile(bins.FFmpeg, script, 0700))
				must(t, os.WriteFile(bins.FFprobe, script, 0700))
			} else if failure == "missing input" {
				must(t, os.Remove(filepath.Join(s.Root, seg.Path)))
				must(t, os.WriteFile(bins.FFmpeg, []byte("#!/bin/sh\nprintf 'Error opening input: No such file or directory\\n' >&2\nexit 1\n"), 0700))
			}
			engine := NewEngine(s, bins)
			for attempt, offset := range []time.Duration{0, 6 * time.Second, 17 * time.Second} {
				err := engine.Tick(context.Background(), now.Add(offset))
				actual, readErr := s.Event(ev.ID)
				must(t, readErr)
				if attempt < 2 {
					if !errors.Is(err, errVideoAssembly) || !s.HasSegment(seg.Path) || actual.Cursor != ev.Cursor || actual.AssemblyFailures != attempt+1 {
						t.Fatalf("attempt %d discarded video or lost its failure count: %+v, %v", attempt+1, actual, err)
					}
					if failure != "missing input" {
						_, err = os.Stat(filepath.Join(s.Root, seg.Path))
						must(t, err)
					}
					must(t, engine.Tick(context.Background(), now.Add(offset+time.Second)))
					continue
				}
				must(t, err)
				if s.HasSegment(seg.Path) || actual.Status != "closed" || !actual.Incomplete || actual.AssemblyFailures != 0 {
					t.Fatalf("third failure did not discard the bad video: %+v", actual)
				}
				if _, err = os.Stat(filepath.Join(s.Root, seg.Path)); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("bad file remains: %v", err)
				}
			}
			notices := s.Notices()
			if len(notices) != 1 {
				t.Fatalf("expected one deletion notice: %+v", notices)
			}
			for _, detail := range []string{"Удалён повреждённый фрагмент после 3 ошибок", ev.CameraID, ev.ID, seg.Path} {
				if !strings.Contains(notices[0].Message, detail) {
					t.Fatalf("missing deletion detail %q: %s", detail, notices[0].Message)
				}
			}
			must(t, s.Recover())
			must(t, ScanSegments(context.Background(), s, bins.FFprobe, false))
			must(t, NewEngine(s, bins).Tick(context.Background(), now.Add(time.Minute)))
			if s.HasSegment(seg.Path) || len(s.Notices()) != 1 || s.QueueCount() != 1 {
				t.Fatal("deleted video was retried or re-registered after recovery")
			}
		})
	}
}

func TestArchiveCleanupKeepsVideoOnToolAndStorageFailures(t *testing.T) {
	for _, failure := range []string{"missing ffmpeg", "missing ffprobe", "permission denied", "no space left", "timeout", "valid inputs", "probe unavailable"} {
		t.Run(failure, func(t *testing.T) {
			s := testStore(t)
			bins := emptyVideoBinaries(t)
			now := time.Now().Truncate(time.Second)
			ev, seg := pendingRecording(t, s, ID(), now.Add(-30*time.Second))
			engine := NewEngine(s, bins)
			switch failure {
			case "missing ffmpeg":
				must(t, os.Remove(bins.FFmpeg))
			case "missing ffprobe":
				must(t, os.Remove(bins.FFprobe))
			case "permission denied", "no space left":
				must(t, os.WriteFile(bins.FFmpeg, []byte("#!/bin/sh\nprintf '"+failure+"\\n' >&2\nexit 1\n"), 0700))
			case "timeout":
				engine.assemble = func(context.Context, *Store, Binaries, Event, []Segment) (Part, error) {
					return Part{}, fmt.Errorf("%w: %w", errVideoAssembly, context.DeadlineExceeded)
				}
			case "valid inputs", "probe unavailable":
				must(t, os.WriteFile(bins.FFmpeg, []byte("#!/bin/sh\nprintf 'Invalid data found when processing input\\n' >&2\nexit 1\n"), 0700))
				if failure == "valid inputs" {
					must(t, os.WriteFile(bins.FFprobe, []byte("#!/bin/sh\nprintf '%s' '{\"streams\":[{\"codec_type\":\"video\",\"codec_name\":\"h264\"}],\"format\":{\"duration\":\"5\"}}'\n"), 0700))
				} else {
					must(t, os.Remove(bins.FFprobe))
				}
			}
			for _, seconds := range []int{0, 6, 17, 38, 79} {
				if err := engine.Tick(context.Background(), now.Add(time.Duration(seconds)*time.Second)); !errors.Is(err, errVideoAssembly) {
					t.Fatalf("expected retryable failure, got %v", err)
				}
			}
			actual, err := s.Event(ev.ID)
			must(t, err)
			_, err = os.Stat(filepath.Join(s.Root, seg.Path))
			must(t, err)
			if !s.HasSegment(seg.Path) || actual.Cursor != ev.Cursor || actual.Status != "closing" || len(s.Notices()) != 0 {
				t.Fatalf("tool failure discarded video: %+v", actual)
			}
		})
	}
}

func TestArchiveCleanupHonorsCancellationAndDiskGuard(t *testing.T) {
	for _, failure := range []string{"cancellation", "disk offline"} {
		t.Run(failure, func(t *testing.T) {
			s := testStore(t)
			engine := NewEngine(s, emptyVideoBinaries(t))
			now := time.Now().Truncate(time.Second)
			ev, seg := pendingRecording(t, s, ID(), now.Add(-time.Minute))
			for _, seconds := range []int{0, 6} {
				if err := engine.Tick(context.Background(), now.Add(time.Duration(seconds)*time.Second)); !errors.Is(err, errVideoAssembly) {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			engine.assemble = func(context.Context, *Store, Binaries, Event, []Segment) (Part, error) {
				if failure == "cancellation" {
					cancel()
				} else {
					s.Guard.Development = false
				}
				return Part{}, fmt.Errorf("%w: %w", errVideoAssembly, errNoVideo)
			}
			if err := engine.Tick(ctx, now.Add(17*time.Second)); err == nil {
				t.Fatal("expected interrupted processing")
			}
			actual, err := s.Event(ev.ID)
			must(t, err)
			_, err = os.Stat(filepath.Join(s.Root, seg.Path))
			must(t, err)
			if actual.AssemblyFailures != 2 || actual.Status != "closing" || !s.HasSegment(seg.Path) || len(s.Notices()) != 0 {
				t.Fatalf("interrupted processing discarded video: %+v", actual)
			}
		})
	}
}

func TestArchiveCleanupPreservesGoodFragmentsWithRealFFmpeg(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not installed")
	}
	s := testStore(t)
	now := time.Now().UTC().Truncate(time.Second)
	ev, first := pendingRecording(t, s, ID(), now.Add(-30*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=5", "-t", "5", "-c:v", "mpeg4", "-movflags", "frag_keyframe+empty_moov", "-y", filepath.Join(s.Root, first.Path)).CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, output)
	}
	valid, err := os.ReadFile(filepath.Join(s.Root, first.Path))
	must(t, err)
	// Put a broken header first so FFmpeg cannot publish a usable prefix and
	// report success before reaching the corrupt file in the middle.
	must(t, os.WriteFile(filepath.Join(s.Root, first.Path), []byte("incomplete MP4 header"), 0600))
	var bad, good Segment
	for i := 1; i <= 3; i++ {
		start := time.UnixMilli(first.Start).UTC().Add(time.Duration(i) * 5 * time.Second)
		path := filepath.Join(s.Root, "buffer", ev.CameraID, start.Format("2006-01-02_15-04-05.000000")+".mp4")
		data := valid
		if i == 2 {
			data = []byte("incomplete MP4 header")
		}
		must(t, AtomicWrite(path, data, 0600))
		seg, err := ParseSegment(s.Root, path, 5)
		must(t, err)
		must(t, s.AddSegment(seg))
		if i == 2 {
			bad = seg
		} else {
			good = seg
		}
		ev.End = seg.End
	}
	must(t, s.SaveEvent(ev))
	// The same buffer may also be pinned by an overlapping event.
	overlap := ev
	overlap.ID = ID()
	overlap.Start++
	must(t, s.SaveEvent(overlap))
	engine := NewEngine(s, Binaries{FFmpeg: ffmpeg, FFprobe: ffprobe})
	for _, seconds := range []int{0, 6} {
		if err = engine.Tick(ctx, now.Add(time.Duration(seconds)*time.Second)); !errors.Is(err, errVideoAssembly) {
			t.Fatalf("expected invalid input: %v", err)
		}
	}
	must(t, engine.Tick(ctx, now.Add(17*time.Second)))
	for _, id := range []string{ev.ID, overlap.ID} {
		actual, err := s.Event(id)
		must(t, err)
		parts, err := s.Parts(id)
		must(t, err)
		if actual.Status != "closed" || !actual.Incomplete || actual.Cursor != ev.End || len(parts) != 1 || len(parts[0].Gaps) != 1 {
			t.Fatalf("good fragments did not survive cleanup: %+v, %+v", actual, parts)
		}
		probe, err := Probe(ctx, ffprobe, filepath.Join(s.Root, parts[0].Path), false)
		must(t, err)
		if probe.Duration < 9.8 {
			t.Fatalf("healthy footage lost: %+v", probe)
		}
	}
	if s.HasSegment(first.Path) || s.HasSegment(bad.Path) || !s.HasSegment(good.Path) || len(s.Notices()) != 2 {
		t.Fatal("cleanup removed good inputs or kept corrupt data")
	}
}
