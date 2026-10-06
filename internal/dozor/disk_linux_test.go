//go:build linux

package dozor

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// Run as a non-root user against a fresh, empty, already mounted exFAT test image.
// This test never formats a device; an explicit opt-in mount path is required.
func TestExFATArchiveIO(t *testing.T) {
	root := os.Getenv("DOZOR_EXFAT_TEST_ROOT")
	if root == "" {
		t.Skip("set DOZOR_EXFAT_TEST_ROOT to an empty exFAT test mount")
	}
	if os.Getuid() == 0 {
		t.Fatal("run as a non-root user to verify archive mount permissions")
	}
	entries, err := os.ReadDir(root)
	must(t, err)
	if len(entries) != 0 {
		t.Fatal("test requires an empty mount; refusing to write into an existing archive")
	}
	mount, err := archiveMount(root)
	must(t, err)
	if mount.FSType != "exfat" || !mountOptionsInclude(mount.Options, archiveMountOptions("exfat", os.Getuid(), os.Getgid())) {
		t.Fatalf("test mount must use Dozor exFAT options: %+v", mount)
	}
	g := Guard{Root: root, UUID: mount.UUID}
	s, err := OpenStore(g)
	must(t, err)
	t.Cleanup(func() {
		if s != nil {
			s.Close()
		}
	})
	ev := Event{ID: ID(), CameraID: ID(), Start: time.Now().UnixMilli(), Status: "closed"}
	must(t, s.SaveEvent(ev))
	p := fixturePart(t, s, ev, ev.Start, ev.Start+1000)
	for _, path := range []string{filepath.Join(root, "catalog.sqlite"), filepath.Join(root, eventDir(ev), "event.json"), filepath.Join(root, p.Path)} {
		info, err := os.Stat(path)
		must(t, err)
		stat := info.Sys().(*syscall.Stat_t)
		if info.Mode().Perm() != 0600 || stat.Uid != uint32(os.Getuid()) || stat.Gid != uint32(os.Getgid()) {
			t.Fatalf("wrong archive permissions for %s: %v, uid=%d gid=%d", path, info.Mode(), stat.Uid, stat.Gid)
		}
	}
	var mode, integrity string
	must(t, s.DB.QueryRow("PRAGMA journal_mode").Scan(&mode))
	must(t, s.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity))
	if mode != "wal" || integrity != "ok" {
		t.Fatalf("SQLite on exFAT: mode=%s integrity=%s", mode, integrity)
	}
	must(t, s.Close())
	s, err = OpenStore(g)
	must(t, err)
	got, err := s.Event(ev.ID)
	must(t, err)
	if got.CameraID != ev.CameraID {
		t.Fatalf("event did not survive reopening: %+v", got)
	}
	parts, err := s.Parts(ev.ID)
	must(t, err)
	if len(parts) != 1 || parts[0].SHA256 != p.SHA256 {
		t.Fatalf("part did not survive reopening: %+v", parts)
	}
	var jobs int
	must(t, s.DB.QueryRow("SELECT COUNT(*) FROM jobs WHERE ref=?", p.ID).Scan(&jobs))
	if jobs != 1 {
		t.Fatalf("upload queue did not survive reopening: jobs=%d", jobs)
	}
}
