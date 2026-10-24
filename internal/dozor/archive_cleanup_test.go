package dozor

import (
	"context"
	"database/sql"
	"encoding/binary"
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

func TestArchiveDeletesUnassemblableEventAfterFourthFailure(t *testing.T) {
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
			for _, seconds := range []int{0, 6, 17} {
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
			// The count is durable, including errors unrelated to corrupt input.
			engine = NewEngine(s, bins)
			if failure == "timeout" {
				engine.assemble = func(context.Context, *Store, Binaries, Event, []Segment) (Part, error) {
					return Part{}, fmt.Errorf("%w: %w", errVideoAssembly, context.DeadlineExceeded)
				}
			}
			must(t, engine.Tick(context.Background(), now.Add(38*time.Second)))
			if _, err = s.Event(ev.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("fourth failure left an event: %v", err)
			}
			if len(s.Notices()) != 1 || !strings.Contains(s.Notices()[0].Message, "после 4 ошибок") {
				t.Fatal("missing deletion notice", s.Notices())
			}
			if _, err = os.Stat(filepath.Join(s.Root, eventDir(ev))); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("deleted archive directory remains: %v", err)
			}
			must(t, s.Recover())
			must(t, engine.Tick(context.Background(), now.Add(2*time.Minute)))
			if _, err = s.Event(ev.ID); !errors.Is(err, sql.ErrNoRows) || s.QueueCount() != 0 {
				t.Fatalf("deleted event or upload jobs returned: %v", err)
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

// The recorder registers a fragment from its reported duration. One that
// declares a video track but holds no frames probes fine on its own, while FFmpeg
// drops the empty track and the assembled minute has no video.
func framelessBinaries(t *testing.T) Binaries {
	t.Helper()
	dir := t.TempDir()
	ffmpeg := filepath.Join(dir, "ffmpeg")
	probe := filepath.Join(dir, "ffprobe")
	// The fake concatenates the listed inputs, like the real concat demuxer.
	must(t, os.WriteFile(ffmpeg, []byte(`#!/bin/sh
for a; do
	[ "$prev" = -i ] && list=$a
	prev=$a
	out=$a
done
cd "$(dirname "$list")" || exit 1
sed -n "s/^file '\(.*\)'\$/\1/p" "$list" | while read -r f; do cat "$f"; done > "$out"
`), 0700))
	must(t, os.WriteFile(probe, []byte(`#!/bin/sh
for a; do last=$a; done
case "$last" in
*.partial) grep -q FRAMES "$last" || { printf '%s' '{"streams":[],"format":{}}'; exit 0; } ;;
esac
printf '%s' '{"streams":[{"codec_type":"video","codec_name":"h264"}],"format":{"duration":"5"}}'
`), 0700))
	return Binaries{MediaMTX: ffmpeg, FFmpeg: ffmpeg, FFprobe: probe, Self: ffmpeg}
}

func TestArchiveSkipsFramelessWindowAndKeepsLaterVideo(t *testing.T) {
	s := testStore(t)
	bins := framelessBinaries(t)
	now := time.Now().Truncate(time.Second)
	base := now.Add(-3 * time.Minute)
	ev := Event{ID: ID(), CameraID: ID(), Status: "closing"}
	var empty, good []Segment
	for i := range 14 {
		start := base.Add(time.Duration(i) * 5 * time.Second).UTC()
		path := filepath.Join(s.Root, "buffer", ev.CameraID, start.Format("2006-01-02_15-04-05.000000")+".mp4")
		content := "moov without frames"
		if i >= 12 {
			content = "FRAMES"
		}
		must(t, AtomicWrite(path, []byte(content), 0600))
		seg, err := ParseSegment(s.Root, path, 5)
		must(t, err)
		must(t, s.AddSegment(seg))
		if i < 12 {
			empty = append(empty, seg)
		} else {
			good = append(good, seg)
		}
	}
	ev.Start, ev.Cursor, ev.End = empty[0].Start, empty[0].Start, good[1].End
	must(t, s.SaveEvent(ev))
	engine := NewEngine(s, bins)
	attempts := 0
	engine.assemble = func(ctx context.Context, s *Store, b Binaries, ev Event, segs []Segment) (Part, error) {
		attempts++
		return Assemble(ctx, s, b, ev, segs)
	}
	for _, offset := range []time.Duration{0, 6 * time.Second} {
		err := engine.Tick(context.Background(), now.Add(offset))
		if !errors.Is(err, errNoVideo) || !errors.Is(err, errVideoAssembly) {
			t.Fatalf("expected the empty-video error, got %v", err)
		}
	}
	must(t, engine.Tick(context.Background(), now.Add(17*time.Second)))
	actual, err := s.Event(ev.ID)
	must(t, err)
	parts, err := s.Parts(ev.ID)
	must(t, err)
	if actual.Status != "closed" || !actual.Incomplete || actual.Cursor != ev.End || actual.AssemblyFailures != 0 {
		t.Fatalf("frameless window kept the event in the retry loop: %+v", actual)
	}
	if len(parts) != 1 || parts[0].Start != good[0].Start || parts[0].End != good[1].End {
		t.Fatalf("later footage was lost with the frameless window: %+v", parts)
	}
	notices := s.Notices()
	if len(notices) != 1 {
		t.Fatalf("expected one notice for the skipped window: %+v", notices)
	}
	for _, detail := range []string{"Окно записи без видео пропущено после 3 ошибок", ev.CameraID, ev.ID, empty[0].Path, empty[11].Path, "12 фрагм."} {
		if !strings.Contains(notices[0].Message, detail) {
			t.Fatalf("missing %q: %s", detail, notices[0].Message)
		}
	}
	// Three failed attempts and the one that carried the later footage.
	for _, offset := range []time.Duration{time.Minute, 5 * time.Minute, time.Hour} {
		must(t, engine.Tick(context.Background(), now.Add(offset)))
	}
	if attempts != 4 || len(s.Notices()) != 1 {
		t.Fatalf("the closed event was retried: attempts=%d notices=%+v", attempts, s.Notices())
	}
}

func TestArchiveSkipsFramelessSegmentWithRealFFmpeg(t *testing.T) {
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
	ev, seg := pendingRecording(t, s, ID(), now.Add(-30*time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	path := filepath.Join(s.Root, seg.Path)
	// Unlike mpeg4, H.264 keeps its parameters in the header, which stay valid without frames.
	output, err := exec.CommandContext(ctx, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=160x90:rate=5", "-t", "5", "-c:v", "libx264", "-movflags", "frag_keyframe+empty_moov", "-y", path).CombinedOutput()
	if err != nil {
		t.Skipf("ffmpeg cannot encode H.264: %v: %s", err, output)
	}
	data, err := os.ReadFile(path)
	must(t, err)
	// Keep the headers that declare the video track and drop every fragment.
	var headers []byte
	for len(data) >= 8 {
		size := int(binary.BigEndian.Uint32(data))
		if size < 8 || size > len(data) {
			break
		}
		if kind := string(data[4:8]); kind == "ftyp" || kind == "moov" {
			headers = append(headers, data[:size]...)
		}
		data = data[size:]
	}
	must(t, AtomicWrite(path, headers, 0600))
	if probe, err := Probe(ctx, ffprobe, path, false); err != nil || probe.Video == "" {
		t.Fatalf("the fragment must look valid on its own: %+v, %v", probe, err)
	}
	engine := NewEngine(s, Binaries{FFmpeg: ffmpeg, FFprobe: ffprobe})
	for _, seconds := range []int{0, 6} {
		if err = engine.Tick(ctx, now.Add(time.Duration(seconds)*time.Second)); !errors.Is(err, errNoVideo) {
			t.Fatalf("expected the empty-video error: %v", err)
		}
	}
	must(t, engine.Tick(ctx, now.Add(17*time.Second)))
	actual, err := s.Event(ev.ID)
	must(t, err)
	if actual.Status != "closed" || !actual.Incomplete || actual.Cursor != seg.End || len(s.Notices()) != 1 {
		t.Fatalf("frameless fragment kept the event in the retry loop: %+v, %+v", actual, s.Notices())
	}
	for _, seconds := range []int{60, 120} {
		must(t, engine.Tick(ctx, now.Add(time.Duration(seconds)*time.Second)))
	}
	if len(s.Notices()) != 1 {
		t.Fatal("the skipped window was reported again", s.Notices())
	}
}
