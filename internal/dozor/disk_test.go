package dozor

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testExt4UUID = "12345678-1234-1234-1234-123456789abc"

func TestDiskUUIDs(t *testing.T) {
	for _, uuid := range []string{testExt4UUID, strings.ToUpper(testExt4UUID), "12AB-34CD", "12ab-34cd"} {
		if !diskUUIDPattern.MatchString(uuid) {
			t.Errorf("valid UUID rejected: %q", uuid)
		}
	}
	for _, uuid := range []string{"", "12345678", "123-4567", "1234-56789", "GGGG-1234", "../12AB-34CD", "12AB-34CD\n", "12AB-34CD.service", "12AB-34CD;id", " 12AB-34CD"} {
		if diskUUIDPattern.MatchString(uuid) {
			t.Errorf("unsafe or invalid UUID accepted: %q", uuid)
		}
	}
}

func TestArchiveDiskSelection(t *testing.T) {
	otherMount, root, archive, empty := "/media/archive", "/", "/srv/dozor", ""
	for _, tc := range []struct {
		name   string
		disks  []Disk
		uuid   string
		wantFS string
	}{
		{"ext4", []Disk{{Children: []Disk{{UUID: testExt4UUID, FSType: "ext4"}}}}, testExt4UUID, "ext4"},
		{"exfat", []Disk{{Children: []Disk{{UUID: "12AB-34CD", FSType: "exfat", Mountpoints: []*string{nil, &empty}}}}}, "12AB-34CD", "exfat"},
		{"existing archive", []Disk{{UUID: "12AB-34CD", FSType: "exfat", Mountpoints: []*string{&archive}}}, "12AB-34CD", "exfat"},
		{"mounted elsewhere", []Disk{{UUID: "12AB-34CD", FSType: "exfat", Mountpoints: []*string{&otherMount}}}, "12AB-34CD", ""},
		{"system partition", []Disk{{UUID: testExt4UUID, FSType: "ext4", Mountpoints: []*string{&root}}}, testExt4UUID, ""},
		{"fat32 with short UUID", []Disk{{UUID: "12AB-34CD", FSType: "vfat"}}, "12AB-34CD", ""},
		{"unsupported fs", []Disk{{UUID: testExt4UUID, FSType: "btrfs"}}, testExt4UUID, ""},
		{"missing", []Disk{{UUID: "12AB-34CD", FSType: "exfat"}}, "5678-90AB", ""},
		{"duplicate UUID", []Disk{{UUID: "12AB-34CD", FSType: "exfat"}, {UUID: "12AB-34CD", FSType: "exfat"}}, "12AB-34CD", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, err := archiveDisk(tc.disks, tc.uuid)
			if tc.wantFS == "" {
				if err == nil {
					t.Fatal("unsafe disk selection accepted")
				}
				return
			}
			must(t, err)
			if d.UUID != tc.uuid || d.FSType != tc.wantFS {
				t.Fatalf("unexpected disk: %+v", d)
			}
		})
	}
}

func TestArchiveGuardMount(t *testing.T) {
	for _, tc := range []struct {
		name, uuid, fsType, target, options string
		ok                                  bool
	}{
		{"ext4", testExt4UUID, "ext4", "/srv/dozor", "rw,nosuid,nodev,noexec", true},
		{"exfat", "12AB-34CD", "exfat", "/srv/dozor", "rw,nosuid,nodev,noexec,uid=997,gid=996,fmask=0177,dmask=0077", true},
		{"read only", "12AB-34CD", "exfat", "/srv/dozor", "ro", false},
		{"conflicting mode", "12AB-34CD", "exfat", "/srv/dozor", "rw,ro", false},
		{"no rw option", "12AB-34CD", "exfat", "/srv/dozor", "user.rw", false},
		{"wrong UUID", "5678-90AB", "exfat", "/srv/dozor", "rw", false},
		{"wrong target", "12AB-34CD", "exfat", "/srv", "rw", false},
		{"fat32", "12AB-34CD", "vfat", "/srv/dozor", "rw", false},
		{"unsupported", "12AB-34CD", "ntfs", "/srv/dozor", "rw", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uuid := "12AB-34CD"
			if tc.fsType == "ext4" {
				uuid = testExt4UUID
			}
			g := Guard{Root: "/srv/dozor", UUID: uuid}
			err := g.checkMount(archiveMountInfo{UUID: tc.uuid, FSType: tc.fsType, Target: tc.target, Options: tc.options})
			if (err == nil) != tc.ok {
				t.Fatalf("guard result: %v, want accepted=%v", err, tc.ok)
			}
		})
	}
}

func TestArchiveMountUnit(t *testing.T) {
	for _, tc := range []struct {
		fsType, uuid, options string
	}{
		{"ext4", testExt4UUID, "rw,nosuid,nodev,noexec"},
		{"exfat", "12AB-34CD", "rw,nosuid,nodev,noexec,uid=997,gid=996,fmask=0177,dmask=0077"},
	} {
		t.Run(tc.fsType, func(t *testing.T) {
			unit, err := archiveMountUnit(Disk{UUID: tc.uuid, FSType: tc.fsType}, 997, 996)
			must(t, err)
			for _, line := range []string{"What=/dev/disk/by-uuid/" + tc.uuid, "Where=/srv/dozor", "Type=" + tc.fsType, "Options=" + tc.options} {
				if !strings.Contains(unit, "\n"+line+"\n") {
					t.Errorf("mount unit missing %q:\n%s", line, unit)
				}
			}
		})
	}
	for _, d := range []Disk{{UUID: "12AB-34CD", FSType: "vfat"}, {UUID: "12AB-34CD\nOptions=exec", FSType: "exfat"}, {UUID: "12AB-34CD", FSType: "exfat\nOptions=exec"}} {
		if _, err := archiveMountUnit(d, 997, 996); err == nil {
			t.Fatalf("invalid mount accepted: %+v", d)
		}
	}
	required := archiveMountOptions("exfat", 997, 996)
	if !mountOptionsInclude("relatime,"+required+",errors=remount-ro", required) {
		t.Fatal("valid exFAT mount options rejected")
	}
	for _, options := range []string{"rw,nosuid,nodev,noexec", strings.ReplaceAll(required, "uid=997", "uid=0"), strings.ReplaceAll(required, "fmask=0177", "fmask=0022")} {
		if mountOptionsInclude(options, required) {
			t.Fatalf("wrong exFAT permissions accepted: %s", options)
		}
	}
}

func TestDiskSelectionAPI(t *testing.T) {
	for _, tc := range []struct {
		name, uuid string
		helperExit int
		status     int
	}{
		{"exfat", "12AB-34CD", 0, http.StatusNoContent},
		{"ext4", testExt4UUID, 0, http.StatusNoContent},
		{"bad UUID", "12AB-34CD.service", 0, http.StatusBadRequest},
		{"helper failure", "12AB-34CD", 1, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			log := filepath.Join(dir, "systemctl-args")
			must(t, os.WriteFile(filepath.Join(dir, "systemctl"), []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$DOZOR_TEST_SYSTEMCTL_LOG\"\nexit \"$DOZOR_TEST_SYSTEMCTL_EXIT\"\n"), 0700))
			t.Setenv("PATH", dir)
			t.Setenv("DOZOR_TEST_SYSTEMCTL_LOG", log)
			t.Setenv("DOZOR_TEST_SYSTEMCTL_EXIT", fmt.Sprint(tc.helperExit))
			cf, err := LoadConfig(filepath.Join(dir, "config.json"))
			must(t, err)
			a, err := NewApp(cf, Binaries{}, true, dir, filepath.Join(dir, "s.sock"), "test")
			must(t, err)
			a.sessions["disk-test"] = Session{CSRF: "csrf", Expires: time.Now().Add(time.Hour)}
			r := httptest.NewRequest("POST", "/api/v1/disks/select", strings.NewReader(fmt.Sprintf(`{"uuid":%q}`, tc.uuid)))
			r.AddCookie(&http.Cookie{Name: "dozor_session", Value: "disk-test"})
			r.Header.Set("X-CSRF-Token", "csrf")
			w := httptest.NewRecorder()
			a.Handler().ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			saved, err := LoadConfig(cf.Path)
			must(t, err)
			wantUUID := ""
			if tc.status == http.StatusNoContent {
				wantUUID = tc.uuid
			}
			if saved.Get().DiskUUID != wantUUID {
				t.Fatalf("saved UUID: %q, want %q", saved.Get().DiskUUID, wantUUID)
			}
			args, err := os.ReadFile(log)
			if tc.name == "bad UUID" {
				if !os.IsNotExist(err) {
					t.Fatal("invalid UUID reached privileged helper")
				}
			} else {
				must(t, err)
				if string(args) != "start\ndozor-disk-prepare@"+tc.uuid+".service\n" {
					t.Fatalf("unexpected helper arguments: %q", args)
				}
			}
		})
	}
}
