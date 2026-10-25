package dozor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"
)

func TestMotionFrameWatchdog(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancelCause(context.Background())
		defer cancel(nil)
		frames := make(chan struct{}, 1)
		done := make(chan struct{})
		go func() { defer close(done); watchMotionFrames(ctx, frames, cancel, detectorFrameTimeout) }()
		synctest.Wait()
		for range 3 {
			time.Sleep(19 * time.Second)
			frames <- struct{}{}
			synctest.Wait()
			if ctx.Err() != nil {
				t.Fatal("healthy frames did not extend deadline")
			}
		}
		time.Sleep(detectorFrameTimeout)
		<-done
		if !errors.Is(context.Cause(ctx), errDetectorStalled) {
			t.Fatal(context.Cause(ctx))
		}
	})
}

func TestMotionStallKillsProcessAndKeepsWarmupRecording(t *testing.T) {
	// Emit complete static frames followed by an incomplete frame, then stay alive.
	// Bytes without a complete frame must not renew the deadline.
	bin := filepath.Join(t.TempDir(), "detector")
	must(t, os.WriteFile(bin, []byte("#!/bin/sh\ndd if=/dev/zero bs=57600 count=15 2>/dev/null\nprintf x\nexec sleep 30\n"), 0700))
	camera := Camera{ID: ID(), SubURL: "rtsp://fixture/sub", Sensitivity: .8}
	for range 2 {
		frames := 0
		started := time.Now()
		err := (LocalMotion{FFmpeg: bin}).run(context.Background(), camera, func(signal MotionSignal) {
			frames++
			if signal.Healthy != (frames >= 15) {
				t.Fatalf("unsafe warmup at frame %d: %+v", frames, signal)
			}
		}, time.Second)
		if !errors.Is(err, errDetectorStalled) || frames != 15 || time.Since(started) > 3*time.Second {
			t.Fatalf("stalled process was not terminated: frames=%d elapsed=%s err=%v", frames, time.Since(started), err)
		}
	}
}

func TestStalledDetectorPreservesVideoAndResumesMotionMode(t *testing.T) {
	s := testStore(t)
	e := recordingEngine(t, s)
	camera := ID()
	now := time.Now().Truncate(time.Second)
	// The detector last reported no movement before it stalled.
	e.Signal(MotionSignal{CameraID: camera, Healthy: true, Source: "local", At: now})
	for second := -45; second < 45; second += 5 {
		start := now.Add(time.Duration(second) * time.Second)
		must(t, e.AddSegment(Segment{Path: start.Format("150405") + ".mp4", CameraID: camera, Start: start.UnixMilli(), End: start.Add(5 * time.Second).UnixMilli()}))
	}
	e.Signal(MotionSignal{CameraID: camera, Healthy: false, Source: "local", At: now.Add(detectorFrameTimeout)})
	must(t, e.Tick(context.Background(), now.Add(detectorFrameTimeout)))
	events, err := s.Events(camera, 10)
	must(t, err)
	if len(events) != 1 || events[0].Source != "detector_failure" {
		t.Fatal(events)
	}
	parts, err := s.Parts(events[0].ID)
	must(t, err)
	if len(parts) == 0 || parts[0].Start > now.UnixMilli() {
		t.Fatal("video preceding stall was not retained", parts)
	}
	recovered := now.Add(25 * time.Second)
	e.Signal(MotionSignal{CameraID: camera, Healthy: true, Source: "local", At: recovered})
	must(t, e.Tick(context.Background(), now.Add(50*time.Second)))
	event, err := s.Event(events[0].ID)
	must(t, err)
	if event.Status != "closed" || event.End != now.Add(30*time.Second).UnixMilli() {
		t.Fatal("continuous fallback did not finish", event)
	}
}
