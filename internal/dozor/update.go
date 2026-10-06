package dozor

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const DefaultReleaseURL = "https://github.com/Jourloy/dozor/releases/latest/download/release.json"

type Release struct {
	Version  string `json:"version"`
	Platform string `json:"platform"`
	URL      string `json:"url"`
	SHA256   string `json:"sha256"`
	Size     int64  `json:"size"`
}
type SignedRelease struct {
	Release   Release `json:"release"`
	Signature string  `json:"signature"`
}
type UpdateJournal struct {
	Previous     string `json:"previous"`
	Candidate    string `json:"candidate"`
	SystemBackup string `json:"system_backup,omitempty"`
}
type Updater struct {
	Root      string
	PublicKey ed25519.PublicKey
	Client    *http.Client
	Stop      func() error
	Start     func() error
	Healthy   func(context.Context, string) bool
	System    *SystemIntegration
	Immediate bool
}

var versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

func Newer(a, b string) bool {
	if !versionPattern.MatchString(a) {
		return false
	}
	if !versionPattern.MatchString(b) {
		return true
	}
	av := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bv := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := range av {
		if len(av[i]) != len(bv[i]) {
			return len(av[i]) > len(bv[i])
		}
		if av[i] != bv[i] {
			return av[i] > bv[i]
		}
	}
	return false
}
func VerifyRelease(s SignedRelease, key ed25519.PublicKey) error {
	b, e := json.Marshal(s.Release)
	if e != nil {
		return e
	}
	sig, e := base64.StdEncoding.DecodeString(s.Signature)
	if e != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, b, sig) {
		return errors.New("неверная подпись обновления")
	}
	r := s.Release
	u, e := url.Parse(r.URL)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || !versionPattern.MatchString(r.Version) || r.Platform != "linux-arm64" || r.Size <= 0 || r.Size > 512<<20 {
		return errors.New("недопустимый пакет обновления")
	}
	h, e := hex.DecodeString(r.SHA256)
	if e != nil || len(h) != 32 {
		return errors.New("неверный SHA256 пакета")
	}
	return nil
}
func (u *Updater) client() *http.Client {
	if u.Client != nil {
		return u.Client
	}
	return &http.Client{Timeout: 15 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) > 5 || req.URL.Scheme != "https" {
			return errors.New("unsafe update redirect")
		}
		return nil
	}}
}
func (u *Updater) Fetch(ctx context.Context, address string) (SignedRelease, error) {
	var s SignedRelease
	if strings.TrimSpace(address) == "" {
		address = DefaultReleaseURL
	}
	v, e := url.Parse(address)
	if e != nil || v.Scheme != "https" || v.Host == "" || v.User != nil {
		return s, errors.New("обновления требуют HTTPS")
	}
	// Resolve latest once, then fetch the signed manifest from that exact tag.
	// Otherwise a release published between requests could change the candidate.
	var tag string
	parts := strings.Split(strings.Trim(v.Path, "/"), "/")
	if v.Host == "github.com" && len(parts) == 6 && parts[2] == "releases" && parts[3] == "latest" && parts[4] == "download" && parts[5] == "release.json" {
		tag, e = u.latestGitHubTag(ctx, parts[0], parts[1])
		if e != nil {
			return s, e
		}
		v.Path = "/" + parts[0] + "/" + parts[1] + "/releases/download/" + tag + "/release.json"
		v.RawPath, v.RawQuery, v.Fragment = "", "", ""
		address = v.String()
	}
	req, e := http.NewRequestWithContext(ctx, "GET", address, nil)
	if e != nil {
		return s, e
	}
	res, e := u.client().Do(req)
	if e != nil {
		return s, errors.New("источник обновлений недоступен")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return s, errors.New("источник обновлений вернул ошибку")
	}
	d := json.NewDecoder(io.LimitReader(res.Body, 65536))
	d.DisallowUnknownFields()
	if e = d.Decode(&s); e != nil {
		return s, e
	}
	if e = VerifyRelease(s, u.PublicKey); e != nil {
		return s, e
	}
	if tag != "" && s.Release.Version != tag {
		return s, errors.New("версия подписанного пакета не совпадает с тегом релиза")
	}
	return s, nil
}

func (u *Updater) latestGitHubTag(ctx context.Context, owner, repo string) (string, error) {
	address := "https://api.github.com/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/releases/latest"
	req, e := http.NewRequestWithContext(ctx, "GET", address, nil)
	if e != nil {
		return "", e
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2026-03-10")
	res, e := u.client().Do(req)
	if e != nil {
		return "", errors.New("источник обновлений недоступен")
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("источник обновлений вернул ошибку GitHub (%d)", res.StatusCode)
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if e = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(&release); e != nil {
		return "", errors.New("источник обновлений вернул некорректный релиз")
	}
	if release.Draft || release.Prerelease || !versionPattern.MatchString(release.Tag) {
		return "", errors.New("источник обновлений не содержит стабильного тега версии")
	}
	return release.Tag, nil
}

func (u *Updater) Stage(ctx context.Context, s SignedRelease) (string, error) {
	if e := VerifyRelease(s, u.PublicKey); e != nil {
		return "", e
	}
	r := s.Release
	releases := filepath.Join(u.Root, "releases")
	if e := os.MkdirAll(releases, 0755); e != nil {
		return "", e
	}
	if e := os.Chmod(releases, 0755); e != nil {
		return "", e
	}
	dest := filepath.Join(releases, r.Version)
	if b, e := os.ReadFile(filepath.Join(dest, ".bundle-sha256")); e == nil {
		if string(b) == r.SHA256 {
			return dest, nil
		}
		return "", errors.New("версия уже существует с другим хешем")
	}
	archive, e := os.CreateTemp(releases, ".download-*")
	if e != nil {
		return "", e
	}
	defer os.Remove(archive.Name())
	defer archive.Close()
	req, e := http.NewRequestWithContext(ctx, "GET", r.URL, nil)
	if e != nil {
		return "", e
	}
	res, e := u.client().Do(req)
	if e != nil {
		return "", errors.New("не удалось скачать пакет")
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		return "", errors.New("пакет недоступен")
	}
	h := sha256.New()
	n, e := io.Copy(io.MultiWriter(archive, h), io.LimitReader(res.Body, r.Size+1))
	if e != nil || n != r.Size || hex.EncodeToString(h.Sum(nil)) != r.SHA256 {
		return "", errors.New("пакет повреждён или загружен не полностью")
	}
	if _, e = archive.Seek(0, 0); e != nil {
		return "", e
	}
	stage, e := os.MkdirTemp(releases, ".stage-*")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(stage)
	gz, e := gzip.NewReader(archive)
	if e != nil {
		return "", e
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	var expanded int64
	for {
		hdr, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return "", e
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if !filepath.IsLocal(name) || strings.Contains(name, "\\") {
			return "", errors.New("unsafe archive path")
		}
		target := filepath.Join(stage, name)
		if hdr.Typeflag == tar.TypeDir {
			if e = os.MkdirAll(target, 0755); e != nil {
				return "", e
			}
			continue
		}
		if hdr.Typeflag != tar.TypeReg || seen[name] || hdr.Size < 0 || hdr.Size > 256<<20 {
			return "", errors.New("unsafe archive entry")
		}
		seen[name] = true
		expanded += hdr.Size
		if expanded > 1024<<20 {
			return "", errors.New("archive too large")
		}
		allowed := name == "VERSION" || strings.HasPrefix(name, "LICENSES/") || name == "bin/dozor" || name == "bin/mediamtx" || name == "bin/ffmpeg" || name == "bin/ffprobe"
		if !allowed {
			return "", errors.New("unexpected archive file")
		}
		if e = os.MkdirAll(filepath.Dir(target), 0755); e != nil {
			return "", e
		}
		mode := os.FileMode(0444)
		if strings.HasPrefix(name, "bin/") {
			mode = 0555
		}
		f, e := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if e != nil {
			return "", e
		}
		_, e = io.CopyN(f, tr, hdr.Size)
		if e == nil {
			// The root updater may run with umask 0077, but the service runs
			// as dozor and must be able to read and execute the release.
			e = f.Chmod(mode)
		}
		if e == nil {
			e = f.Sync()
		}
		ce := f.Close()
		if e == nil {
			e = ce
		}
		if e != nil {
			return "", e
		}
	}
	for _, file := range []string{"bin/dozor", "bin/mediamtx", "bin/ffmpeg", "bin/ffprobe", "VERSION"} {
		if !seen[file] {
			return "", errors.New("incomplete release")
		}
	}
	version, e := os.ReadFile(filepath.Join(stage, "VERSION"))
	if e != nil || strings.TrimSpace(string(version)) != r.Version {
		return "", errors.New("release version mismatch")
	}
	if e = AtomicWrite(filepath.Join(stage, ".bundle-sha256"), []byte(r.SHA256), 0444); e != nil {
		return "", e
	}
	var dirs []string
	if e = filepath.WalkDir(stage, func(path string, entry os.DirEntry, err error) error {
		if err == nil && entry.IsDir() {
			if err = os.Chmod(path, 0755); err != nil {
				return err
			}
			dirs = append(dirs, path)
		}
		return err
	}); e != nil {
		return "", e
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		if e = syncDirectory(dirs[i]); e != nil {
			return "", e
		}
	}
	if e = os.Rename(stage, dest); e != nil {
		return "", e
	}
	return dest, syncDirectory(releases)
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (u *Updater) clearJournal() error {
	if err := os.Remove(filepath.Join(u.Root, "pending-update.json")); err != nil {
		return err
	}
	return syncDirectory(u.Root)
}
func (u *Updater) switchTo(target string) error {
	base, e := filepath.EvalSymlinks(filepath.Join(u.Root, "releases"))
	if e != nil {
		return e
	}
	target, e = filepath.EvalSymlinks(target)
	if e != nil {
		return e
	}
	rel, e := filepath.Rel(base, target)
	if e != nil || !filepath.IsLocal(rel) || strings.Contains(rel, string(os.PathSeparator)) {
		return errors.New("invalid release target")
	}
	next := filepath.Join(u.Root, ".current-"+ID())
	if e = os.Symlink(filepath.Join("releases", rel), next); e != nil {
		return e
	}
	defer os.Remove(next)
	if e = os.Rename(next, filepath.Join(u.Root, "current")); e != nil {
		return e
	}
	d, e := os.Open(u.Root)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func (u *Updater) Recover() error {
	p := filepath.Join(u.Root, "pending-update.json")
	b, e := os.ReadFile(p)
	if errors.Is(e, os.ErrNotExist) {
		return nil
	}
	if e != nil {
		return e
	}
	var j UpdateJournal
	if e = json.Unmarshal(b, &j); e != nil {
		return e
	}
	if e = u.switchTo(j.Previous); e != nil {
		return e
	}
	if j.SystemBackup != "" {
		if u.System == nil {
			return errors.New("system recovery is not configured")
		}
		if e = u.System.restore(u.Root, j.SystemBackup); e != nil {
			return e
		}
	}
	return u.finishUpdate(j)
}
func (u *Updater) Apply(ctx context.Context, s SignedRelease) error {
	dest, e := u.Stage(ctx, s)
	if e != nil {
		return e
	}
	previous, e := filepath.EvalSymlinks(filepath.Join(u.Root, "current"))
	if e != nil {
		return e
	}
	j := UpdateJournal{Previous: previous, Candidate: dest}
	var integration systemManifest
	if u.System != nil {
		if integration, e = releaseSystemIntegration(ctx, dest, s.Release.Version); e != nil {
			return e
		}
		if e = u.System.ensureRecovery(); e != nil {
			return e
		}
		if j.SystemBackup, e = u.System.snapshot(u.Root); e != nil {
			return e
		}
	}
	// Persist both rollback snapshots before stopping or changing the installation.
	if e = WriteJSON(filepath.Join(u.Root, "pending-update.json"), j); e != nil {
		return e
	}
	rollback := func(cause error) error {
		if u.Stop != nil {
			_ = u.Stop()
		}
		if re := u.Recover(); re != nil {
			return fmt.Errorf("rollback failed: %w", re)
		}
		if u.Start != nil {
			_ = u.Start()
		}
		return cause
	}
	if u.Stop != nil {
		if e = u.Stop(); e != nil {
			return rollback(e)
		}
	}
	if e = u.switchTo(dest); e != nil {
		return rollback(e)
	}
	if u.System != nil {
		if e = u.System.install(dest, integration); e != nil {
			return rollback(e)
		}
	}
	if u.Start != nil {
		if e = u.Start(); e != nil {
			return rollback(e)
		}
	}
	health, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if u.Healthy != nil && !u.Healthy(health, s.Release.Version) {
		return rollback(errors.New("обновление не прошло проверку; предыдущая версия восстановлена"))
	}
	return u.finishUpdate(j)
}
