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
		return errors.New("диск не выбран: требуется Linux и UUID ext4")
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
	b, e := exec.Command("findmnt", "--json", "--mountpoint", g.Root, "--output", "TARGET,UUID,FSTYPE,OPTIONS").Output()
	if e != nil {
		return errors.New("диск отключён")
	}
	var v struct {
		Filesystems []struct{ Target, UUID, FSType, Options string }
	}
	if e = json.Unmarshal(b, &v); e != nil {
		return e
	}
	if len(v.Filesystems) != 1 {
		return errors.New("диск не смонтирован")
	}
	f := v.Filesystems[0]
	if f.UUID != g.UUID || f.FSType != "ext4" || filepath.Clean(f.Target) != filepath.Clean(g.Root) || !strings.Contains(","+f.Options+",", ",rw,") {
		return errors.New("UUID, файловая система или режим диска не соответствует настройкам")
	}
	return nil
}
