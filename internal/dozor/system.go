package dozor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

func archiveDisk(disks []Disk, uuid string) (*Disk, error) {
	if !diskUUIDPattern.MatchString(uuid) {
		return nil, errors.New("invalid archive UUID")
	}
	var found *Disk
	matches := 0
	var walk func([]Disk)
	walk = func(v []Disk) {
		for _, d := range v {
			if d.UUID == uuid {
				matches++
				x := d
				found = &x
			}
			walk(d.Children)
		}
	}
	walk(disks)
	if matches > 1 {
		return nil, errors.New("multiple devices have the selected UUID")
	}
	if found == nil || !supportedArchiveFS(found.FSType) {
		return nil, errors.New("ext4 or exFAT partition not found")
	}
	for _, m := range found.Mountpoints {
		if m != nil && *m != "" && *m != "/srv/dozor" {
			return nil, errors.New("partition already mounted elsewhere")
		}
	}
	return found, nil
}

func archiveMountOptions(fsType string, uid, gid int) string {
	options := "rw,nosuid,nodev,noexec"
	if fsType == "exfat" {
		// exFAT stores no Unix ownership or permissions; apply them to the whole volume.
		options += fmt.Sprintf(",uid=%d,gid=%d,fmask=0177,dmask=0077", uid, gid)
	}
	return options
}

func archiveMountUnit(d Disk, uid, gid int) (string, error) {
	if !diskUUIDPattern.MatchString(d.UUID) || !supportedArchiveFS(d.FSType) || uid < 0 || gid < 0 {
		return "", errors.New("invalid archive mount parameters")
	}
	return "[Unit]\nDescription=Dozor archive disk\n\n[Mount]\nWhat=/dev/disk/by-uuid/" + d.UUID + "\nWhere=/srv/dozor\nType=" + d.FSType + "\nOptions=" + archiveMountOptions(d.FSType, uid, gid) + "\nTimeoutSec=20\n\n[Install]\nWantedBy=multi-user.target\n", nil
}

func MountDisk(uuid string) error {
	if os.Geteuid() != 0 || runtime.GOOS != "linux" || !diskUUIDPattern.MatchString(uuid) {
		return errors.New("requires root, Linux and an ext4 or exFAT UUID")
	}
	disks, e := Disks()
	if e != nil {
		return e
	}
	found, e := archiveDisk(disks, uuid)
	if e != nil {
		return e
	}
	account, e := user.Lookup("dozor")
	if e != nil {
		return e
	}
	uid, e := strconv.Atoi(account.Uid)
	if e != nil {
		return e
	}
	gid, e := strconv.Atoi(account.Gid)
	if e != nil {
		return e
	}
	unit, e := archiveMountUnit(*found, uid, gid)
	if e != nil {
		return e
	}
	// Never replace an already mounted archive with another device while recording.
	if current, e := archiveMount("/srv/dozor"); e == nil {
		if current.UUID != uuid {
			return errors.New("unmount current archive before replacing it")
		}
		if found.FSType == "exfat" && !mountOptionsInclude(current.Options, archiveMountOptions(found.FSType, uid, gid)) {
			return errors.New("unmount current exFAT archive before changing its mount permissions")
		}
	}
	if e = os.MkdirAll("/srv/dozor", 0555); e != nil {
		return e
	}
	if e = AtomicWrite("/etc/systemd/system/srv-dozor.mount", []byte(unit), 0644); e != nil {
		return e
	}
	// Start the same UUID-bound mount when the USB device returns after removal.
	rule := `ACTION=="add", SUBSYSTEM=="block", ENV{ID_FS_UUID}=="` + uuid + `", TAG+="systemd", ENV{SYSTEMD_WANTS}+="srv-dozor.mount"` + "\n"
	if e = AtomicWrite("/etc/udev/rules.d/90-dozor-disk.rules", []byte(rule), 0644); e != nil {
		return e
	}
	if e = exec.Command("udevadm", "control", "--reload-rules").Run(); e != nil {
		return e
	}
	for _, args := range [][]string{{"daemon-reload"}, {"enable", "srv-dozor.mount"}, {"start", "srv-dozor.mount"}} {
		if e = exec.Command("systemctl", args...).Run(); e != nil {
			return e
		}
	}
	if e = (Guard{Root: "/srv/dozor", UUID: uuid}).Check(); e != nil {
		return e
	}
	if found.FSType == "exfat" {
		current, e := archiveMount("/srv/dozor")
		if e != nil {
			return e
		}
		if !mountOptionsInclude(current.Options, archiveMountOptions(found.FSType, uid, gid)) {
			return errors.New("exFAT archive mount permissions do not match the dozor account")
		}
		return nil
	}
	if e = os.Chown("/srv/dozor", uid, gid); e != nil {
		return e
	}
	return os.Chmod("/srv/dozor", 0700)
}
func SystemUpdate(ctx context.Context, configPath string, recoverOnly bool) (err error) {
	if os.Geteuid() != 0 || runtime.GOOS != "linux" {
		return errors.New("updates require root on Linux")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	u := &Updater{Root: "/opt/dozor", System: &SystemIntegration{
		Root: "/", RecoveryBinary: self,
		Reload: func() error {
			child, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			return exec.CommandContext(child, "systemctl", "daemon-reload").Run()
		},
	}}
	if recoverOnly {
		return u.Recover()
	}
	unlock, err := lockUpdater(u.Root)
	if err != nil {
		return err
	}
	defer unlock()
	message := "Проверяем наличие обновлений"
	status := func(msg string) {
		message = msg
		_ = writeUpdateStatus("/var/lib/dozor", message, true)
	}
	defer func() {
		if err != nil {
			message = err.Error()
		}
		_ = writeUpdateStatus("/var/lib/dozor", message, false)
	}()
	if e := u.Recover(); e != nil {
		return e
	}
	client := UnixClient("/run/dozor/control.sock")
	u.Immediate, err = consumeUpdateMarker("/var/lib/dozor", "update-request")
	if err != nil {
		return err
	}
	reconcile, err := consumeUpdateMarker("/var/lib/dozor", "update-reconcile-request")
	if err != nil {
		return err
	}
	cfg, err := loadUpdaterSettings(configPath, filepath.Join(u.Root, "updater-settings.json"))
	if err != nil {
		return err
	}
	u.PublicKey, err = readUpdateKey("/etc/dozor/update.pub")
	if err != nil {
		return err
	}
	if reconcile {
		current, err := u.installedHealth(ctx, client)
		if err != nil {
			return err
		}
		if err = u.reconcileSystem(ctx, current.Version); err != nil {
			return err
		}
		status("Служба обновления готова")
		return nil
	}
	if !cfg.AutoUpdate && !u.Immediate {
		status("Автообновление выключено")
		return nil
	}
	u.Stop = func() error {
		child, cancel := context.WithTimeout(ctx, 45*time.Second)
		defer cancel()
		return exec.CommandContext(child, "systemctl", "stop", "dozor.service").Run()
	}
	u.Start = func() error {
		child, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		return exec.CommandContext(child, "systemctl", "start", "dozor.service").Run()
	}
	u.Healthy = func(ctx context.Context, version string) bool {
		for ctx.Err() == nil {
			health, e := readProcessHealth(ctx, client)
			if e == nil && health.Ready && health.Version == version {
				return true
			}
			if e != nil {
				u.HealthError = e.Error()
			} else if health.Version != version {
				u.HealthError = fmt.Sprintf("ожидалась версия %s, API сообщает %s", version, health.Version)
			} else {
				u.HealthError = health.failure()
			}
			if !pause(ctx, time.Second) {
				break
			}
		}
		return false
	}
	return u.updateRunning(ctx, cfg.ReleaseURL, client, status)
}

type processHealth struct {
	Version  string      `json:"version"`
	Ready    bool        `json:"ready"`
	Core     HealthCheck `json:"core"`
	Database HealthCheck `json:"database"`
	Updater  HealthCheck `json:"updater"`
}

func readProcessHealth(ctx context.Context, client *http.Client) (processHealth, error) {
	var health processHealth
	req, e := http.NewRequestWithContext(ctx, "GET", "http://unix/health", nil)
	if e != nil {
		return health, e
	}
	res, e := client.Do(req)
	if e != nil {
		return health, fmt.Errorf("API приложения недоступен: %w", e)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return health, errors.New("не удалось определить версию работающей Dozor")
	}
	if e = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&health); e != nil || (!versionPattern.MatchString(health.Version) && health.Version != "dev") {
		return health, errors.New("не удалось определить версию работающей Dozor")
	}
	return health, nil
}

func preflightRelease(ctx context.Context, staged, version string) error {
	probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(probeCtx, filepath.Join(staged, "bin", "dozor"), "version").Output()
	if err != nil {
		return errors.New("Новый пакет несовместим с устройством")
	}
	if strings.TrimSpace(string(out)) != version {
		return errors.New("версия бинарника не совпадает с версией подписанного пакета")
	}
	// Recorder failures are reported by runtime diagnostics. They must not veto
	// a working control plane that can install the next corrective release.
	return nil
}

func (u *Updater) updateRunning(ctx context.Context, address string, client *http.Client, status func(string)) (err error) {
	defer func() {
		if err != nil {
			status(err.Error())
		}
	}()
	current, e := u.installedHealth(ctx, client)
	if e != nil {
		return e
	}
	// Repair system integration when possible, but a damaged application binary
	// must not prevent the independent helper from installing its replacement.
	if e = u.reconcileSystem(ctx, current.Version); e != nil {
		status("Не удалось согласовать текущую интеграцию; проверяем исправление: " + e.Error())
	}
	s, e := u.Fetch(ctx, address)
	if e != nil {
		return e
	}
	if !Newer(s.Release.Version, current.Version) {
		status("Установлена актуальная версия")
		return nil
	}
	// Download and verify before reserving an idle maintenance window.
	status("Скачиваем и проверяем обновление")
	staged, e := u.Stage(ctx, s)
	if e != nil {
		return e
	}
	if e = preflightRelease(ctx, staged, s.Release.Version); e != nil {
		return e
	}
	if e = u.checkCandidateUpdater(ctx, staged, s); e != nil {
		return e
	}
	// Recheck after the download in case the running application changed meanwhile.
	current, e = u.installedHealth(ctx, client)
	if e != nil {
		return e
	}
	if !Newer(s.Release.Version, current.Version) {
		status("Установлена актуальная версия")
		return nil
	}
	previous, e := filepath.EvalSymlinks(filepath.Join(u.Root, "current"))
	if e != nil {
		return e
	}
	// Persist recovery before reserving the paused window. The failure unit
	// restarts the recorder only when this journal exists.
	if e = WriteJSON(filepath.Join(u.Root, "pending-update.json"), UpdateJournal{Previous: previous, Candidate: staged}); e != nil {
		return e
	}
	prepareURL := "http://unix/prepare-update"
	if u.Immediate {
		prepareURL += "?immediate=true"
	}
	prepareCtx, cancelPrepare := context.WithTimeout(ctx, 3*time.Second)
	defer cancelPrepare()
	req, _ := http.NewRequestWithContext(prepareCtx, "POST", prepareURL, nil)
	res, e := client.Do(req)
	if e == nil {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if res.StatusCode == http.StatusConflict {
			status("Обновление загружено; ожидаем завершения событий")
			return u.clearJournal()
		}
		if res.StatusCode != 204 && res.StatusCode < 500 {
			return fmt.Errorf("подготовка обновления: HTTP %d", res.StatusCode)
		}
	}
	// A dead API cannot veto a fully verified corrective update. systemd stops
	// its entire cgroup before the candidate is switched into place.
	status("Устанавливаем обновление; Dozor перезапускается")
	if e = u.Apply(ctx, s); e != nil {
		return e
	}
	status(fmt.Sprintf("Установлена %s · %s", s.Release.Version, time.Now().UTC().Format(time.RFC3339)))
	return nil
}
func BundleBinaries() Binaries {
	self, _ := os.Executable()
	dir := filepath.Dir(self)
	resolve := func(name string) string {
		p := filepath.Join(dir, name)
		if _, e := os.Stat(p); e == nil {
			return p
		}
		return name
	}
	return Binaries{resolve("mediamtx"), resolve("ffmpeg"), resolve("ffprobe"), self}
}

func (h processHealth) failure() string {
	for _, c := range []struct {
		name  string
		check HealthCheck
	}{{"приложение", h.Core}, {"база", h.Database}, {"обновлятор", h.Updater}} {
		if !c.check.Ready && c.check.State != "" {
			return c.name + ": " + c.check.State + " " + c.check.Error
		}
	}
	return "приложение не подтвердило готовность"
}
