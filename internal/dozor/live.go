package dozor

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"time"
)

const liveAddress = "127.0.0.1:8888"

// MediaMTX playlists use relative, single-component filenames. Never expose its
// HTML player or accept another path, camera URL, or upstream from the client.
var liveFile = regexp.MustCompile(`^[a-zA-Z0-9_-]+\.(m3u8|mp4)$`)
var liveSession = regexp.MustCompile(`^[a-f0-9]{8}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{4}-[a-f0-9]{12}$`)

var liveTransport = &http.Transport{
	DialContext:           (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
	ResponseHeaderTimeout: 15 * time.Second,
	IdleConnTimeout:       30 * time.Second,
	MaxIdleConnsPerHost:   8,
}

func (a *App) liveHandler(transport http.RoundTripper) http.HandlerFunc {
	proxy := &httputil.ReverseProxy{
		Transport: transport,
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(&url.URL{Scheme: "http", Host: liveAddress})
			p.Out.URL.Path = "/" + p.In.PathValue("id") + "/" + p.In.PathValue("file")
			p.Out.URL.RawPath = ""
			// MediaMTX 1.21 first tests cookie support with a redirect. Select its
			// cookieless mode and preserve the session carried by relative HLS URLs.
			// The viewer's Dozor cookie and all other query parameters stay private.
			query := url.Values{"cookieCheck": {"1"}}
			if session := p.In.URL.Query().Get("session"); session != "" {
				query.Set("session", session)
			}
			p.Out.URL.RawQuery = query.Encode()
			p.Out.Header = make(http.Header)
			for _, key := range []string{"Range", "If-Range"} {
				if value := p.In.Header.Get(key); value != "" {
					p.Out.Header.Set(key, value)
				}
			}
		},
		ModifyResponse: func(r *http.Response) error {
			if r.StatusCode != http.StatusOK && r.StatusCode != http.StatusPartialContent && r.StatusCode != http.StatusRequestedRangeNotSatisfiable {
				return errors.New("live stream unavailable")
			}
			// Keep only media headers. In particular, do not expose upstream CORS,
			// cookies or redirects, or let it override our no-store policy.
			headers := make(http.Header)
			for _, key := range []string{"Content-Type", "Content-Length", "Content-Range", "Accept-Ranges"} {
				if value := r.Header.Get(key); value != "" {
					headers.Set(key, value)
				}
			}
			headers.Set("Cache-Control", "no-store")
			r.Header = headers
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, _ error) {
			if r.Context().Err() == context.Canceled {
				return
			}
			apiError(w, http.StatusBadGateway, "трансляция недоступна: проверьте подключение камеры и повторите")
		},
		ErrorLog: log.New(io.Discard, "", 0),
	}
	return func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !safeID.MatchString(id) || !liveFile.MatchString(r.PathValue("file")) {
			apiError(w, http.StatusNotFound, "трансляция не найдена")
			return
		}
		if session := r.URL.Query().Get("session"); session != "" && !liveSession.MatchString(session) {
			apiError(w, http.StatusBadRequest, "неверная сессия трансляции")
			return
		}
		var camera *Camera
		for _, c := range a.Config.Get().Cameras {
			if c.ID == id {
				camera = &c
				break
			}
		}
		if camera == nil {
			apiError(w, http.StatusNotFound, "камера не найдена")
			return
		}
		if !camera.Enabled {
			apiError(w, http.StatusConflict, "камера отключена: включите её в настройках")
			return
		}
		a.mu.RLock()
		ready := a.runtime != nil && a.runtime.mediaSince.Load() > 0
		a.mu.RUnlock()
		if !ready {
			apiError(w, http.StatusServiceUnavailable, "видеослужба недоступна: проверьте диск и дождитесь подключения камер")
			return
		}
		// A stalled viewer must not hold the application lock or a request open
		// indefinitely while the recorder is restarting or the disk disappears.
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		proxy.ServeHTTP(w, r.WithContext(ctx))
	}
}
