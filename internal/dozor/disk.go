package dozor

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type Disk struct {
	Name        string       `json:"name"`
	Model       string       `json:"model"`
	Size        json.Number  `json:"size"`
	UUID        string       `json:"uuid"`
	FSType      string       `json:"fstype"`
	Free        *json.Number `json:"fsavail"`
	Mountpoints []*string    `json:"mountpoints"`
	Children    []Disk       `json:"children,omitempty"`
}

func Disks() ([]Disk, error) {
	if runtime.GOOS != "linux" {
		return []Disk{}, nil
	}
	b, e := exec.Command("lsblk", "--json", "--bytes", "--output", "NAME,MODEL,SIZE,UUID,FSTYPE,FSAVAIL,MOUNTPOINTS").Output()
	if e != nil {
		return nil, errors.New("lsblk недоступен")
	}
	var v struct {
		Blockdevices []Disk `json:"blockdevices"`
	}
	d := json.NewDecoder(strings.NewReader(string(b)))
	d.UseNumber()
	e = d.Decode(&v)
	return v.Blockdevices, e
}

type Guard struct {
	Root, UUID  string
	Development bool
}

func supportedArchiveFS(fsType string) bool {
	return fsType == "ext4" || fsType == "exfat"
}

type archiveMountInfo struct {
	Target, UUID, FSType, Options string
}

func archiveMount(root string) (archiveMountInfo, error) {
	// systemd's mount namespace can retain covered bind mounts after hotplug.
	// Inspect only the visible filesystem, not the layers underneath it.
	b, e := exec.Command("findmnt", "--json", "--uniq", "--mountpoint", root, "--output", "TARGET,UUID,FSTYPE,OPTIONS").Output()
	if e != nil {
		return archiveMountInfo{}, errors.New("диск отключён")
	}
	var v struct {
		Filesystems []archiveMountInfo
	}
	if e = json.Unmarshal(b, &v); e != nil {
		return archiveMountInfo{}, e
	}
	if len(v.Filesystems) != 1 {
		return archiveMountInfo{}, errors.New("диск не смонтирован")
	}
	return v.Filesystems[0], nil
}

func mountOptionsInclude(actual, required string) bool {
	for _, option := range strings.Split(required, ",") {
		if !strings.Contains(","+actual+",", ","+option+",") {
			return false
		}
	}
	return true
}

func (g Guard) checkMount(f archiveMountInfo) error {
	if f.UUID != g.UUID || !supportedArchiveFS(f.FSType) || filepath.Clean(f.Target) != filepath.Clean(g.Root) || !mountOptionsInclude(f.Options, "rw") || mountOptionsInclude(f.Options, "ro") {
		return errors.New("UUID, файловая система или режим диска не соответствует настройкам")
	}
	return nil
}

func (g Guard) Check() error {
	if g.Development {
		fi, e := os.Stat(g.Root)
		if e != nil {
			return e
		}
		if !fi.IsDir() {
			return errors.New("archive is not directory")
		}
		return nil
	}
	if runtime.GOOS != "linux" || g.UUID == "" {
		return errors.New("диск не выбран: требуется Linux и UUID раздела ext4 или exFAT")
	}
	if !diskUUIDPattern.MatchString(g.UUID) {
		return errors.New("неверный UUID диска")
	}
	if _, e := os.Stat(filepath.Join("/dev/disk/by-uuid", g.UUID)); e != nil {
		return errors.New("устройство выбранного диска отсутствует")
	}
	resolved, e := filepath.EvalSymlinks(g.Root)
	if e != nil || resolved != filepath.Clean(g.Root) {
		return errors.New("точка монтирования недоступна или является ссылкой")
	}
	f, e := archiveMount(g.Root)
	if e != nil {
		return e
	}
	return g.checkMount(f)
}
