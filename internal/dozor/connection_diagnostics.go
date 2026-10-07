package dozor

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The latest failure is independent of the bounded journal and survives both
// reconnects and restarts. It describes an observation, not the current state.
type CameraFailure struct {
	At      int64  `json:"at"`
	Message string `json:"message"`
}

func (s *Store) SaveCameraFailure(camera, kind string, failure CameraFailure) error {
	_, err := s.DB.Exec(`INSERT INTO camera_diagnostics(camera,kind,at,message) VALUES(?,?,?,?)
 ON CONFLICT(camera,kind) DO UPDATE SET at=excluded.at,message=excluded.message
 WHERE excluded.at>=camera_diagnostics.at`, camera, kind, failure.At, failure.Message)
	return err
}

func (s *Store) LastCameraFailure(camera, kind string) (*CameraFailure, error) {
	var failure CameraFailure
	err := s.DB.QueryRow("SELECT at,message FROM camera_diagnostics WHERE camera=? AND kind=?", camera, kind).Scan(&failure.At, &failure.Message)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &failure, nil
}

func (r *Runtime) reportCameraFailure(camera Camera, kind, message string, now time.Time) {
	message = cameraDiagnostic(message, CameraURL(camera, false), CameraURL(camera, true))
	if message == "" {
		return
	}
	failure := CameraFailure{At: now.UnixMilli(), Message: message}
	if err := r.Store.SaveCameraFailure(camera.ID, kind, failure); err != nil {
		log.Printf("Не удалось сохранить диагностику камеры %s: %v", camera.ID, err)
	}
	// MediaMTX retries every five seconds. Keep the last attempt up to date,
	// but do not let identical retries fill the journal or journald.
	r.diagnosticMu.Lock()
	defer r.diagnosticMu.Unlock()
	if r.diagnosticNotices == nil {
		r.diagnosticNotices = make(map[string]CameraFailure)
	}
	key := camera.ID + ":" + kind
	previous := r.diagnosticNotices[key]
	if previous.Message == message && failure.At-previous.At < time.Minute.Milliseconds() {
		return
	}
	r.diagnosticNotices[key] = failure
	prefix := "Не удалось подключиться к камере"
	if kind == "detector" {
		prefix = "Детектор недоступен, временная непрерывная запись"
	}
	notice := fmt.Sprintf("%s %s (%s): %s", prefix, camera.Name, camera.ID, message)
	r.Store.Notice(notice)
	log.Print(notice)
}

var diagnosticURL = regexp.MustCompile(`(?i)(?:rtsp|rtsps|http|https)://[^\s"'<>]+`)
var diagnosticAddress = regexp.MustCompile(` @ 0x[0-9a-fA-F]+\]`)
var diagnosticEscape = regexp.MustCompile(`%[0-9a-fA-F]{2}`)

// Redact before normalizing/truncating: a byte limit must never leave a partial
// password in a public diagnostic. URLs can also carry credentials in queries.
func cameraDiagnostic(message string, inputs ...string) string {
	secrets := make(map[string]bool)
	for _, input := range inputs {
		u, err := url.Parse(input)
		if err != nil || u.User == nil {
			continue
		}
		password, _ := u.User.Password()
		for _, secret := range []string{password, u.User.Username()} {
			if secret == "" {
				continue
			}
			for _, value := range []string{url.QueryEscape(secret), url.PathEscape(secret), url.User(secret).String(), secret} {
				secrets[value] = true
				secrets[diagnosticEscape.ReplaceAllStringFunc(value, strings.ToLower)] = true
			}
		}
	}
	var values []string
	for value := range secrets {
		values = append(values, value)
	}
	// Replace overlapping credentials together, including decoded whitespace,
	// before masking URLs (which are delimited by whitespace in tool output).
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	var replacements []string
	for _, value := range values {
		replacements = append(replacements, value, "[скрыто]")
	}
	message = strings.NewReplacer(replacements...).Replace(message)
	message = diagnosticURL.ReplaceAllString(message, "[адрес камеры]")
	// FFmpeg object addresses change on every retry and are not diagnostic.
	message = diagnosticAddress.ReplaceAllString(message, "]")
	message = strings.Join(strings.Fields(strings.ToValidUTF8(message, "�")), " ")
	if len(message) > diagnosticLimit {
		message = strings.ToValidUTF8(message[:diagnosticLimit], "") + " …"
	}
	return message
}

// Drain oversized lines instead of stopping the child or retaining unbounded
// output. Drop the entire line so that even a split credential cannot leak.
type diagnosticLines struct {
	pending  []byte
	overflow bool
	emit     func(string)
}

func (w *diagnosticLines) Write(p []byte) (int, error) {
	n := len(p)
	for len(p) > 0 {
		end := bytes.IndexByte(p, '\n')
		part := p
		if end >= 0 {
			part = p[:end]
		}
		if !w.overflow {
			if len(w.pending)+len(part) > 64<<10 {
				w.pending = nil
				w.overflow = true
			} else {
				w.pending = append(w.pending, part...)
			}
		}
		if end < 0 {
			break
		}
		w.Flush()
		p = p[end+1:]
	}
	return n, nil
}

func (w *diagnosticLines) Flush() {
	if w.overflow {
		w.emit("Слишком длинное диагностическое сообщение пропущено")
	} else if len(w.pending) > 0 {
		w.emit(string(w.pending))
	}
	w.pending = w.pending[:0]
	w.overflow = false
}

func cameraCommandError(ctx context.Context, tool string, err error, output *diagnosticBuffer, inputs ...string) error {
	// output contains only complete, redacted lines; command/start errors still
	// need redaction, and cancellation must remain detectable by callers.
	cause := err
	if ctx.Err() != nil {
		cause = ctx.Err()
	}
	detail := cameraDiagnostic(mediaCommandError(ctx, tool, err, output).Error(), inputs...)
	return &cameraToolError{message: detail, cause: cause}
}

type cameraToolError struct {
	message string
	cause   error
}

func (e *cameraToolError) Error() string { return e.message }
func (e *cameraToolError) Unwrap() error { return e.cause }

func cameraCommandOutput(output *diagnosticBuffer, inputs ...string) *diagnosticLines {
	return &diagnosticLines{emit: func(line string) {
		_, _ = output.Write([]byte(cameraDiagnostic(line, inputs...) + "\n"))
	}}
}

var mediaSourceError = regexp.MustCompile(`(?:^|\s)ERR \[path ([a-f0-9]{32})\] \[RTSP source\] (.+)$`)

func mediaDiagnostics(c Config, notice func(string), failed func(Camera, string)) (*diagnosticLines, *diagnosticBuffer) {
	cameras := make(map[string]Camera)
	var inputs []string
	for _, camera := range c.Cameras {
		if camera.Enabled {
			cameras[camera.ID] = camera
			inputs = append(inputs, CameraURL(camera, false), CameraURL(camera, true))
		}
	}
	var global diagnosticBuffer
	return &diagnosticLines{emit: func(line string) {
		if match := mediaSourceError.FindStringSubmatch(line); match != nil {
			if camera, ok := cameras[match[1]]; ok {
				failed(camera, cameraDiagnostic("RTSP: "+match[2], inputs...))
			}
			return
		}
		detail := cameraDiagnostic(line, inputs...)
		if detail == "" {
			return
		}
		notice("MediaMTX: " + detail)
		if !strings.Contains(line, "[path ") {
			_, _ = global.Write([]byte(detail + "\n"))
		}
	}}, &global
}
