package dozor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os/exec"
	"time"
)

type MotionSource interface {
	Run(context.Context, Camera, func(MotionSignal)) error
}
type Detector struct {
	background   []float64
	mask         []bool
	threshold    float64
	warmup, hits int
}

func NewDetector(w, h int, sensitivity float64, masks []Rect) *Detector {
	d := &Detector{background: make([]float64, w*h), mask: make([]bool, w*h), threshold: .005 + (1-sensitivity)*.08}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			for _, r := range masks {
				if float64(x)/float64(w) >= r.X && float64(x)/float64(w) < r.X+r.W && float64(y)/float64(h) >= r.Y && float64(y)/float64(h) < r.Y+r.H {
					d.mask[y*w+x] = true
				}
			}
		}
	}
	return d
}
func (d *Detector) Frame(p []byte) bool {
	if len(p) != len(d.background) {
		return false
	}
	changed, total := 0, 0
	for i, b := range p {
		v := float64(b)
		if d.warmup == 0 {
			d.background[i] = v
		}
		delta := math.Abs(v - d.background[i])
		if !d.mask[i] {
			total++
			if delta > 24 {
				changed++
			}
		}
		alpha := .025
		if d.warmup < 15 {
			alpha = .25
		}
		d.background[i] += alpha * (v - d.background[i])
	}
	d.warmup++
	if d.warmup < 15 || total == 0 {
		return false
	}
	if float64(changed)/float64(total) > d.threshold {
		d.hits++
	} else {
		d.hits = 0
	}
	return d.hits >= 2
}

const detectorFrameTimeout = 20 * time.Second

var errDetectorStalled = errors.New("детектор не получает кадры 20 секунд")

type LocalMotion struct{ FFmpeg string }

func (l LocalMotion) Run(ctx context.Context, c Camera, emit func(MotionSignal)) error {
	return l.run(ctx, c, emit, detectorFrameTimeout)
}

func (l LocalMotion) run(ctx context.Context, c Camera, emit func(MotionSignal), timeout time.Duration) (result error) {
	if c.SubURL == "" {
		return errors.New("нет дополнительного потока для детектора")
	}
	parent := ctx
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	input := CameraURL(c, true)
	cmd := exec.CommandContext(ctx, l.FFmpeg, "-nostdin", "-v", "error", "-rtsp_transport", "tcp", "-timeout", "8000000", "-i", input, "-an", "-vf", "fps=5,scale=320:180", "-pix_fmt", "gray", "-f", "rawvideo", "pipe:1")
	var stderr diagnosticBuffer
	output := cameraCommandOutput(&stderr, input)
	cmd.Stderr = output
	cmd.WaitDelay = 2 * time.Second
	out, e := cmd.StdoutPipe()
	if e != nil {
		return e
	}
	if e = cmd.Start(); e != nil {
		return cameraCommandError(ctx, "ffmpeg", e, &stderr, input)
	}
	frames := make(chan struct{}, 1)
	watched := make(chan struct{})
	go func() {
		defer close(watched)
		watchMotionFrames(ctx, frames, cancel, timeout)
	}()
	defer func() {
		cause := context.Cause(ctx)
		cancel(nil)
		<-watched
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		waitErr := cmd.Wait()
		output.Flush()
		if waitErr == nil {
			waitErr = result
		}
		if errors.Is(cause, errDetectorStalled) && parent.Err() == nil {
			waitErr = cause
		}
		result = fmt.Errorf("поток детектора прерван: %w", cameraCommandError(parent, "ffmpeg", waitErr, &stderr, input))
	}()
	d := NewDetector(320, 180, c.Sensitivity, c.Masks)
	frame := make([]byte, 320*180)
	for {
		_, e = io.ReadFull(out, frame)
		if e != nil {
			return e
		}
		select {
		case frames <- struct{}{}:
		default:
		}
		active := d.Frame(frame)
		// Keep continuous recording through the background model's warmup.
		emit(MotionSignal{CameraID: c.ID, Active: active, Healthy: d.warmup >= 15, Source: "local", At: time.Now()})
	}
}

// A live process or incoming network packets do not prove that frames decode.
// Only complete frames reset the timer; cancellation kills FFmpeg and unblocks ReadFull.
func watchMotionFrames(ctx context.Context, frames <-chan struct{}, cancel context.CancelCauseFunc, timeout time.Duration) {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-frames:
			timer.Reset(timeout)
		case <-timer.C:
			cancel(errDetectorStalled)
			return
		}
	}
}

func RunMotion(ctx context.Context, c Camera, b Binaries, emit func(MotionSignal), notice func(string)) {
	emit(MotionSignal{CameraID: c.ID, Healthy: false, Source: "initializing", At: time.Now()})
	for ctx.Err() == nil {
		if c.Motion != "local" && c.ONVIF != "" {
			err := (ONVIFMotion{}).Run(ctx, c, emit)
			if ctx.Err() != nil {
				return
			}
			emit(MotionSignal{CameraID: c.ID, Healthy: false, Source: "onvif", At: time.Now()})
			if c.Motion == "onvif" {
				notice("ONVIF: " + cameraDiagnostic(err.Error(), CameraURL(c, false)))
				if !pause(ctx, 10*time.Second) {
					return
				}
				continue
			}
		}
		err := (LocalMotion{FFmpeg: b.FFmpeg}).Run(ctx, c, emit)
		if ctx.Err() != nil {
			return
		}
		emit(MotionSignal{CameraID: c.ID, Healthy: false, Source: "local", At: time.Now()})
		notice("Дополнительный поток / локальный детектор: " + err.Error())
		if !pause(ctx, 10*time.Second) {
			return
		}
	}
}
