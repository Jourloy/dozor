package dozor

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
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
func SystemUpdate(ctx context.Context, configPath string, recoverOnly bool) error {
	if os.Geteuid() != 0 || runtime.GOOS != "linux" {
		return errors.New("updates require root on Linux")
	}
	u := &Updater{Root: "/opt/dozor"}
	if recoverOnly {
		return u.Recover()
	}
	if e := u.Recover(); e != nil {
		return e
	}
	c, e := LoadConfig(configPath)
	if e != nil {
		return e
	}
	cfg := c.Get()
	status := func(msg string) { _ = AtomicWrite("/var/lib/dozor/update-status.json", []byte(msg), 0644) }
	if !cfg.AutoUpdate {
		status("Автообновление выключено")
		return nil
	}
	b, e := os.ReadFile("/etc/dozor/update.pub")
	if e != nil {
		status("Не установлен ключ проверки обновлений")
		return e
	}
	key, e := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if e != nil || len(key) != ed25519.PublicKeySize {
		status("Не установлен ключ проверки обновлений")
		return errors.New("invalid update public key")
	}
	u.PublicKey = key
	client := UnixClient("/run/dozor/control.sock")
	u.Stop = func() error { return exec.Command("systemctl", "stop", "dozor.service").Run() }
	u.Start = func() error { return exec.Command("systemctl", "start", "dozor.service").Run() }
	u.Healthy = func(ctx context.Context, version string) bool {
		for ctx.Err() == nil {
			health, e := readProcessHealth(ctx, client)
			if e == nil && health.Ready && health.Version == version {
				return true
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
	Version string `json:"version"`
	Ready   bool   `json:"ready"`
}

func readProcessHealth(ctx context.Context, client *http.Client) (processHealth, error) {
	var health processHealth
	req, e := http.NewRequestWithContext(ctx, "GET", "http://unix/health", nil)
	if e != nil {
		return health, e
	}
	res, e := client.Do(req)
	if e != nil {
		return health, errors.New("Dozor недоступна; обновление отложено")
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
	for _, check := range []struct{ name, arg string }{{"dozor", "version"}, {"mediamtx", "--version"}, {"ffmpeg", "-version"}, {"ffprobe", "-version"}} {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		cmd := exec.CommandContext(probeCtx, filepath.Join(staged, "bin", check.name), check.arg)
		out, err := cmd.Output()
		cancel()
		if err != nil {
			return errors.New("Новый пакет несовместим с устройством")
		}
		if check.name == "dozor" && strings.TrimSpace(string(out)) != version {
			return errors.New("версия бинарника не совпадает с версией подписанного пакета")
		}
	}
	return nil
}

func (u *Updater) updateRunning(ctx context.Context, address string, client *http.Client, status func(string)) (err error) {
	defer func() {
		if err != nil {
			status(err.Error())
		}
	}()
	current, e := readProcessHealth(ctx, client)
	if e != nil {
		return e
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
	staged, e := u.Stage(ctx, s)
	if e != nil {
		return e
	}
	if e = preflightRelease(ctx, staged, s.Release.Version); e != nil {
		return e
	}
	// Recheck after the download in case the running application changed meanwhile.
	current, e = readProcessHealth(ctx, client)
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
	if e = WriteJSON(filepath.Join(u.Root, "pending-update.json"), UpdateJournal{previous, staged}); e != nil {
		return e
	}
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://unix/prepare-update", nil)
	res, e := client.Do(req)
	if e != nil {
		return errors.New("Dozor недоступна; обновление отложено")
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != 204 {
		status("Обновление загружено; ожидаем завершения событий")
		return u.clearJournal()
	}
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
