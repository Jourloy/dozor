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

func MountDisk(uuid string) error {
	if os.Geteuid() != 0 || runtime.GOOS != "linux" || !diskUUIDPattern.MatchString(uuid) {
		return errors.New("requires root, Linux and ext4 UUID")
	}
	disks, e := Disks()
	if e != nil {
		return e
	}
	var found *Disk
	var walk func([]Disk)
	walk = func(v []Disk) {
		for _, d := range v {
			if d.UUID == uuid {
				x := d
				found = &x
			}
			walk(d.Children)
		}
	}
	walk(disks)
	if found == nil || found.FSType != "ext4" {
		return errors.New("ext4 partition not found")
	}
	for _, m := range found.Mountpoints {
		if m != nil && *m != "" && *m != "/srv/dozor" {
			return errors.New("partition already mounted elsewhere")
		}
	}
	// Never replace an already mounted archive with another device while recording.
	if out, e := exec.Command("findmnt", "-n", "-M", "/srv/dozor", "-o", "UUID").Output(); e == nil && strings.TrimSpace(string(out)) != uuid {
		return errors.New("unmount current archive before replacing it")
	}
	if e = os.MkdirAll("/srv/dozor", 0555); e != nil {
		return e
	}
	unit := "[Unit]\nDescription=Dozor archive disk\n\n[Mount]\nWhat=/dev/disk/by-uuid/" + uuid + "\nWhere=/srv/dozor\nType=ext4\nOptions=rw,nosuid,nodev,noexec\nTimeoutSec=20\n\n[Install]\nWantedBy=multi-user.target\n"
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
	account, e := user.Lookup("dozor")
	if e != nil {
		return e
	}
	uid, _ := strconv.Atoi(account.Uid)
	gid, _ := strconv.Atoi(account.Gid)
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
	if !cfg.AutoUpdate || cfg.ReleaseURL == "" {
		status("Автообновление выключено или не указан адрес release.json")
		return nil
	}
	b, e := os.ReadFile("/etc/dozor/update.pub")
	if e != nil {
		status("Не установлен ключ проверки обновлений")
		return e
	}
	key, e := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if e != nil || len(key) != ed25519.PublicKeySize {
		return errors.New("invalid update public key")
	}
	u.PublicKey = key
	s, e := u.Fetch(ctx, cfg.ReleaseURL)
	if e != nil {
		status(e.Error())
		return e
	}
	current, _ := os.ReadFile("/opt/dozor/current/VERSION")
	if !Newer(s.Release.Version, strings.TrimSpace(string(current))) {
		status("Установлена актуальная версия")
		return nil
	}
	// Download and verify before reserving an idle maintenance window.
	staged, e := u.Stage(ctx, s)
	if e != nil {
		status(e.Error())
		return e
	}
	for _, check := range []struct{ name, arg string }{{"dozor", "version"}, {"mediamtx", "--version"}, {"ffmpeg", "-version"}, {"ffprobe", "-version"}} {
		probeCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		cmd := exec.CommandContext(probeCtx, filepath.Join(staged, "bin", check.name), check.arg)
		err := cmd.Run()
		cancel()
		if err != nil {
			status("Новый пакет несовместим с устройством")
			return errors.New("release binary preflight failed")
		}
	}
	previous, e := filepath.EvalSymlinks("/opt/dozor/current")
	if e != nil {
		return e
	}
	// Persist recovery before reserving the paused window. The failure unit
	// restarts the recorder only when this journal exists.
	if e = WriteJSON("/opt/dozor/pending-update.json", UpdateJournal{previous, staged}); e != nil {
		return e
	}
	client := UnixClient("/run/dozor/control.sock")
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
	u.Stop = func() error { return exec.Command("systemctl", "stop", "dozor.service").Run() }
	u.Start = func() error { return exec.Command("systemctl", "start", "dozor.service").Run() }
	u.Healthy = func(ctx context.Context, version string) bool {
		for ctx.Err() == nil {
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://unix/health", nil)
			res, e := client.Do(req)
			if e == nil {
				var v struct {
					Version string
					Ready   bool
				}
				e = json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&v)
				res.Body.Close()
				if e == nil && v.Ready && v.Version == version {
					return true
				}
			}
			if !pause(ctx, time.Second) {
				break
			}
		}
		return false
	}
	if e = u.Apply(ctx, s); e != nil {
		status(e.Error())
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
