package dozor

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCleanupRemovesEventsWithoutVideo(t *testing.T) {
	s := testStore(t)
	now := time.Date(2026, 10, 14, 22, 0, 0, 0, time.UTC)
	old := now.Add(-time.Hour).UnixMilli()
	cam := ID()
	closed := func(start, end int64) Event {
		ev := Event{ID: ID(), CameraID: cam, Start: start, End: end, Cursor: start, Status: "closed"}
		must(t, s.saveEvent(ev, true))
		return ev
	}
	// Never assembled: the recorder wrote nothing during the window.
	empty := closed(old-6*3600_000, old)
	// Its only part was pruned before it reached S3.
	lost := closed(old-60_000, old)
	gone := fixturePart(t, s, lost, lost.Start, lost.End)
	gone.Deleted, gone.Lost = true, true
	must(t, s.SavePart(gone))
	// Pruned locally but kept in S3: still has video.
	cloud := closed(old-60_000, old)
	uploaded := fixturePart(t, s, cloud, cloud.Start, cloud.End)
	uploaded.Uploaded, uploaded.Deleted = true, true
	must(t, s.SavePart(uploaded))
	// A minute of video followed by hours without any.
	tail := closed(old-6*3600_000, old)
	first := fixturePart(t, s, tail, tail.Start, tail.Start+60_000)
	tail.Cursor = first.End
	must(t, s.saveEvent(tail, false))
	// Too recent: a late file may still arrive.
	recent := closed(now.Add(-time.Minute).UnixMilli()-60_000, now.Add(-time.Minute).UnixMilli())
	// Disconnect clips keep their end so a late tail can reopen them.
	marked := closed(old-60_000, old)
	marked.DisconnectedAt = old
	markedPart := fixturePart(t, s, marked, marked.Start, marked.Start+5_000)
	marked.Cursor = markedPart.End
	must(t, s.saveEvent(marked, false))

	if _, err := s.CleanupEmptyEvents(now); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []Event{empty, lost} {
		if _, err := s.Event(ev.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("event without video kept: %v", err)
		}
		if _, err := os.Stat(filepath.Join(s.Root, eventDir(ev))); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("event directory kept: %v", err)
		}
	}
	if parts, err := s.Parts(lost.ID); err != nil || len(parts) != 0 {
		t.Fatalf("parts kept: %v %v", parts, err)
	}
	for _, ev := range []Event{cloud, recent, marked} {
		current, err := s.Event(ev.ID)
		must(t, err)
		if current.End != ev.End {
			t.Fatalf("event changed: %+v", current)
		}
	}
	trimmed, err := s.Event(tail.ID)
	must(t, err)
	if trimmed.End != first.End || trimmed.Uploaded {
		t.Fatalf("tail not trimmed: %+v", trimmed)
	}
	var jobs int
	must(t, s.DB.QueryRow("SELECT COUNT(*) FROM jobs WHERE ref IN (?,?,?)", empty.ID, lost.ID, gone.ID).Scan(&jobs))
	if jobs != 0 {
		t.Fatalf("jobs of deleted events remained: %d", jobs)
	}

	// The catalog rebuilt from sidecars must not bring them back.
	root, guard := s.Root, s.Guard
	must(t, s.Close())
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(filepath.Join(root, "catalog.sqlite"+suffix)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	s, err = OpenStore(guard)
	must(t, err)
	defer s.Close()
	must(t, s.Recover())
	for _, ev := range []Event{empty, lost} {
		if _, err := s.Event(ev.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("event without video recovered: %v", err)
		}
	}
	if _, err := s.Event(tail.ID); err != nil {
		t.Fatal(err)
	}
}

func TestEventWithoutVideoIsNotUploaded(t *testing.T) {
	s := testStore(t)
	cfg := S3Config{Bucket: "archive", Prefix: "dozor"}
	must(t, s.SetTarget(cfg))
	ev := Event{ID: ID(), CameraID: ID(), Start: 1000, End: 9000, Cursor: 1000, Status: "closed"}
	must(t, s.saveEvent(ev, true))
	remote := &memoryRemote{}
	drainUploads(t, s, cfg, remote)
	if len(remote.keys) != 0 || s.QueueCount() != 0 {
		t.Fatalf("empty event uploaded: %v", remote.keys)
	}
}
