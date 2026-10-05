package dozor

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"golang.org/x/crypto/bcrypt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

//go:embed web/*
var webFiles embed.FS

type Session struct {
	CSRF    string
	Expires time.Time
}
type loginAttempt struct {
	Count int
	Until time.Time
}

func jsonOut(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func apiError(w http.ResponseWriter, status int, msg string) {
	jsonOut(w, status, map[string]string{"error": msg})
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		apiError(w, 400, "неверный JSON")
		return false
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		apiError(w, 400, "ожидался один JSON-объект")
		return false
	}
	return true
}
func sameOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o == "" {
		return true
	}
	u, e := url.Parse(o)
	return e == nil && u.Host == r.Host && (u.Scheme == "http" || u.Scheme == "https")
}
func (a *App) session(r *http.Request) (Session, bool) {
	c, e := r.Cookie("dozor_session")
	if e != nil {
		return Session{}, false
	}
	a.authMu.Lock()
	defer a.authMu.Unlock()
	s, ok := a.sessions[c.Value]
	if !ok || time.Now().After(s.Expires) {
		delete(a.sessions, c.Value)
		return Session{}, false
	}
	return s, true
}
func (a *App) protected(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s, ok := a.session(r)
		if !ok {
			apiError(w, 401, "нужен вход")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if !sameOrigin(r) || subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(s.CSRF)) != 1 {
				apiError(w, 403, "неверный CSRF-токен")
				return
			}
		}
		h(w, r)
	}
}
func (a *App) Handler() http.Handler {
	m := http.NewServeMux()
	assets, _ := fs.Sub(webFiles, "web")
	m.Handle("GET /", http.FileServer(http.FS(assets)))
	m.HandleFunc("GET /api/v1/auth", func(w http.ResponseWriter, r *http.Request) {
		s, ok := a.session(r)
		jsonOut(w, 200, map[string]any{"setup_required": a.Config.Get().PasswordHash == "", "authenticated": ok, "csrf": s.CSRF})
	})
	m.HandleFunc("POST /api/v1/login", a.login)
	m.HandleFunc("POST /api/v1/logout", a.protected(func(w http.ResponseWriter, r *http.Request) {
		c, _ := r.Cookie("dozor_session")
		a.authMu.Lock()
		delete(a.sessions, c.Value)
		a.authMu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "dozor_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteStrictMode})
		w.WriteHeader(204)
	}))
	m.HandleFunc("GET /api/v1/status", a.protected(func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, a.Status()) }))
	m.HandleFunc("GET /api/v1/settings", a.protected(func(w http.ResponseWriter, r *http.Request) { jsonOut(w, 200, PublicConfig(a.Config.Get())) }))
	m.HandleFunc("PUT /api/v1/settings", a.protected(func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			Timezone   string   `json:"timezone"`
			S3         S3Config `json:"s3"`
			AutoUpdate bool     `json:"auto_update"`
			ReleaseURL string   `json:"release_url"`
		}
		if !decode(w, r, &v) {
			return
		}
		c := a.Config.Get()
		old := c
		c.Timezone = v.Timezone
		c.S3 = v.S3
		c.AutoUpdate = v.AutoUpdate
		c.ReleaseURL = v.ReleaseURL
		RestoreSecrets(&c, old)
		if e := a.Config.Save(c); e != nil {
			apiError(w, 400, e.Error())
			return
		}
		a.Reload()
		jsonOut(w, 200, PublicConfig(c))
	}))
	m.HandleFunc("POST /api/v1/cameras/probe", a.protected(func(w http.ResponseWriter, r *http.Request) {
		var c Camera
		if !decode(w, r, &c) {
			return
		}
		a.cameraSecret(&c)
		if e := ValidateCamera(c); e != nil {
			apiError(w, 400, e.Error())
			return
		}
		p, e := Probe(r.Context(), a.Bins.FFprobe, CameraURL(c, false), true)
		if e != nil {
			apiError(w, 400, e.Error())
			return
		}
		jsonOut(w, 200, p)
	}))
	m.HandleFunc("PUT /api/v1/cameras/{id}", a.protected(a.saveCamera))
	m.HandleFunc("DELETE /api/v1/cameras/{id}", a.protected(func(w http.ResponseWriter, r *http.Request) {
		c := a.Config.Get()
		cams := []Camera{}
		for _, cam := range c.Cameras {
			if cam.ID != r.PathValue("id") {
				cams = append(cams, cam)
			}
		}
		c.Cameras = cams
		if e := a.Config.Save(c); e != nil {
			apiError(w, 500, "не удалось сохранить камеры")
			return
		}
		a.Reload()
		w.WriteHeader(204)
	}))
	m.HandleFunc("POST /api/v1/discovery", a.protected(func(w http.ResponseWriter, r *http.Request) {
		v, e := Discover(r.Context())
		if e != nil {
			apiError(w, 500, "поиск недоступен")
			return
		}
		jsonOut(w, 200, v)
	}))
	m.HandleFunc("POST /api/v1/profiles", a.protected(func(w http.ResponseWriter, r *http.Request) {
		var c Camera
		if !decode(w, r, &c) {
			return
		}
		a.cameraSecret(&c)
		if c.ONVIF == "" {
			apiError(w, 400, "укажите ONVIF endpoint")
			return
		}
		v, e := Profiles(r.Context(), c)
		if e != nil {
			apiError(w, 400, e.Error())
			return
		}
		jsonOut(w, 200, v)
	}))
	m.HandleFunc("GET /api/v1/events", a.protected(func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		defer a.mu.RUnlock()
		if a.runtime == nil {
			apiError(w, 503, "диск недоступен")
			return
		}
		from, to := int64(0), int64(1<<62)
		if date := r.URL.Query().Get("date"); date != "" {
			loc, _ := time.LoadLocation(a.Config.Get().Timezone)
			day, err := time.ParseInLocation("2006-01-02", date, loc)
			if err != nil {
				apiError(w, 400, "неверная дата")
				return
			}
			from = day.UnixMilli()
			to = day.AddDate(0, 0, 1).UnixMilli()
		}
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		if offset < 0 || offset > 10000000 {
			apiError(w, 400, "неверная страница")
			return
		}
		v, e := a.runtime.Store.EventPage(r.URL.Query().Get("camera"), from, to, offset)
		if e != nil {
			apiError(w, 500, "каталог недоступен")
			return
		}
		jsonOut(w, 200, v)
	}))
	m.HandleFunc("GET /api/v1/events/{id}", a.protected(func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		defer a.mu.RUnlock()
		if a.runtime == nil {
			apiError(w, 503, "диск недоступен")
			return
		}
		ev, e := a.runtime.Store.Event(r.PathValue("id"))
		if e != nil {
			apiError(w, 404, "событие не найдено")
			return
		}
		parts, e := a.runtime.Store.Parts(ev.ID)
		if e != nil {
			apiError(w, 500, "каталог недоступен")
			return
		}
		jsonOut(w, 200, map[string]any{"event": ev, "parts": parts})
	}))
	m.HandleFunc("GET /api/v1/parts/{id}/video", a.protected(a.video))
	m.HandleFunc("GET /api/v1/disks", a.protected(func(w http.ResponseWriter, r *http.Request) {
		v, e := Disks()
		if e != nil {
			apiError(w, 500, e.Error())
			return
		}
		jsonOut(w, 200, map[string]any{"disks": v, "development": a.Development, "selected_uuid": a.Config.Get().DiskUUID})
	}))
	m.HandleFunc("POST /api/v1/disks/select", a.protected(func(w http.ResponseWriter, r *http.Request) {
		var v struct {
			UUID string `json:"uuid"`
		}
		if !decode(w, r, &v) {
			return
		}
		if !diskUUIDPattern.MatchString(v.UUID) {
			apiError(w, 400, "неверный UUID")
			return
		}
		cmd := exec.CommandContext(r.Context(), "systemctl", "start", "dozor-disk-prepare@"+v.UUID+".service")
		cmd.Stdout = io.Discard
		cmd.Stderr = io.Discard
		if cmd.Run() != nil {
			apiError(w, 400, "не удалось подключить ext4: проверьте UUID, занятость диска и установку системного помощника")
			return
		}
		c := a.Config.Get()
		c.DiskUUID = v.UUID
		if e := a.Config.Save(c); e != nil {
			apiError(w, 500, "не удалось сохранить выбор диска")
			return
		}
		a.Reload()
		w.WriteHeader(204)
	}))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; media-src 'self'; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		m.ServeHTTP(w, r)
	})
}
func (a *App) cameraSecret(c *Camera) {
	for _, p := range a.Config.Get().Cameras {
		if p.ID == c.ID && c.Password == "" {
			c.Password = p.Password
		}
	}
}
func (a *App) saveCamera(w http.ResponseWriter, r *http.Request) {
	var cam Camera
	if !decode(w, r, &cam) {
		return
	}
	id := r.PathValue("id")
	if id == "new" {
		cam.ID = ID()
	} else {
		if !safeID.MatchString(id) {
			apiError(w, 400, "неверный ID")
			return
		}
		cam.ID = id
	}
	a.cameraSecret(&cam)
	if e := ValidateCamera(cam); e != nil {
		apiError(w, 400, e.Error())
		return
	}
	c := a.Config.Get()
	changed := true
	idx := -1
	for i, prev := range c.Cameras {
		if prev.ID == cam.ID {
			idx = i
			changed = prev.URL != cam.URL || prev.SubURL != cam.SubURL || prev.Username != cam.Username || prev.Password != cam.Password || !prev.Enabled && cam.Enabled
		}
	}
	if changed && cam.Enabled {
		if _, e := Probe(r.Context(), a.Bins.FFprobe, CameraURL(cam, false), true); e != nil {
			apiError(w, 400, e.Error())
			return
		}
		if cam.SubURL != "" {
			if _, e := Probe(r.Context(), a.Bins.FFprobe, CameraURL(cam, true), true); e != nil {
				apiError(w, 400, "дополнительный поток недоступен")
				return
			}
		}
	}
	if idx < 0 {
		if len(c.Cameras) >= 16 {
			apiError(w, 400, "не более 16 камер")
			return
		}
		c.Cameras = append(c.Cameras, cam)
	} else {
		c.Cameras[idx] = cam
	}
	if e := a.Config.Save(c); e != nil {
		apiError(w, 400, e.Error())
		return
	}
	a.Reload()
	jsonOut(w, 200, PublicConfig(c))
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if !sameOrigin(r) {
		apiError(w, 403, "неверный источник запроса")
		return
	}
	var v struct {
		Password string `json:"password"`
		Token    string `json:"token"`
	}
	if !decode(w, r, &v) {
		return
	}
	if len(v.Password) < 12 || len(v.Password) > 72 {
		apiError(w, 400, "пароль: от 12 до 72 байт")
		return
	}
	ip, _, _ := net.SplitHostPort(r.RemoteAddr)
	a.authMu.Lock()
	defer a.authMu.Unlock()
	now := time.Now()
	for k, x := range a.attempts {
		if now.After(x.Until) {
			delete(a.attempts, k)
		}
	}
	if len(a.attempts) > 1000 {
		apiError(w, 429, "повторите позже")
		return
	}
	at := a.attempts[ip]
	if at.Count >= 10 && now.Before(at.Until) {
		apiError(w, 429, "слишком много попыток")
		return
	}
	at.Count++
	at.Until = now.Add(10 * time.Minute)
	a.attempts[ip] = at
	c := a.Config.Get()
	if c.PasswordHash == "" {
		if a.setupToken == "" || subtle.ConstantTimeCompare([]byte(v.Token), []byte(a.setupToken)) != 1 {
			apiError(w, 403, "неверный код первого запуска")
			return
		}
		h, e := bcrypt.GenerateFromPassword([]byte(v.Password), 12)
		if e != nil {
			apiError(w, 500, "ошибка пароля")
			return
		}
		c.PasswordHash = string(h)
		if a.Config.Save(c) != nil {
			apiError(w, 500, "не удалось сохранить пароль")
			return
		}
		a.setupToken = ""
		_ = os.Remove(filepath.Join(a.StateDir, "setup-token"))
	} else if bcrypt.CompareHashAndPassword([]byte(c.PasswordHash), []byte(v.Password)) != nil {
		apiError(w, 401, "неверный пароль")
		return
	}
	delete(a.attempts, ip)
	for k, s := range a.sessions {
		if now.After(s.Expires) {
			delete(a.sessions, k)
		}
	}
	if len(a.sessions) >= 100 {
		apiError(w, 429, "слишком много сессий")
		return
	}
	id := ID() + ID()
	s := Session{ID(), now.Add(12 * time.Hour)}
	a.sessions[id] = s
	http.SetCookie(w, &http.Cookie{Name: "dozor_session", Value: id, Path: "/", HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: 43200})
	jsonOut(w, 200, map[string]string{"csrf": s.CSRF})
}
func (a *App) video(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	if a.runtime == nil {
		a.mu.RUnlock()
		apiError(w, 503, "диск недоступен")
		return
	}
	s := a.runtime.Store
	p, e := s.Part(r.PathValue("id"))
	if e != nil || p.Deleted {
		a.mu.RUnlock()
		apiError(w, 404, "запись отсутствует на диске")
		return
	}
	if s.Guard.Check() != nil {
		a.mu.RUnlock()
		apiError(w, 503, "диск отключён")
		return
	}
	root, e := os.OpenRoot(s.Root)
	if e != nil {
		a.mu.RUnlock()
		apiError(w, 503, "диск недоступен")
		return
	}
	f, e := root.Open(p.Path)
	root.Close()
	a.mu.RUnlock()
	if e != nil {
		apiError(w, 404, "запись недоступна")
		return
	}
	defer f.Close()
	fi, e := f.Stat()
	if e != nil {
		apiError(w, 500, "ошибка файла")
		return
	}
	w.Header().Set("Content-Type", "video/mp4")
	if r.URL.Query().Get("download") == "1" {
		w.Header().Set("Content-Disposition", `attachment; filename="`+p.ID+`.mp4"`)
	}
	http.ServeContent(w, r, p.ID+".mp4", fi.ModTime(), f)
}

var diskUUIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{8}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{4}-[a-fA-F0-9]{12}$`)
