package dozor

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"time"
)

const (
	recordingStallTimeout = 30 * time.Second
	recordingRetryMax     = 5 * time.Minute
)

// The control listener is loopback-only with a per-runtime credential. It is not
// exposed through the application proxy or shared with the anonymous live reader.
type mediaControl struct {
	address  string
	password string
	client   *http.Client
}

func newMediaControl() (*mediaControl, error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return nil, err
	}
	return &mediaControl{address: address, password: ID(), client: &http.Client{
		Timeout:       5 * time.Second,
		Transport:     &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

func (m *mediaControl) setSource(ctx context.Context, camera, source string) error {
	if !safeID.MatchString(camera) {
		return errors.New("неверный ID камеры")
	}
	body, err := json.Marshal(map[string]string{"source": source})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, "http://"+m.address+"/v3/config/paths/patch/"+camera, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.SetBasicAuth("dozor-recorder", m.password)
	req.Header.Set("Content-Type", "application/json")
	res, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("управление записью недоступно: %w", err)
	}
	defer res.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(res.Body, 4096))
	if res.StatusCode != http.StatusOK {
		// Never expose an API response that could echo a camera URL/password.
		return fmt.Errorf("управление записью: HTTP %d", res.StatusCode)
	}
	return nil
}

func (m *mediaControl) reconnect(ctx context.Context, c Camera) error {
	// Changing only this path's source closes its recorder, flushing the tail,
	// and leaves every other camera running. No publisher permission is granted.
	if err := m.setSource(ctx, c.ID, "publisher"); err != nil {
		return err
	}
	return m.setSource(ctx, c.ID, CameraURL(c, false))
}

type recordingWatch struct {
	lastProgress  time.Time
	lastPath      string
	nextAttempt   time.Time
	retryDelay    time.Duration
	failed        bool
	restore       bool
	recoveryAfter int64
	observedPath  string
	generation    int64
}

// The observation clock is monotonic: a bogus future timestamp in a segment
// cannot keep the watchdog healthy forever. Only a new, probed file is progress.
func (w *recordingWatch) progress(path string, now time.Time) bool {
	if path == "" || path == w.lastPath {
		return false
	}
	w.lastPath = path
	w.lastProgress = now
	return true
}

func (w *recordingWatch) due(now time.Time) bool {
	return now.Sub(w.lastProgress) >= recordingStallTimeout && !now.Before(w.nextAttempt)
}

func (w *recordingWatch) attempted(now time.Time) {
	if w.retryDelay == 0 {
		w.retryDelay = time.Minute
	} else {
		w.retryDelay = min(w.retryDelay*2, recordingRetryMax)
	}
	w.nextAttempt = now.Add(w.retryDelay)
}

func (r *Runtime) watchRecording(ctx context.Context, camera Camera) {
	watch := recordingWatch{lastProgress: time.Now()}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		r.checkRecording(ctx, camera, &watch, time.Now(), func(ctx context.Context, path string) (ProbeResult, error) {
			return Probe(ctx, r.Engine.Bins.FFprobe, path, false)
		})
	}
}

func (r *Runtime) checkRecording(ctx context.Context, camera Camera, watch *recordingWatch, now time.Time, probeFile func(context.Context, string) (ProbeResult, error)) {
	mediaSince := r.mediaSince.Load()
	if mediaSince == 0 || !r.workerReady.Load() {
		watch.lastProgress = now
		return
	}
	if mediaSince != watch.generation {
		*watch = recordingWatch{lastProgress: now, generation: mediaSince}
		// A new MediaMTX process loads the original sources from disk.
	}
	var seg Segment
	err := r.Store.DB.QueryRowContext(ctx, "SELECT path,camera,start,end FROM segments WHERE camera=? ORDER BY rowid DESC LIMIT 1", camera.ID).Scan(&seg.Path, &seg.CameraID, &seg.Start, &seg.End)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		r.reportArchiveError("Не удалось проверить запись", err, now)
		return // A catalog failure is not evidence that a camera needs a restart.
	}
	// Old archive files must not report recovery after restarting the service.
	if seg.Path != "" && seg.Path != watch.observedPath && seg.End >= time.Unix(watch.generation, 0).UnixMilli() {
		watch.observedPath = seg.Path
		path := filepath.Join(r.Store.Root, seg.Path)
		probe, probeErr := probeFile(ctx, path)
		if probeErr == nil && probe.Video != "" && probe.Duration > 0 && probe.Duration <= 3600 && !watch.restore && seg.Start >= watch.recoveryAfter {
			if watch.progress(seg.Path, now) && watch.failed {
				r.Store.Notice(fmt.Sprintf("Запись камеры %s (%s) восстановлена: получен новый видеофрагмент", camera.Name, camera.ID))
				watch.failed = false
				watch.retryDelay = 0
				watch.nextAttempt = time.Time{}
			}
		}
	}
	if ctx.Err() != nil || !watch.due(now) {
		return
	}
	r.reportCameraFailure(camera, "recording", "Нет новых пригодных видеофрагментов 30 секунд; выполняется переподключение", now)
	watch.failed = true
	watch.attempted(now)
	if watch.restore {
		err = r.mediaControl.setSource(ctx, camera.ID, CameraURL(camera, false))
	} else {
		// Even an ambiguous HTTP failure must restore the original source.
		watch.restore = true
		// The flushed tail of the old session is retained, but does not prove
		// that recording has resumed after this attempt.
		watch.recoveryAfter = now.UnixMilli()
		err = r.mediaControl.reconnect(ctx, camera)
	}
	if err != nil {
		if ctx.Err() == nil {
			r.reportCameraFailure(camera, "recording", "Не удалось восстановить запись: "+err.Error(), now)
		}
		// A partially applied change must be repaired promptly.
		watch.nextAttempt = now.Add(10 * time.Second)
		return
	}
	watch.restore = false
	// Reconcile closed files promptly; a successful reconnect alone is not recovery.
	select {
	case r.bufferWake <- struct{}{}:
	default:
	}
}
