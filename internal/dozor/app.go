package dozor

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Runtime struct {
	Store             *Store
	Engine            *Engine
	cancel            context.CancelFunc
	archiveCancel     context.CancelFunc
	wg                sync.WaitGroup
	segments          chan Segment
	bufferWake        chan struct{}
	mediaSince        atomic.Int64
	streamsView       atomic.Value
	workerReady       atomic.Bool
	catalogMS         int64
	journalMS         atomic.Int64
	bufferMS          atomic.Int64
	streams           map[string]AvailabilityInterval
	streamSignals     chan StreamSignal
	archiveMu         sync.Mutex
	archiveError      string
	archiveNotice     time.Time
	diagnosticMu      sync.Mutex
	diagnosticNotices map[string]CameraFailure
}

func (r *Runtime) reportArchiveError(prefix string, err error, now time.Time) {
	r.archiveMu.Lock()
	defer r.archiveMu.Unlock()
	if errors.Is(err, context.Canceled) {
		return
	}
	message := prefix + ": " + err.Error()
	if message == r.archiveError && now.Sub(r.archiveNotice) < time.Minute {
		return
	}
	r.archiveError, r.archiveNotice = message, now
	r.Store.Notice(message)
	// Keep diagnostics available in journald even when the catalog cannot be written.
	log.Print(message)
}

type App struct {
	Config                    *ConfigFile
	Bins                      Binaries
	Development               bool
	StateDir, Socket, Version string
	mu                        sync.RWMutex
	runtime                   *Runtime
	storageError              string
	reload                    chan struct{}
	authMu                    sync.Mutex
	sessions                  map[string]Session
	attempts                  map[string]loginAttempt
	setupToken                string
	updateMu                  sync.Mutex
	reboots                   *RebootScheduler
	statusView                atomic.Value
	healthView                atomic.Value
	runStarted                atomic.Int64
	storage                   *StoragePolicyFile
}

func NewApp(c *ConfigFile, b Binaries, dev bool, state, socket, version string) (*App, error) {
	if e := os.MkdirAll(state, 0700); e != nil {
		return nil, e
	}
	reboots, err := loadRebootScheduler(state, time.Now())
	if err != nil {
		return nil, err
	}
	storage, err := loadStoragePolicy(state)
	if err != nil {
		return nil, err
	}
	a := &App{storage: storage, reboots: reboots, Config: c, Bins: b, Development: dev, StateDir: state, Socket: socket, Version: version, reload: make(chan struct{}, 1), sessions: map[string]Session{}, attempts: map[string]loginAttempt{}}
	if c.Get().PasswordHash == "" {
		p := filepath.Join(state, "setup-token")
		token, e := os.ReadFile(p)
		if errors.Is(e, os.ErrNotExist) {
			token = []byte(ID() + ID())
			e = AtomicWrite(p, token, 0600)
		}
		if e != nil {
			return nil, e
		}
		a.setupToken = string(token)
	}
	return a, nil
}
func (a *App) Reload() {
	select {
	case a.reload <- struct{}{}:
	default:
	}
}
func (a *App) start(ctx context.Context) error {
	started := time.Now()
	c := a.Config.Get()
	g := Guard{c.Archive, c.DiskUUID, a.Development}
	if e := g.Check(); e != nil {
		return e
	}
	s, e := OpenStore(g)
	if e != nil {
		return e
	}
	if !a.Development {
		if readUpdateStatus(a.StateDir).Running {
			// A legacy installer may reuse a staged directory after rollback.
			// Its recovery helper still identifies the old catalog writer.
			child, stop := context.WithTimeout(ctx, 3*time.Second)
			legacy := exec.CommandContext(child, "/usr/local/libexec/dozor-recover", "system", "catalog-protocol").Run() != nil
			stop()
			if legacy {
				if err := s.RequestRecovery(); err != nil {
					s.Close()
					return err
				}
			}
		}
		marker, markerErr := os.ReadFile("/opt/dozor/current/.catalog-transition")
		if markerErr == nil {
			token := strings.TrimSpace(string(marker))
			if !safeID.MatchString(token) {
				s.Close()
				return errors.New("invalid catalog transition marker")
			}
			if err := s.requestRecovery(token); err != nil {
				s.Close()
				return err
			}
		} else if !errors.Is(markerErr, os.ErrNotExist) {
			s.Close()
			return markerErr
		}
	}

	rctx, cancel := context.WithCancel(ctx)
	archiveCtx, archiveCancel := context.WithCancel(rctx)
	r := &Runtime{catalogMS: time.Since(started).Milliseconds(), Store: s, Engine: NewEngine(s, a.Bins), cancel: cancel, archiveCancel: archiveCancel, segments: make(chan Segment, 512), bufferWake: make(chan struct{}, 1)}
	r.Engine.deferRecovered = true
	if e = r.initStreams(c.Cameras, time.Now()); e != nil {
		s.Close()
		archiveCancel()
		cancel()
		return e
	}
	a.mu.Lock()
	a.runtime = r
	a.mu.Unlock()
	r.publishStreams()
	a.publishStatus(r)
	launch := func(f func()) { r.wg.Add(1); go func() { defer r.wg.Done(); f() }() }
	launch(func() {
		RunMedia(rctx, c, a.Bins, a.Socket, filepath.Join(a.StateDir, "mediamtx.yml"), s.Notice, func(ready bool) {
			if ready {
				r.mediaSince.Store(time.Now().Unix())
			} else {
				r.mediaSince.Store(0)
				// A crashed recorder may never run its per-path offline hooks.
				for _, cam := range c.Cameras {
					if cam.Enabled {
						select {
						case r.streamSignals <- StreamSignal{CameraID: cam.ID, At: time.Now().UnixMilli()}:
						default:
						}
					}
				}
			}
		}, func(camera Camera, message string) {
			if rctx.Err() == nil {
				r.reportCameraFailure(camera, "connection", message, time.Now())
			}
		})
	})
	for _, cam := range c.Cameras {
		if cam.Enabled {
			launch(func() {
				RunMotion(rctx, cam, a.Bins, r.Engine.Signal, func(message string) {
					r.reportCameraFailure(cam, "detector", message, time.Now())
				})
			})
		}
	}
	if c.S3.Enabled {
		launch(func() {
			if err := s.SetTarget(c.S3); err != nil {
				s.Notice(err.Error())
				return
			}
			remote := NewS3(c.S3)
			for rctx.Err() == nil {
				worked, err := UploadOne(rctx, s, c.S3, remote)
				if err != nil || !worked {
					if !pause(rctx, 3*time.Second) {
						return
					}
				}
			}
		})
	}
	launch(func() { a.runArchive(archiveCtx, r) })
	launch(func() { a.runBufferReconciliation(archiveCtx, r) })
	launch(func() {
		for rctx.Err() == nil {
			a.publishStatus(r)
			if !pause(rctx, 2*time.Second) {
				return
			}
		}
	})
	launch(func() { s.RunRecovery(archiveCtx, func() bool { return !readUpdateStatus(a.StateDir).Running }, nil) })
	return nil
}
func (a *App) stop() {
	a.mu.Lock()
	if a.runtime == nil {
		a.mu.Unlock()
		return
	}
	r := a.runtime
	a.runtime = nil
	a.mu.Unlock()
	r.cancel()
	r.wg.Wait()
	finishCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.finishDisabledStreams(finishCtx, a.Config.Get().Cameras, time.Now()); err != nil {
		r.reportArchiveError("Не удалось завершить запись выключенной камеры; повтор при восстановлении", err, time.Now())
	}
	_ = r.Store.Close()
}
func (a *App) Run(ctx context.Context) error {
	a.runStarted.Store(time.Now().UnixMilli())
	if e := os.MkdirAll(filepath.Dir(a.Socket), 0700); e != nil {
		return e
	}
	if _, e := os.Stat(a.Socket); e == nil {
		conn, ce := net.DialTimeout("unix", a.Socket, time.Second)
		if ce == nil {
			conn.Close()
			return errors.New("Dozor уже запущена")
		}
		_ = os.Remove(a.Socket)
	}
	listener, e := net.Listen("unix", a.Socket)
	if e != nil {
		return e
	}
	if e = os.Chmod(a.Socket, 0600); e != nil {
		listener.Close()
		return e
	}
	internal := &http.Server{Handler: a.internalHandler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = internal.Serve(listener) }()
	defer func() { _ = internal.Close(); _ = os.Remove(a.Socket) }()
	rebootCtx, stopReboots := context.WithCancel(ctx)
	rebootsDone := make(chan struct{})
	go func() { defer close(rebootsDone); a.runRebootScheduler(rebootCtx) }()
	defer func() { stopReboots(); <-rebootsDone }()
	healthCtx, cancelHealth := context.WithCancel(ctx)
	healthDone := make(chan struct{})
	go func() { defer close(healthDone); a.runHealth(healthCtx) }()
	defer func() { cancelHealth(); <-healthDone }()
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	defer a.stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-a.reload:
			a.stop()
		case <-tick.C:
			a.mu.RLock()
			r := a.runtime
			a.mu.RUnlock()
			if r == nil {
				if err := a.start(ctx); err != nil {
					a.mu.Lock()
					a.storageError = err.Error()
					a.mu.Unlock()
				} else {
					a.mu.Lock()
					a.storageError = ""
					a.mu.Unlock()
				}
			} else if err := r.Store.Guard.Check(); err != nil {
				a.mu.Lock()
				a.storageError = err.Error()
				a.mu.Unlock()
				a.stop()
			}
		}
	}
}

func (a *App) internalHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("POST /stream", func(w http.ResponseWriter, r *http.Request) {
		var v StreamSignal
		if !decode(w, r, &v) {
			return
		}
		if !safeID.MatchString(v.CameraID) || v.At <= 0 || v.At > time.Now().Add(time.Second).UnixMilli() {
			apiError(w, 400, "неверное состояние потока")
			return
		}
		a.mu.RLock()
		defer a.mu.RUnlock()
		if a.runtime == nil {
			apiError(w, 503, "диск недоступен")
			return
		}
		if _, ok := a.runtime.streamSnapshot()[v.CameraID]; !ok {
			apiError(w, 404, "камера не найдена")
			return
		}
		select {
		case a.runtime.streamSignals <- v:
			w.WriteHeader(204)
		default:
			apiError(w, 503, "очередь занята")
		}
	})
	m.HandleFunc("POST /segment", func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			Path     string  `json:"path"`
			Duration float64 `json:"duration"`
		}
		if !decode(w, r, &v) {
			return
		}
		a.mu.RLock()
		defer a.mu.RUnlock()
		if a.runtime == nil {
			apiError(w, 503, "диск недоступен")
			return
		}
		seg, e := ParseSegment(a.runtime.Store.Root, v.Path, v.Duration)
		if e != nil {
			apiError(w, 400, "неверный фрагмент")
			return
		}
		select {
		case a.runtime.segments <- seg:
			w.WriteHeader(204)
		default:
			apiError(w, 503, "очередь занята")
		}
	})
	m.HandleFunc("POST /prepare-update", func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		defer a.mu.RUnlock()
		if a.runtime != nil && !a.runtime.Engine.PrepareUpdate(r.URL.Query().Get("immediate") == "true") {
			apiError(w, 409, "активное событие")
			return
		}
		if a.runtime != nil && a.runtime.archiveCancel != nil {
			a.runtime.archiveCancel()
		}
		w.WriteHeader(204)
	})
	m.HandleFunc("POST /update-request", a.consumeUpdateRequest)
	m.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, a.Health()) })
	m.HandleFunc("POST /recover", func(w http.ResponseWriter, req *http.Request) {
		a.mu.RLock()
		defer a.mu.RUnlock()
		if a.runtime == nil {
			apiError(w, 503, "диск недоступен")
			return
		}
		if err := a.runtime.Store.RequestRecovery(); err != nil {
			apiError(w, 500, err.Error())
			return
		}
		a.Reload()
		w.WriteHeader(http.StatusAccepted)
	})
	return m
}
func UnixClient(socket string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}
func (a *App) Status() map[string]any {
	m := map[string]any{"version": a.Version, "disk_ready": false, "queue": 0, "cameras": []any{}, "notices": []Notice{}}
	if value := a.statusView.Load(); value != nil {
		for k, v := range value.(map[string]any) {
			m[k] = v
		}
	}
	// These fields must remain fresh even while a media operation is stalled.
	if a.mu.TryRLock() {
		m["disk_ready"] = a.runtime != nil
		m["storage_error"] = a.storageError
		a.mu.RUnlock()
	}
	update := readUpdateStatus(a.StateDir)
	m["update_status"], m["update_running"] = update.Message, update.Running
	m["health"] = a.Health()
	return m
}
func (a *App) publishStatus(r *Runtime) {
	c := a.Config.Get()
	m := map[string]any{"version": a.Version, "disk_ready": true, "queue": r.Store.QueueCount(), "notices": r.Store.Notices(), "observed_at": time.Now().UnixMilli()}
	var uploadError string
	_ = r.Store.DB.QueryRow("SELECT error FROM jobs WHERE error LIKE 'S3 %' ORDER BY next DESC LIMIT 1").Scan(&uploadError)
	m["upload_error"] = uploadError
	used, total, _ := DiskUsage(r.Store.Root)
	m["disk_used"], m["disk_total"] = used, total
	states := r.Engine.States()
	streams := r.streamSnapshot()
	cams := []any{}
	for _, cam := range c.Cameras {
		var last int64
		_ = r.Store.DB.QueryRow("SELECT COALESCE(MAX(end),0) FROM segments WHERE camera=?", cam.ID).Scan(&last)
		connectionError, _ := r.Store.LastCameraFailure(cam.ID, "connection")
		detectorError, _ := r.Store.LastCameraFailure(cam.ID, "detector")
		st := states[cam.ID]
		cams = append(cams, map[string]any{"id": cam.ID, "name": cam.Name, "enabled": cam.Enabled, "online": cam.Enabled && r.mediaSince.Load() > 0 && streams[cam.ID].State == "online", "last_segment": last, "motion": st.Active, "detector_healthy": st.Healthy, "source": st.Source, "last_connection_error": connectionError, "last_detector_error": detectorError})
	}
	m["cameras"] = cams
	m["recovery"] = r.Store.RecoveryProgress()
	a.statusView.Store(m)
}
func (r *Runtime) publishStreams() {
	view := make(map[string]AvailabilityInterval, len(r.streams))
	for k, v := range r.streams {
		view[k] = v
	}
	r.streamsView.Store(view)
}
func (r *Runtime) streamSnapshot() map[string]AvailabilityInterval {
	if v := r.streamsView.Load(); v != nil {
		return v.(map[string]AvailabilityInterval)
	}
	return nil
}
func (a *App) runArchive(ctx context.Context, r *Runtime) {
	started := time.Now()
	for ctx.Err() == nil {
		err := r.Store.ReplayOperations(ctx)
		if err == nil {
			err = r.Store.RecoverPending(ctx)
		}
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return
		}
		r.reportArchiveError("Не удалось восстановить журнал", err, time.Now())
		if !pause(ctx, time.Second) {
			return
		}
	}
	r.journalMS.Store(time.Since(started).Milliseconds())
	r.workerReady.Store(true)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	iteration := 0
	for ctx.Err() == nil {
		if err := r.Store.ReplayOperations(ctx); err != nil && ctx.Err() == nil {
			r.reportArchiveError("Не удалось завершить операции каталога", err, time.Now())
		}
		if err := DrainSegmentQueue(ctx, r.Store, r.Engine.AddSegment); err != nil && ctx.Err() == nil {
			r.reportArchiveError("Не удалось обработать очередь фрагментов", err, time.Now())
		}
		for len(r.segments) > 0 {
			if err := r.Engine.AddSegment(<-r.segments); err != nil {
				r.Store.Notice(err.Error())
			}
		}
		if err := r.tickStreams(ctx, time.Now()); err != nil && ctx.Err() == nil {
			r.Store.Notice(err.Error())
		}
		r.publishStreams()
		if err := r.Engine.Tick(ctx, time.Now()); err != nil && ctx.Err() == nil {
			r.reportArchiveError("Ошибка обработки архива; повтор будет выполнен", err, time.Now())
		}
		iteration++
		if iteration%15 == 0 {
			if !a.Development {
				if err := r.Store.PruneArchive(time.Now(), a.storage.Get(), func() (uint64, uint64, error) { return DiskUsage(r.Store.Root) }); err != nil {
					r.reportArchiveError("Не удалось очистить диск", err, time.Now())
				}
			}
		}
		if ctx.Err() != nil {
			return
		}
		a.publishStatus(r)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
func (a *App) CheckReady() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.runtime == nil {
		return fmt.Errorf("архив недоступен: %s", a.storageError)
	}
	return nil
}

// Reconciliation can probe a large interrupted buffer without holding up new
// completion notifications, live events, HTTP handlers or update preparation.
func (a *App) runBufferReconciliation(ctx context.Context, r *Runtime) {
	for !r.workerReady.Load() {
		if !pause(ctx, 20*time.Millisecond) {
			return
		}
	}
	started := time.Now()
	if err := scanSegments(ctx, r.Store, a.Bins.FFprobe, r.mediaSince.Load() > 0, r.Engine.AddSegment); err != nil && ctx.Err() == nil {
		r.reportArchiveError("Не удалось сверить буфер", err, time.Now())
	}
	r.bufferMS.Store(time.Since(started).Milliseconds())
	r.Engine.mu.Lock()
	r.Engine.deferRecovered = false
	r.Engine.mu.Unlock()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-r.bufferWake:
		}
		if err := r.reconcileSegments(ctx); err != nil && ctx.Err() == nil {
			r.reportArchiveError("Не удалось сверить буфер", err, time.Now())
		}
	}
}
