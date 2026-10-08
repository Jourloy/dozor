package dozor

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type updaterSettings struct {
	AutoUpdate bool   `json:"auto_update"`
	ReleaseURL string `json:"release_url"`
}

func loadUpdaterSettings(config, backup string) (updaterSettings, error) {
	parse := func(path string) (updaterSettings, error) {
		cfg := updaterSettings{AutoUpdate: true, ReleaseURL: DefaultReleaseURL}
		data, err := os.ReadFile(path)
		if err != nil {
			return cfg, err
		}
		if err = json.Unmarshal(data, &cfg); err != nil {
			return cfg, err
		}
		if strings.TrimSpace(cfg.ReleaseURL) == "" {
			cfg.ReleaseURL = DefaultReleaseURL
		}
		u, err := url.Parse(cfg.ReleaseURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
			return cfg, errors.New("invalid updater source")
		}
		return cfg, nil
	}
	cfg, err := parse(config)
	if err != nil {
		return parse(backup)
	}
	if err = WriteJSON(backup, cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}
func consumeUpdateMarker(state, name string) (bool, error) {
	path := filepath.Join(state, name)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, errors.New("invalid update request marker")
	}
	if err = os.Remove(path); err != nil {
		return false, err
	}
	return true, syncDirectory(state)
}
func lockUpdater(root string) (func(), error) {
	f, err := os.OpenFile(filepath.Join(root, ".update-lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, errors.New("обновление уже выполняется")
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
func readUpdateKey(path string) (ed25519.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("Не установлен ключ проверки обновлений")
	}
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(data)))
	if err != nil || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("Неверный ключ проверки обновлений")
	}
	return ed25519.PublicKey(key), nil
}

// This is deliberately usable without loading cameras or opening the archive.
func CheckUpdater(ctx context.Context, root, keyPath string) error {
	key, err := readUpdateKey(keyPath)
	if err != nil {
		return err
	}
	// With a staged package the root preflight supplies a signed manifest. Normal
	// service readiness only checks the installed command and integration.
	if data, err := os.ReadFile(filepath.Join(root, "current", ".release.json")); err == nil {
		var release SignedRelease
		if err = json.Unmarshal(data, &release); err != nil {
			return err
		}
		if err = VerifyRelease(release, key); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	unit, err := os.ReadFile("/etc/systemd/system/dozor-update.service")
	if err != nil {
		return err
	}
	text := string(unit)
	if !strings.Contains(text, "ExecStart=/usr/local/libexec/dozor-system system update") && !strings.Contains(text, "ExecStart=/opt/dozor/current/bin/dozor system update") {
		return errors.New("неверная команда службы обновления")
	}
	child, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	out, err := exec.CommandContext(child, "systemctl", "show", "--property=LoadState", "--value", "dozor-update.service").Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "loaded" {
		return errors.New("служба обновления не загружена")
	}
	out, err = exec.CommandContext(child, "systemctl", "show", "--property=ActiveState", "--value", "dozor-update.timer").Output()
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(out)) != "active" {
		return errors.New("таймер обновления не работает")
	}
	return nil
}
func (u *Updater) installedHealth(ctx context.Context, client *http.Client) (processHealth, error) {
	// Root-owned metadata is installed before confirming the release. The current
	// symlink also selects the correct metadata after rollback.
	data, err := os.ReadFile(filepath.Join(u.Root, "current", ".release.json"))
	if err == nil {
		var s SignedRelease
		if err = json.Unmarshal(data, &s); err != nil {
			return processHealth{}, err
		}
		if err = VerifyRelease(s, u.PublicKey); err != nil {
			return processHealth{}, err
		}
		return processHealth{Version: s.Release.Version}, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return processHealth{}, err
	}
	confirmed, readErr := os.ReadFile(filepath.Join(u.Root, "current", ".confirmed-version"))
	if readErr == nil {
		version := strings.TrimSpace(string(confirmed))
		if !versionPattern.MatchString(version) {
			return processHealth{}, errors.New("invalid confirmed release version")
		}
		return processHealth{Version: version}, nil
	}
	if !errors.Is(readErr, os.ErrNotExist) {
		return processHealth{}, readErr
	}
	// Bootstrap from older signed packages, which did not preserve the manifest.
	probe, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	health, healthErr := readProcessHealth(probe, client)
	if healthErr == nil {
		return health, nil
	}
	versionCtx, stop := context.WithTimeout(ctx, 3*time.Second)
	defer stop()
	out, e := exec.CommandContext(versionCtx, filepath.Join(u.Root, "current", "bin", "dozor"), "version").Output()
	version := strings.TrimSpace(string(out))
	if e == nil && versionPattern.MatchString(version) {
		return processHealth{Version: version}, nil
	}
	return health, healthErr
}
func (u *Updater) checkCandidateUpdater(ctx context.Context, staged string, s SignedRelease) error {
	if u.System == nil {
		return nil
	}
	// Old candidates do not advertise the self-check. Keep downgrade/recovery
	// compatibility; all new releases advertise and must pass it.
	manifest, err := releaseSystemIntegration(ctx, staged, s.Release.Version)
	if err != nil {
		return err
	}
	if manifest.UpdaterProtocol == 0 {
		return nil
	}
	temp, err := os.MkdirTemp(u.Root, ".update-probe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	if err = WriteJSON(filepath.Join(temp, "release.json"), s); err != nil {
		return err
	}
	if err = AtomicWrite(filepath.Join(temp, "update.pub"), []byte(base64.StdEncoding.EncodeToString(u.PublicKey)), 0600); err != nil {
		return err
	}
	if err = os.Link(filepath.Join(staged, ".bundle.tar.gz"), filepath.Join(temp, "bundle.tar.gz")); err != nil {
		return err
	}
	child, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	out, err := exec.CommandContext(child, filepath.Join(staged, "bin", "dozor"), "system", "verify-update", temp).CombinedOutput()
	if err != nil {
		return fmt.Errorf("самопроверка обновлятора: %s: %w", strings.TrimSpace(string(out)), err)
	}
	return nil
}
func VerifyUpdaterFixture(dir string) error {
	key, err := readUpdateKey(filepath.Join(dir, "update.pub"))
	if err != nil {
		return err
	}
	data, err := os.ReadFile(filepath.Join(dir, "release.json"))
	if err != nil {
		return err
	}
	var release SignedRelease
	if err = json.Unmarshal(data, &release); err != nil {
		return err
	}
	if err = VerifyRelease(release, key); err != nil {
		return err
	}
	sha, _, size, err := HashFile(filepath.Join(dir, "bundle.tar.gz"))
	if err != nil {
		return err
	}
	if sha != release.Release.SHA256 || size != release.Release.Size {
		return errors.New("пакет повреждён или загружен не полностью")
	}
	probe := filepath.Join(dir, "write-probe")
	if err = AtomicWrite(probe, []byte("verified"), 0600); err != nil {
		return err
	}
	if err = removeDurable(probe); err != nil {
		return err
	}
	workspace, err := os.MkdirTemp(dir, "prepare-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(workspace)
	// Exercise this candidate's real download/verification/extraction code with
	// the saved package. The transport cannot contact the network.
	u := Updater{Root: workspace, PublicKey: key, Client: &http.Client{Transport: localUpdatePackage{file: filepath.Join(dir, "bundle.tar.gz")}}}
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Second)
	defer cancel()
	_, err = u.Stage(ctx, release)
	return err
}

type localUpdatePackage struct{ file string }

func (p localUpdatePackage) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(p.file)
	if err != nil {
		return nil, err
	}
	return &http.Response{StatusCode: 200, Body: f, Header: make(http.Header), Request: req}, nil
}
