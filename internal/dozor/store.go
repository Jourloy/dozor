package dozor

import (
	"database/sql"
	"encoding/json"
	"errors"
	_ "github.com/mattn/go-sqlite3"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Event struct {
	ID             string `json:"id"`
	CameraID       string `json:"camera_id"`
	Source         string `json:"source"`
	Start          int64  `json:"start"`
	End            int64  `json:"end"`
	Cursor         int64  `json:"cursor"`
	Status         string `json:"status"`
	ShortPrebuffer bool   `json:"short_prebuffer"`
	Incomplete     bool   `json:"incomplete"`
	Uploaded       bool   `json:"uploaded"`
	Lost           bool   `json:"lost"`
	DisconnectedAt int64  `json:"disconnected_at,omitempty"`
}
type Segment struct {
	Path     string `json:"path"`
	CameraID string `json:"camera_id"`
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
}
type Part struct {
	ID             string `json:"id"`
	EventID        string `json:"event_id"`
	CameraID       string `json:"camera_id"`
	Path           string `json:"path"`
	Start          int64  `json:"start"`
	End            int64  `json:"end"`
	Size           int64  `json:"size"`
	SHA256         string `json:"sha256"`
	MD5            string `json:"md5"`
	Uploaded       bool   `json:"uploaded"`
	Deleted        bool   `json:"deleted"`
	Lost           bool   `json:"lost"`
	Gaps           []Gap  `json:"gaps,omitempty"`
	DisconnectedAt int64  `json:"disconnected_at,omitempty"`
}
type Gap struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}
type UploadJob struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Ref      string `json:"ref"`
	Attempts int    `json:"attempts"`
	Next     int64  `json:"next"`
	Error    string `json:"error"`
}
type Notice struct {
	At      int64  `json:"at"`
	Message string `json:"message"`
}
type Store struct {
	DB     *sql.DB
	Root   string
	Guard  Guard
	mutate sync.Mutex
}

func OpenStore(g Guard) (*Store, error) {
	if e := g.Check(); e != nil {
		return nil, e
	}
	if e := os.MkdirAll(filepath.Join(g.Root, "events"), 0700); e != nil {
		return nil, e
	}
	db, e := sql.Open("sqlite3", filepath.Join(g.Root, "catalog.sqlite")+"?_journal_mode=WAL&_synchronous=FULL&_busy_timeout=5000&_foreign_keys=on")
	if e != nil {
		return nil, e
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`CREATE TABLE IF NOT EXISTS events(id TEXT PRIMARY KEY,camera TEXT NOT NULL,start INTEGER,end INTEGER,cursor INTEGER,status TEXT,payload TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS events_camera_end ON events(camera,end);
 CREATE TABLE IF NOT EXISTS segments(path TEXT PRIMARY KEY,camera TEXT,start INTEGER,end INTEGER);
 CREATE INDEX IF NOT EXISTS segments_time ON segments(camera,start);
 CREATE TABLE IF NOT EXISTS parts(id TEXT PRIMARY KEY,event TEXT,start INTEGER,end INTEGER,uploaded INTEGER DEFAULT 0,deleted INTEGER DEFAULT 0,payload TEXT NOT NULL);
 CREATE INDEX IF NOT EXISTS parts_event ON parts(event,start);
 CREATE TABLE IF NOT EXISTS jobs(id TEXT PRIMARY KEY,kind TEXT,ref TEXT,attempts INTEGER DEFAULT 0,next INTEGER DEFAULT 0,error TEXT DEFAULT '');
 CREATE TABLE IF NOT EXISTS notices(at INTEGER,message TEXT);
 CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value TEXT);
 CREATE TABLE IF NOT EXISTS availability(camera TEXT,start INTEGER,end INTEGER,state TEXT,PRIMARY KEY(camera,start));
 CREATE TABLE IF NOT EXISTS camera_diagnostics(camera TEXT,kind TEXT,at INTEGER NOT NULL,message TEXT NOT NULL,PRIMARY KEY(camera,kind));
 CREATE INDEX IF NOT EXISTS availability_end ON availability(end);`)
	if e != nil {
		db.Close()
		return nil, e
	}
	s := &Store{DB: db, Root: g.Root, Guard: g}
	if e = s.Recover(); e != nil {
		db.Close()
		return nil, e
	}
	return s, nil
}
func (s *Store) Close() error { return s.DB.Close() }
func eventDir(e Event) string {
	return filepath.Join("events", e.CameraID, time.UnixMilli(e.Start).UTC().Format("2006-01-02"), e.ID)
}
func (s *Store) SaveEvent(e Event) error {
	if err := s.Guard.Check(); err != nil {
		return err
	}
	b, _ := json.Marshal(e)
	_, err := s.DB.Exec(`INSERT INTO events VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET end=excluded.end,cursor=excluded.cursor,status=excluded.status,payload=excluded.payload`, e.ID, e.CameraID, e.Start, e.End, e.Cursor, e.Status, string(b))
	if err != nil {
		return err
	}
	return WriteJSON(filepath.Join(s.Root, eventDir(e), "event.json"), e)
}
func (s *Store) Event(id string) (Event, error) {
	var b string
	var e Event
	err := s.DB.QueryRow("SELECT payload FROM events WHERE id=?", id).Scan(&b)
	if err == nil {
		err = json.Unmarshal([]byte(b), &e)
	}
	return e, err
}
func (s *Store) Events(camera string, limit int) ([]Event, error) {
	q := "SELECT payload FROM events"
	args := []any{}
	if camera != "" {
		q += " WHERE camera=?"
		args = append(args, camera)
	}
	q += " ORDER BY start DESC LIMIT ?"
	args = append(args, limit)
	rows, err := s.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	v := []Event{}
	for rows.Next() {
		var b string
		var e Event
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(b), &e); err != nil {
			return nil, err
		}
		v = append(v, e)
	}
	return v, rows.Err()
}
func (s *Store) PendingEvents() ([]Event, error) {
	rows, err := s.DB.Query("SELECT payload FROM events WHERE status!='closed' ORDER BY start")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	v := []Event{}
	for rows.Next() {
		var b string
		var e Event
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal([]byte(b), &e); err != nil {
			return nil, err
		}
		v = append(v, e)
	}
	return v, rows.Err()
}

func (s *Store) EventPage(camera string, from, to int64, offset int) ([]Event, error) {
	q := "SELECT payload FROM events WHERE end>? AND start<?"
	args := []any{from, to}
	if camera != "" {
		q += " AND camera=?"
		args = append(args, camera)
	}
	q += " ORDER BY start DESC,id DESC LIMIT 200 OFFSET ?"
	args = append(args, offset)
	rows, e := s.DB.Query(q, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	v := []Event{}
	for rows.Next() {
		var b string
		var ev Event
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(b), &ev); e != nil {
			return nil, e
		}
		v = append(v, ev)
	}
	return v, rows.Err()
}
func (s *Store) AddSegment(v Segment) error {
	_, e := s.DB.Exec("INSERT OR IGNORE INTO segments VALUES(?,?,?,?)", v.Path, v.CameraID, v.Start, v.End)
	return e
}
func (s *Store) HasSegment(path string) bool {
	var n int
	return s.DB.QueryRow("SELECT 1 FROM segments WHERE path=?", path).Scan(&n) == nil
}
func (s *Store) Segments(cam string, after, until int64) ([]Segment, error) {
	rows, e := s.DB.Query("SELECT path,camera,start,end FROM segments WHERE camera=? AND end>? AND start<? ORDER BY start", cam, after, until)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	v := []Segment{}
	for rows.Next() {
		var x Segment
		if e = rows.Scan(&x.Path, &x.CameraID, &x.Start, &x.End); e != nil {
			return nil, e
		}
		v = append(v, x)
	}
	return v, rows.Err()
}
func (s *Store) SavePart(p Part) error {
	if e := s.Guard.Check(); e != nil {
		return e
	}
	b, _ := json.Marshal(p)
	_, e := s.DB.Exec(`INSERT INTO parts VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET uploaded=excluded.uploaded,deleted=excluded.deleted,payload=excluded.payload`, p.ID, p.EventID, p.Start, p.End, p.Uploaded, p.Deleted, string(b))
	if e != nil {
		return e
	}
	return WriteJSON(filepath.Join(s.Root, p.Path+".json"), p)
}
func (s *Store) Part(id string) (Part, error) {
	var b string
	var p Part
	e := s.DB.QueryRow("SELECT payload FROM parts WHERE id=?", id).Scan(&b)
	if e == nil {
		e = json.Unmarshal([]byte(b), &p)
	}
	return p, e
}
func (s *Store) Parts(id string) ([]Part, error) {
	rows, e := s.DB.Query("SELECT payload FROM parts WHERE event=? ORDER BY start", id)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	v := []Part{}
	for rows.Next() {
		var b string
		var p Part
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		if e = json.Unmarshal([]byte(b), &p); e != nil {
			return nil, e
		}
		v = append(v, p)
	}
	return v, rows.Err()
}
func (s *Store) Enqueue(kind, ref string) error {
	_, e := s.DB.Exec("INSERT OR IGNORE INTO jobs(id,kind,ref) VALUES(?,?,?)", kind+":"+ref, kind, ref)
	return e
}
func (s *Store) NextJob(now time.Time) (UploadJob, error) {
	var j UploadJob
	e := s.DB.QueryRow("SELECT id,kind,ref,attempts,next,error FROM jobs WHERE next<=? ORDER BY CASE kind WHEN 'part' THEN 0 ELSE 1 END,next,id LIMIT 1", now.UnixMilli()).Scan(&j.ID, &j.Kind, &j.Ref, &j.Attempts, &j.Next, &j.Error)
	return j, e
}
func (s *Store) Retry(j UploadJob, message string) error {
	n := j.Attempts + 1
	delay := time.Second * time.Duration(1<<min(n, 10))
	_, e := s.DB.Exec("UPDATE jobs SET attempts=?,next=?,error=? WHERE id=?", n, time.Now().Add(delay).UnixMilli(), message, j.ID)
	return e
}
func (s *Store) DoneJob(id string) error {
	_, e := s.DB.Exec("DELETE FROM jobs WHERE id=?", id)
	return e
}
func (s *Store) Notice(msg string) {
	_, _ = s.DB.Exec("INSERT INTO notices VALUES(?,?)", time.Now().UnixMilli(), msg)
	_, _ = s.DB.Exec("DELETE FROM notices WHERE rowid NOT IN (SELECT rowid FROM notices ORDER BY at DESC LIMIT 500)")
}
func (s *Store) Notices() []Notice {
	v := []Notice{}
	rows, e := s.DB.Query("SELECT at,message FROM notices ORDER BY at DESC LIMIT 50")
	if e != nil {
		return v
	}
	defer rows.Close()
	for rows.Next() {
		var n Notice
		if rows.Scan(&n.At, &n.Message) == nil {
			v = append(v, n)
		}
	}
	return v
}
func (s *Store) QueueCount() int {
	var n int
	_ = s.DB.QueryRow("SELECT COUNT(*) FROM jobs").Scan(&n)
	return n
}
func (s *Store) PruneBuffer(now time.Time) error {
	rows, e := s.DB.Query(`SELECT path FROM segments s WHERE end<? AND NOT EXISTS(SELECT 1 FROM events e WHERE e.camera=s.camera AND e.status!='closed' AND e.cursor<s.end AND e.end>s.start)`, now.Add(-75*time.Second).UnixMilli())
	if e != nil {
		return e
	}
	var paths []string
	for rows.Next() {
		var p string
		if e = rows.Scan(&p); e != nil {
			rows.Close()
			return e
		}
		paths = append(paths, p)
	}
	e = rows.Close()
	if e != nil {
		return e
	}
	for _, p := range paths {
		if e = s.Guard.Check(); e != nil {
			return e
		}
		if e = os.Remove(filepath.Join(s.Root, p)); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		if _, e = s.DB.Exec("DELETE FROM segments WHERE path=?", p); e != nil {
			return e
		}
	}
	return nil
}
func (s *Store) PruneArchive(usage func() (uint64, uint64, error)) error {
	s.mutate.Lock()
	defer s.mutate.Unlock()
	used, total, e := usage()
	if e != nil || total == 0 {
		return e
	}
	if float64(used)/float64(total) < .90 {
		return nil
	}
	rows, e := s.DB.Query("SELECT payload FROM parts WHERE deleted=0 ORDER BY uploaded DESC,start")
	if e != nil {
		return e
	}
	var parts []Part
	for rows.Next() {
		var b string
		var p Part
		if e = rows.Scan(&b); e != nil {
			rows.Close()
			return e
		}
		if e = json.Unmarshal([]byte(b), &p); e != nil {
			rows.Close()
			return e
		}
		parts = append(parts, p)
	}
	rows.Close()
	for _, p := range parts {
		if float64(used)/float64(total) <= .85 {
			break
		}
		if e = s.Guard.Check(); e != nil {
			return e
		}
		p.Deleted = true
		p.Lost = !p.Uploaded
		if e = s.SavePart(p); e != nil {
			return e
		}
		if e = os.Remove(filepath.Join(s.Root, p.Path)); e != nil && !errors.Is(e, os.ErrNotExist) {
			return e
		}
		_ = s.DoneJob("part:" + p.ID)
		if p.Lost {
			ev, er := s.Event(p.EventID)
			if er == nil {
				ev.Lost = true
				ev.Uploaded = false
				_ = s.SaveEvent(ev)
			}
			s.Notice("Удалена невыгруженная запись: " + p.ID)
		}
		used, total, e = usage()
		if e != nil {
			return e
		}
	}
	return nil
}

// Sidecars are written before publication and retain tombstones after deletion.
func (s *Store) Recover() error {
	root := filepath.Join(s.Root, "events")
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(path) != ".json" || filepath.Base(path) == "manifest.json" {
			return nil
		}
		b, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		if filepath.Base(path) == "event.json" {
			var ev Event
			if json.Unmarshal(b, &ev) != nil || !safeID.MatchString(ev.ID) || !safeID.MatchString(ev.CameraID) {
				return errors.New("повреждён манифест события")
			}
			_, e = s.DB.Exec("INSERT OR IGNORE INTO events VALUES(?,?,?,?,?,?,?)", ev.ID, ev.CameraID, ev.Start, ev.End, ev.Cursor, ev.Status, string(b))
			return e
		}
		var p Part
		if json.Unmarshal(b, &p) != nil || !safeID.MatchString(p.ID) || !safeID.MatchString(p.EventID) {
			return errors.New("повреждён манифест записи")
		}
		full, e := checkedPath(s.Root, p.Path)
		if e != nil || full+".json" != path {
			return errors.New("неверный путь записи")
		}
		original := p
		// The database may have committed a deletion/upload before its sidecar.
		if current, err := s.Part(p.ID); err == nil {
			p = current
		} else if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if p.Deleted {
			if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			_ = os.Remove(full + ".partial")
		}
		if _, e = os.Stat(full); errors.Is(e, os.ErrNotExist) && !p.Deleted {
			if sha, _, size, err := HashFile(full + ".partial"); err == nil && sha == p.SHA256 && size == p.Size {
				if err = os.Rename(full+".partial", full); err != nil {
					return err
				}
			} else {
				p.Deleted = true
				p.Lost = !p.Uploaded
				s.Notice("При восстановлении не найдена запись: " + p.ID)
			}
		}
		if p.Deleted {
			if e = s.DoneJob("part:" + p.ID); e != nil {
				return e
			}
		}
		if p.Deleted != original.Deleted || p.Lost != original.Lost || p.Uploaded != original.Uploaded {
			return s.SavePart(p)
		}
		b, _ = json.Marshal(p)
		_, e = s.DB.Exec("INSERT OR IGNORE INTO parts VALUES(?,?,?,?,?,?,?)", p.ID, p.EventID, p.Start, p.End, p.Uploaded, p.Deleted, string(b))
		return e
	})
	if err != nil {
		return err
	}
	// An unfinished remux has no published identity and can be retried from
	// the pinned buffer. Remove leftovers only after sidecar recovery above.
	if err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if filepath.Ext(path) == ".partial" || len(d.Name()) > 7 && d.Name()[:7] == ".write-" {
			return os.Remove(path)
		}
		return nil
	}); err != nil {
		return err
	}
	// Reconcile a crash after publishing a part but before advancing its event cursor.
	evs, e := s.Events("", 1000000)
	if e != nil {
		return e
	}
	for _, ev := range evs {
		parts, e := s.Parts(ev.ID)
		if e != nil {
			return e
		}
		for _, p := range parts {
			if p.End > ev.Cursor {
				ev.Cursor = p.End
			}
			if p.Lost {
				ev.Lost = true
			}
			if !p.Uploaded && !p.Deleted {
				if e = s.Enqueue("part", p.ID); e != nil {
					return e
				}
			}
		}
		if ev.Status != "closed" {
			ev.Status = "closing"
			ev.Incomplete = true
		}
		if e = s.SaveEvent(ev); e != nil {
			return e
		}
		if ev.Status == "closed" && !ev.Uploaded {
			if e = s.Enqueue("event", ev.ID); e != nil {
				return e
			}
		}
	}
	return nil
}
