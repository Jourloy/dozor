package dozor

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

type Binaries struct{ MediaMTX, FFmpeg, FFprobe, Self string }
type ProbeResult struct {
	Video             string  `json:"video"`
	Audio             string  `json:"audio"`
	Width             int     `json:"width"`
	Height            int     `json:"height"`
	BrowserCompatible bool    `json:"browser_compatible"`
	Duration          float64 `json:"duration"`
}

var errNoVideo = errors.New("в потоке нет видео")

func Probe(ctx context.Context, bin, input string, rtsp bool) (ProbeResult, error) {
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	args := []string{"-v", "error"}
	if rtsp {
		args = append(args, "-rtsp_transport", "tcp", "-timeout", "8000000")
	}
	args = append(args, "-show_streams", "-show_format", "-of", "json", input)
	cmd := exec.CommandContext(ctx, bin, args...)
	var out limitedBuffer
	out.limit = 2 << 20
	cmd.Stdout = &out
	var stderr diagnosticBuffer
	cmd.Stderr = &stderr
	var cameraOutput *diagnosticLines
	if rtsp {
		cameraOutput = cameraCommandOutput(&stderr, input)
		cmd.Stderr = cameraOutput
	}
	var p ProbeResult
	err := cmd.Run()
	if cameraOutput != nil {
		cameraOutput.Flush()
	}
	if err != nil {
		if rtsp {
			return p, fmt.Errorf("не удалось получить видео: %w", cameraCommandError(ctx, "ffprobe", err, &stderr, input))
		}
		return p, mediaCommandError(ctx, "ffprobe", err, &stderr)
	}
	var v struct {
		Streams []struct {
			CodecName     string `json:"codec_name"`
			CodecType     string `json:"codec_type"`
			Width, Height int
		}
		Format struct{ Duration string }
	}
	if err := json.Unmarshal(out.data, &v); err != nil {
		return p, err
	}
	for _, s := range v.Streams {
		if s.CodecType == "video" {
			p.Video = s.CodecName
			p.Width = s.Width
			p.Height = s.Height
		}
		if s.CodecType == "audio" {
			p.Audio = s.CodecName
		}
	}
	p.Duration, _ = strconv.ParseFloat(v.Format.Duration, 64)
	p.BrowserCompatible = p.Video == "h264" && (p.Audio == "" || p.Audio == "aac")
	if p.Video == "" {
		return p, errNoVideo
	}
	return p, nil
}

type limitedBuffer struct {
	data  []byte
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(b.data)+n > b.limit {
		return 0, errors.New("output too large")
	}
	b.data = append(b.data, p...)
	return n, nil
}

const diagnosticLimit = 2048

// Drain all stderr even after the retained prefix is full. Returning a write
// error would close the pipe and could turn a successful media command into a failure.
type diagnosticBuffer struct {
	data      []byte
	truncated bool
}

func (b *diagnosticBuffer) Write(p []byte) (int, error) {
	n := min(len(p), diagnosticLimit-len(b.data))
	b.data = append(b.data, p[:n]...)
	b.truncated = b.truncated || n < len(p)
	return len(p), nil
}

func mediaCommandError(ctx context.Context, tool string, err error, stderr *diagnosticBuffer) error {
	if ctx.Err() != nil {
		err = ctx.Err()
	}
	detail := strings.Join(strings.Fields(strings.ToValidUTF8(string(stderr.data), "�")), " ")
	if stderr.truncated {
		detail += " …"
	}
	if detail == "" {
		return fmt.Errorf("%s: %w", tool, err)
	}
	return fmt.Errorf("%s: %w: %s", tool, err, detail)
}

func HashFile(path string) (sha, md string, size int64, err error) {
	f, e := os.Open(path)
	if e != nil {
		err = e
		return
	}
	defer f.Close()
	s := sha256.New()
	m := md5.New()
	size, err = io.Copy(io.MultiWriter(s, m), f)
	sha = hex.EncodeToString(s.Sum(nil))
	md = base64.StdEncoding.EncodeToString(m.Sum(nil))
	return
}
func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
func MediaConfig(c Config, b Binaries, socket string) ([]byte, error) {
	hook := shellQuote(b.Self) + " hook --socket " + shellQuote(socket)
	paths := map[string]any{}
	for _, cam := range c.Cameras {
		if !cam.Enabled {
			continue
		}
		paths[cam.ID] = map[string]any{"source": CameraURL(cam, false), "rtspTransport": "tcp", "record": true}
	}
	v := map[string]any{"logLevel": "error", "logDestinations": []string{"stdout"}, "logStructured": false, "rtspAddress": "127.0.0.1:8554", "rtspTransports": []string{"tcp"}, "moq": false, "rtmp": false, "webrtc": false, "srt": false, "api": false, "playback": false, "paths": paths,
		// Remux on demand in RAM. fMP4 also works in native Safari over local HTTP.
		"hls": true, "hlsAddress": liveAddress, "hlsAllowOrigins": []string{}, "hlsAlwaysRemux": false,
		"hlsVariant": "fmp4", "hlsSegmentCount": 7, "hlsSegmentDuration": "1s", "hlsSegmentMaxSize": "16M", "hlsDirectory": "", "hlsMuxerCloseAfter": "15s",
		"authInternalUsers": []any{map[string]any{"user": "any", "ips": []string{"127.0.0.1", "::1"}, "permissions": []any{map[string]any{"action": "read"}}}},
		"pathDefaults":      map[string]any{"recordPath": filepath.Join(c.Archive, "buffer", "%path", "%Y-%m-%d_%H-%M-%S.%f"), "recordFormat": "fmp4", "recordPartDuration": "1s", "recordMaxPartSize": "8M", "recordSegmentDuration": "5s", "recordDeleteAfter": "0s", "runOnRecordSegmentComplete": hook, "runOnOnline": hook + " --stream online", "runOnOffline": hook + " --stream offline"}}
	return json.MarshalIndent(v, "", "  ") // JSON is a YAML subset; avoids interpolating credentials into syntax.
}
func RunMedia(ctx context.Context, c Config, b Binaries, socket, configPath string, notice func(string), ready func(bool), failed func(Camera, string)) {
	reportFailure := func(message string) {
		notice(message)
		for _, camera := range c.Cameras {
			if camera.Enabled {
				failed(camera, message)
			}
		}
	}
	data, e := MediaConfig(c, b, socket)
	if e == nil {
		e = AtomicWrite(configPath, data, 0600)
	}
	if e != nil {
		reportFailure("Не удалось подготовить MediaMTX: " + e.Error())
		return
	}
	for ctx.Err() == nil {
		cmd := exec.CommandContext(ctx, b.MediaMTX, configPath)
		cmd.Env = append(os.Environ(), "TZ=UTC")
		output, global := mediaDiagnostics(c, notice, func(camera Camera, message string) {
			if ctx.Err() == nil {
				failed(camera, message)
			}
		})
		// Sharing a writer makes os/exec serialize stdout and stderr writes.
		cmd.Stdout = output
		cmd.Stderr = output
		cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
		cmd.WaitDelay = 8 * time.Second
		e = cmd.Start()
		if e == nil {
			ready(true)
			e = cmd.Wait()
		}
		output.Flush()
		ready(false)
		if ctx.Err() != nil {
			return
		}
		if e == nil {
			e = errors.New("процесс завершился без ошибки")
		}
		reportFailure("MediaMTX остановился; повторный запуск через 5 секунд: " + mediaCommandError(ctx, "MediaMTX", e, global).Error())
		if !pause(ctx, 5*time.Second) {
			return
		}
	}
}
func pause(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
func ParseSegment(root, path string, duration float64) (Segment, error) {
	rel, e := filepath.Rel(root, path)
	if e != nil || !filepath.IsLocal(rel) {
		return Segment{}, errors.New("segment outside archive")
	}
	v := strings.Split(filepath.ToSlash(rel), "/")
	if len(v) != 3 || v[0] != "buffer" || !safeID.MatchString(v[1]) || !strings.HasSuffix(v[2], ".mp4") {
		return Segment{}, errors.New("invalid segment path")
	}
	t, e := time.Parse("2006-01-02_15-04-05.000000", strings.TrimSuffix(v[2], ".mp4"))
	if e != nil || duration <= 0 || duration > 3600 {
		return Segment{}, errors.New("invalid segment timestamp or duration")
	}
	return Segment{Path: rel, CameraID: v[1], Start: t.UnixMilli(), End: t.Add(time.Duration(duration * float64(time.Second))).UnixMilli()}, nil
}

// Exclude the newest file per camera while the recorder is alive. A following file
// proves rotation even if a completion hook was lost. Startup recovery scans all.
func ScanSegments(ctx context.Context, s *Store, probe string, recorderAlive bool) error {
	return scanSegments(ctx, s, probe, recorderAlive, s.AddSegment)
}

func scanSegments(ctx context.Context, s *Store, probe string, recorderAlive bool, register func(Segment) error) error {
	cams, e := os.ReadDir(filepath.Join(s.Root, "buffer"))
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	for _, cam := range cams {
		if !cam.IsDir() || !safeID.MatchString(cam.Name()) {
			continue
		}
		dir := filepath.Join(s.Root, "buffer", cam.Name())
		files, e := filepath.Glob(filepath.Join(dir, "*.mp4"))
		if e != nil {
			return e
		}
		sort.Strings(files)
		if recorderAlive && len(files) > 0 {
			files = files[:len(files)-1]
		}
		if e := scanSegmentFiles(ctx, s, probe, files, 0, register); e != nil {
			return e
		}
	}
	return nil
}

func scanCameraSegments(ctx context.Context, s *Store, probe, camera string, until int64, register func(Segment) error) error {
	files, err := filepath.Glob(filepath.Join(s.Root, "buffer", camera, "*.mp4"))
	if err != nil {
		return err
	}
	return scanSegmentFiles(ctx, s, probe, files, until, register)
}

func scanSegmentFiles(ctx context.Context, s *Store, probe string, files []string, until int64, register func(Segment) error) error {
	for _, path := range files {
		if until > 0 {
			seg, err := ParseSegment(s.Root, path, 1)
			if err != nil || seg.Start >= until {
				continue
			}
		}
		rel, _ := filepath.Rel(s.Root, path)
		if s.HasSegment(rel) {
			continue
		}
		p, e := Probe(ctx, probe, path, false)
		if e != nil {
			continue
		}
		seg, e := ParseSegment(s.Root, path, p.Duration)
		if e != nil {
			continue
		}
		if e = register(seg); e != nil {
			return e
		}
	}
	return nil
}

// Video tool failures can be retried without taking a healthy archive offline.
// Filesystem and catalog errors must still propagate as storage failures.
var errVideoAssembly = errors.New("не удалось собрать MP4")

// Only input-data failures qualify for deletion. A missing executable, timeout,
// or storage failure says nothing about whether the recording is usable.
func invalidVideoData(err error) bool {
	if errors.Is(err, errNoVideo) {
		return true
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, detail := range []string{"permission denied", "operation not permitted", "input/output error", "no space left", "read-only file system", "cannot allocate memory", "resource temporarily unavailable"} {
		if strings.Contains(message, detail) {
			return false
		}
	}
	for _, detail := range []string{"moov atom not found", "invalid data found", "matches no streams", "does not contain any stream", "no such file or directory"} {
		if strings.Contains(message, detail) {
			return true
		}
	}
	return false
}

func Assemble(ctx context.Context, s *Store, b Binaries, ev Event, segs []Segment) (Part, error) {
	p := Part{ID: ID(), EventID: ev.ID, CameraID: ev.CameraID, Start: segs[0].Start, End: segs[len(segs)-1].End}
	p.Path = filepath.Join(eventDir(ev), fmt.Sprintf("%d-%s.mp4", p.Start, p.ID))
	for i := 1; i < len(segs); i++ {
		if segs[i].Start > segs[i-1].End+1500 {
			p.Gaps = append(p.Gaps, Gap{segs[i-1].End, segs[i].Start})
		}
	}
	path := filepath.Join(s.Root, p.Path)
	if e := s.Guard.Check(); e != nil {
		return p, e
	}
	if e := os.MkdirAll(filepath.Dir(path), 0700); e != nil {
		return p, e
	}
	list, e := os.CreateTemp(filepath.Join(s.Root, "buffer"), "concat-*.txt")
	if e != nil {
		return p, e
	}
	defer os.Remove(list.Name())
	for _, seg := range segs { // Paths consist exclusively of generated camera IDs and timestamps.
		rel, e := filepath.Rel(filepath.Dir(list.Name()), filepath.Join(s.Root, seg.Path))
		if e != nil {
			list.Close()
			return p, e
		}
		if _, e = fmt.Fprintf(list, "file '%s'\n", filepath.ToSlash(rel)); e != nil {
			list.Close()
			return p, e
		}
	}
	if e = list.Close(); e != nil {
		return p, e
	}
	partial := path + ".partial"
	defer os.Remove(partial)
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, b.FFmpeg, "-nostdin", "-v", "error", "-f", "concat", "-safe", "1", "-i", list.Name(), "-map", "0:v:0", "-map", "0:a?", "-c", "copy", "-movflags", "+faststart", "-f", "mp4", "-y", partial)
	var stderr diagnosticBuffer
	cmd.Stderr = &stderr
	if e = cmd.Run(); e != nil {
		return p, fmt.Errorf("%w: %w", errVideoAssembly, mediaCommandError(ctx, "ffmpeg", e, &stderr))
	}
	if _, e = Probe(ctx, b.FFprobe, partial, false); e != nil {
		return p, fmt.Errorf("%w: %w", errVideoAssembly, e)
	}
	p.SHA256, p.MD5, p.Size, e = HashFile(partial)
	if e != nil {
		return p, e
	}
	f, e := os.OpenFile(partial, os.O_RDWR, 0600)
	if e != nil {
		return p, e
	}
	e = f.Sync()
	f.Close()
	if e != nil {
		return p, e
	}
	// Durable recovery descriptor precedes final publication.
	if e = WriteJSON(path+".json", p); e != nil {
		return p, e
	}
	if e = s.Guard.Check(); e != nil {
		return p, e
	}
	if e = os.Rename(partial, path); e != nil {
		return p, e
	}
	if e = s.SavePart(p); e != nil {
		return p, e
	}
	if e = s.Enqueue("part", p.ID); e != nil {
		return p, e
	}
	return p, nil
}
