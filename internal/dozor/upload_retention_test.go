package dozor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func drainUploads(t *testing.T, s *Store, cfg S3Config, remote ObjectStore) {
	t.Helper()
	for range 20 {
		work, err := UploadOne(context.Background(), s, cfg, remote)
		must(t, err)
		if !work {
			return
		}
	}
	t.Fatal("upload queue did not drain")
}

func TestRetentionNeverReuploadsAcknowledgedFiles(t *testing.T) {
	for _, lost := range []bool{false, true} {
		t.Run(map[bool]string{false: "complete", true: "lost part"}[lost], func(t *testing.T) {
			s := testStore(t)
			cfg := S3Config{Bucket: "archive", Prefix: "dozor"}
			must(t, s.SetTarget(cfg))
			ev := Event{ID: ID(), CameraID: ID(), Start: 1000, End: 9000, Cursor: 9000, Status: "closed", Lost: lost}
			must(t, s.SaveEvent(ev))
			part := fixturePart(t, s, ev, 1000, 9000)
			if lost {
				missing := fixturePart(t, s, ev, 1000, 9000)
				missing.Deleted = true
				missing.Lost = true
				must(t, s.SavePart(missing))
				must(t, os.Remove(filepath.Join(s.Root, missing.Path)))
			}
			must(t, s.Enqueue("event", ev.ID))
			remote := &memoryRemote{}
			drainUploads(t, s, cfg, remote)
			if len(remote.keys) != 2 {
				t.Fatalf("initial uploads: %v", remote.keys)
			}

			// S3 has removed both objects. Any subsequent PUT would recreate
			// them, so the remote fails if even a verification upload is tried.
			remote.fail = true
			must(t, s.Enqueue("part", part.ID)) // crash before deleting an old job
			must(t, s.Enqueue("event", ev.ID))
			must(t, s.Recover())
			drainUploads(t, s, cfg, remote)
			if s.QueueCount() != 0 {
				t.Fatal("acknowledged jobs remained queued")
			}

			root, guard := s.Root, s.Guard
			must(t, s.Close())
			// Also recover from just the durable archive sidecars.
			for _, suffix := range []string{"", "-wal", "-shm"} {
				err := os.Remove(filepath.Join(root, "catalog.sqlite"+suffix))
				if err != nil && !os.IsNotExist(err) {
					t.Fatal(err)
				}
			}
			recovered, err := OpenStore(guard)
			must(t, err)
			defer recovered.Close()
			must(t, recovered.SetTarget(cfg))
			drainUploads(t, recovered, cfg, remote)
			if recovered.QueueCount() != 0 || len(remote.keys) != 2 {
				t.Fatalf("recovery republished files: %v", remote.keys)
			}

			// An explicit destination change still publishes the local archive.
			cfg.Bucket = "another-archive"
			remote.fail = false
			must(t, recovered.SetTarget(cfg))
			drainUploads(t, recovered, cfg, remote)
			if len(remote.keys) != 4 {
				t.Fatalf("new destination was skipped: %v", remote.keys)
			}
		})
	}
}

func TestManifestReceiptAllowsRevisionsAndDoesNotAcknowledgeFailures(t *testing.T) {
	s := testStore(t)
	cfg := S3Config{Bucket: "archive"}
	must(t, s.SetTarget(cfg))
	ev := Event{ID: ID(), CameraID: ID(), Start: 1000, End: 9000, Cursor: 9000, Status: "closed", Lost: true}
	must(t, s.SaveEvent(ev))
	must(t, s.Enqueue("event", ev.ID))
	remote := &memoryRemote{fail: true}
	_, err := UploadOne(context.Background(), s, cfg, remote)
	if err == nil {
		t.Fatal("expected upload failure")
	}
	if _, err = os.Stat(filepath.Join(s.Root, eventDir(ev), "manifest.receipt")); !os.IsNotExist(err) {
		t.Fatalf("failed PUT left a receipt: %v", err)
	}
	_, err = s.DB.Exec("UPDATE jobs SET next=0")
	must(t, err)
	remote.fail = false
	drainUploads(t, s, cfg, remote)
	ev.DisconnectedAt = 10000
	must(t, s.SaveEvent(ev))
	must(t, s.Enqueue("event", ev.ID))
	drainUploads(t, s, cfg, remote)
	if len(remote.keys) != 2 {
		t.Fatal("manifest revision was skipped", remote.keys)
	}
}
