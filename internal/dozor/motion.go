package dozor

import (
	"context"
	"errors"
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

type LocalMotion struct{ FFmpeg string }

func (l LocalMotion) Run(ctx context.Context, c Camera, emit func(MotionSignal)) error {
	if c.SubURL == "" {
		return errors.New("нет дополнительного потока для детектора")
	}
	cmd := exec.CommandContext(ctx, l.FFmpeg, "-nostdin", "-v", "error", "-rtsp_transport", "tcp", "-timeout", "8000000", "-i", CameraURL(c, true), "-an", "-vf", "fps=5,scale=320:180", "-pix_fmt", "gray", "-f", "rawvideo", "pipe:1")
	cmd.Stderr = io.Discard
	out, e := cmd.StdoutPipe()
	if e != nil {
		return e
	}
	if e = cmd.Start(); e != nil {
		return e
	}
	defer func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	}()
	d := NewDetector(320, 180, c.Sensitivity, c.Masks)
	frame := make([]byte, 320*180)
	for {
		_, e = io.ReadFull(out, frame)
		if e != nil {
			return errors.New("поток детектора прерван")
		}
		emit(MotionSignal{CameraID: c.ID, Active: d.Frame(frame), Healthy: true, Source: "local", At: time.Now()})
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
			_ = err
			emit(MotionSignal{CameraID: c.ID, Healthy: false, Source: "onvif", At: time.Now()})
			if c.Motion == "onvif" {
				notice("ONVIF-события недоступны: " + c.Name)
				if !pause(ctx, 10*time.Second) {
					return
				}
				continue
			}
		}
		if c.SubURL != "" {
			_ = (LocalMotion{FFmpeg: b.FFmpeg}).Run(ctx, c, emit)
		}
		if ctx.Err() != nil {
			return
		}
		emit(MotionSignal{CameraID: c.ID, Healthy: false, Source: "local", At: time.Now()})
		notice("Детектор недоступен, временная непрерывная запись: " + c.Name)
		if !pause(ctx, 10*time.Second) {
			return
		}
	}
}
