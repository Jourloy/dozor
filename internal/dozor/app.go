package dozor

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

type Runtime struct {
	Store         *Store
	Engine        *Engine
	cancel        context.CancelFunc
	wg            sync.WaitGroup
	segments      chan Segment
	mediaSince    atomic.Int64
	streams       map[string]AvailabilityInterval
	streamSignals chan StreamSignal
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
}

func NewApp(c *ConfigFile, b Binaries, dev bool, state, socket, version string) (*App, error) {
	if e := os.MkdirAll(state, 0700); e != nil {
		return nil, e
	}
	a := &App{Config: c, Bins: b, Development: dev, StateDir: state, Socket: socket, Version: version, reload: make(chan struct{}, 1), sessions: map[string]Session{}, attempts: map[string]loginAttempt{}}
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
	c := a.Config.Get()
	g := Guard{c.Archive, c.DiskUUID, a.Development}
	if e := g.Check(); e != nil {
		return e
	}
	s, e := OpenStore(g)
	if e != nil {
		return e
	}
	rctx, cancel := context.WithCancel(ctx)
	r := &Runtime{Store: s, Engine: NewEngine(s, a.Bins), cancel: cancel, segments: make(chan Segment, 512)}
	if e = ScanSegments(rctx, s, a.Bins.FFprobe, false); e != nil {
		s.Close()
		cancel()
		return e
	}
	// Try interrupted events before starting new streams. A failed remux keeps
	// its buffer pinned for later ticks and must not prevent recording startup.
	if e = r.Engine.Tick(rctx, time.Now()); e != nil {
		if !errors.Is(e, errVideoAssembly) {
			s.Close()
			cancel()
			return e
		}
		s.Notice("Не удалось восстановить часть записей; повтор будет выполнен автоматически")
	}
	if e = r.initStreams(c.Cameras, time.Now()); e != nil {
		s.Close()
		cancel()
		return e
	}
	if c.S3.Enabled {
		if e = s.SetTarget(c.S3); e != nil {
			s.Close()
			cancel()
			return e
		}
	}
	a.runtime = r
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
		})
	})
	for _, cam := range c.Cameras {
		if cam.Enabled {
			launch(func() { RunMotion(rctx, cam, a.Bins, r.Engine.Signal, s.Notice) })
		}
	}
	if c.S3.Enabled {
		launch(func() {
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
	return nil
}
func (a *App) stop() {
	if a.runtime == nil {
		return
	}
	r := a.runtime
	a.runtime = nil
	r.cancel()
	r.wg.Wait()
	finishCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := r.finishDisabledStreams(finishCtx, a.Config.Get().Cameras, time.Now()); err != nil {
		r.Store.Notice("Не удалось завершить запись выключенной камеры; повтор при восстановлении")
	}
	_ = r.Store.Close()
}
func (a *App) Run(ctx context.Context) error {
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
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	iteration := 0
	defer func() { a.mu.Lock(); a.stop(); a.mu.Unlock() }()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-a.reload:
			a.mu.Lock()
			a.stop()
			a.mu.Unlock()
		case <-tick.C:
			a.mu.Lock()
			if a.runtime == nil {
				if e := a.start(ctx); e != nil {
					a.storageError = e.Error()
				} else {
					a.storageError = ""
				}
				a.mu.Unlock()
				continue
			}
			r := a.runtime
			if e := r.Store.Guard.Check(); e != nil {
				a.storageError = e.Error()
				a.stop()
				a.mu.Unlock()
				continue
			}
			for len(r.segments) > 0 {
				seg := <-r.segments
				if e := r.Engine.AddSegment(seg); e != nil {
					r.Store.Notice("Ошибка каталога фрагментов")
				}
			}
			if e := r.tickStreams(ctx, time.Now()); e != nil {
				r.Store.Notice("Не удалось обновить доступность камер")
			}
			if e := r.Engine.Tick(ctx, time.Now()); e != nil {
				r.Store.Notice("Ошибка обработки архива; повтор будет выполнен")
			}
			iteration++
			if iteration%15 == 0 {
				if e := r.reconcileSegments(ctx); e != nil {
					r.Store.Notice("Не удалось сверить буфер")
				}
				if !a.Development {
					if e := r.Store.PruneArchive(func() (uint64, uint64, error) { return DiskUsage(r.Store.Root) }); e != nil {
						r.Store.Notice("Не удалось очистить диск")
					}
				}
			}
			a.mu.Unlock()
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
		if _, ok := a.runtime.streams[v.CameraID]; !ok {
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
		if a.runtime != nil && !a.runtime.Engine.PrepareUpdate() {
			apiError(w, 409, "активное событие")
			return
		}
		w.WriteHeader(204)
	})
	m.HandleFunc("GET /health", func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		defer a.mu.RUnlock()
		ready := false
		if a.runtime != nil {
			since := a.runtime.mediaSince.Load()
			ready = since > 0 && time.Now().Unix()-since >= 2
		} else {
			c := a.Config.Get()
			ready = (Guard{Root: c.Archive, UUID: c.DiskUUID, Development: a.Development}).Check() != nil
		}
		jsonOut(w, 200, map[string]any{"version": a.Version, "ready": ready})
	})
	return m
}
func UnixClient(socket string) *http.Client {
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", socket)
	}}}
}
func (a *App) Status() map[string]any {
	a.mu.RLock()
	defer a.mu.RUnlock()
	c := a.Config.Get()
	m := map[string]any{"version": a.Version, "storage_error": a.storageError, "disk_ready": a.runtime != nil, "queue": 0, "cameras": []any{}, "notices": []Notice{}}
	if r := a.runtime; r != nil {
		m["queue"] = r.Store.QueueCount()
		var uploadError string
		_ = r.Store.DB.QueryRow("SELECT error FROM jobs WHERE error LIKE 'S3 %' ORDER BY next DESC LIMIT 1").Scan(&uploadError)
		m["upload_error"] = uploadError
		m["notices"] = r.Store.Notices()
		used, total, _ := DiskUsage(r.Store.Root)
		m["disk_used"] = used
		m["disk_total"] = total
		states := r.Engine.States()
		cams := []any{}
		for _, cam := range c.Cameras {
			var last int64
			_ = r.Store.DB.QueryRow("SELECT COALESCE(MAX(end),0) FROM segments WHERE camera=?", cam.ID).Scan(&last)
			st := states[cam.ID]
			online := cam.Enabled && r.mediaSince.Load() > 0 && r.streams[cam.ID].State == "online"
			cams = append(cams, map[string]any{"id": cam.ID, "name": cam.Name, "enabled": cam.Enabled, "online": online, "last_segment": last, "motion": st.Active, "detector_healthy": st.Healthy, "source": st.Source})
		}
		m["cameras"] = cams
	}
	if b, e := os.ReadFile(filepath.Join(a.StateDir, "update-status.json")); e == nil {
		m["update_status"] = string(b)
	}
	return m
}
func (a *App) CheckReady() error {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.runtime == nil {
		return fmt.Errorf("архив недоступен: %s", a.storageError)
	}
	return nil
}
