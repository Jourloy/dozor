package dozor

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"sort"
	"time"
)

type ObjectStore interface {
	Put(context.Context, string, string, string, string, int64) error
}
type S3Client struct {
	client *s3.Client
	cfg    S3Config
}

func NewS3(c S3Config) *S3Client {
	cfg := aws.Config{Region: c.Region, Credentials: credentials.NewStaticCredentialsProvider(c.AccessKey, c.SecretKey, ""), HTTPClient: &http.Client{Timeout: 30 * time.Minute}, RetryMaxAttempts: 1, RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired, ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.UsePathStyle = c.PathStyle
		if c.Endpoint != "" {
			o.BaseEndpoint = aws.String(c.Endpoint)
		}
	})
	return &S3Client{client, c}
}
func (s *S3Client) matches(ctx context.Context, key, sha string, size int64) bool {
	h, e := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key)})
	return e == nil && aws.ToInt64(h.ContentLength) == size && h.Metadata["sha256"] == sha
}
func (s *S3Client) Put(ctx context.Context, key, file, sha, md string, size int64) error {
	if s.matches(ctx, key, sha, size) {
		return nil
	}
	f, e := os.Open(file)
	if e != nil {
		return e
	}
	defer f.Close()
	body := &rateReader{file: f, ctx: ctx, rate: s.cfg.BytesPerSecond, start: time.Now()}
	typ := "video/mp4"
	if filepath.Ext(file) == ".json" {
		typ = "application/json"
	}
	_, e = s.client.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String(s.cfg.Bucket), Key: aws.String(key), Body: body, ContentLength: aws.Int64(size), ContentMD5: aws.String(md), ContentType: aws.String(typ), Metadata: map[string]string{"sha256": sha}})
	if e != nil {
		if s.matches(ctx, key, sha, size) {
			return nil
		}
		return errors.New("S3: передача не подтверждена")
	}
	if !s.matches(ctx, key, sha, size) {
		return errors.New("S3: размер или контрольная сумма не подтверждены")
	}
	return nil
}

type rateReader struct {
	file  *os.File
	ctx   context.Context
	rate  int64
	start time.Time
	read  int64
}

func (r *rateReader) Read(p []byte) (int, error) {
	if r.rate > 0 && len(p) > 64*1024 {
		p = p[:64*1024]
	}
	n, e := r.file.Read(p)
	r.read += int64(n)
	if r.rate > 0 {
		expected := time.Duration(float64(r.read) / float64(r.rate) * float64(time.Second))
		if delay := expected - time.Since(r.start); delay > 0 && !pause(r.ctx, delay) {
			return n, r.ctx.Err()
		}
	}
	return n, e
}
func (r *rateReader) Seek(offset int64, whence int) (int64, error) {
	pos, e := r.file.Seek(offset, whence)
	r.read = 0
	r.start = time.Now()
	return pos, e
}
func TargetFingerprint(c S3Config) string {
	h := sha256.Sum256([]byte(c.Endpoint + "\n" + c.Region + "\n" + c.Bucket + "\n" + c.Prefix))
	return hex.EncodeToString(h[:])
}
func (s *Store) SetTarget(c S3Config) error {
	if err := s.Guard.Check(); err != nil {
		return err
	}
	fingerprint := TargetFingerprint(c)
	var prev string
	err := s.DB.QueryRow("SELECT value FROM metadata WHERE key='s3_target'").Scan(&prev)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	// Keep the destination beside the durable part sidecars. Rebuilding the
	// SQLite catalogue must not reset acknowledgements for the same bucket.
	targetFile := filepath.Join(s.Root, "s3-target")
	if errors.Is(err, sql.ErrNoRows) {
		data, readErr := os.ReadFile(targetFile)
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		prev = string(data)
	}
	if prev == fingerprint {
		if err = AtomicWrite(targetFile, []byte(fingerprint), 0600); err != nil {
			return err
		}
		_, err = s.DB.Exec("INSERT OR REPLACE INTO metadata VALUES('s3_target',?)", fingerprint)
		return err
	}
	evs, e := s.Events("", 1000000)
	if e != nil {
		return e
	}
	for _, ev := range evs {
		ev.Uploaded = false
		parts, e := s.Parts(ev.ID)
		if e != nil {
			return e
		}
		for _, p := range parts {
			p.Uploaded = false
			if p.Deleted {
				p.Lost = true
				ev.Lost = true
			}
			if e = s.SavePart(p); e != nil {
				return e
			}
			if !p.Deleted {
				if e = s.Enqueue("part", p.ID); e != nil {
					return e
				}
			}
		}
		if e = s.saveEvent(ev, ev.Status == "closed"); e != nil {
			return e
		}
		if ev.Status == "closed" {
			if e = s.Enqueue("event", ev.ID); e != nil {
				return e
			}
		}
	}
	if e = AtomicWrite(targetFile, []byte(fingerprint), 0600); e != nil {
		return e
	}
	_, e = s.DB.Exec("INSERT OR REPLACE INTO metadata VALUES('s3_target',?)", fingerprint)
	return e
}

type manifestReceipt struct {
	Target string `json:"target"`
	SHA256 string `json:"sha256"`
}

func UploadOne(ctx context.Context, s *Store, c S3Config, remote ObjectStore) (bool, error) {
	j, e := s.NextJob(time.Now())
	if errors.Is(e, sql.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if e = s.Guard.Check(); e != nil {
		return false, e
	}
	var rel, sha, md string
	var size int64
	var uploadedEvent Event
	if j.Kind == "part" {
		p, err := s.Part(j.Ref)
		if err != nil {
			return false, err
		}
		if p.Deleted || p.Uploaded {
			return true, s.DoneJob(j.ID)
		}
		rel = p.Path
		sha = p.SHA256
		md = p.MD5
		size = p.Size
	} else {
		// Read event and part markers as one snapshot of archive mutations.
		s.mutate.Lock()
		ev, err := s.Event(j.Ref)
		var parts []Part
		if err == nil {
			parts, err = s.Parts(ev.ID)
		}
		s.mutate.Unlock()
		if err != nil {
			return false, err
		}
		if ev.Uploaded {
			return true, s.DoneJob(j.ID)
		}
		if ev.Status != "closed" {
			return false, s.Retry(j, "событие ещё открыто")
		}
		uploadedEvent = ev
		pending := false
		for _, p := range parts {
			if !p.Uploaded && !p.Lost {
				pending = true
			}
		}
		if pending {
			return false, s.Retry(j, "ожидание частей")
		}
		// A portable manifest includes every part, including loss and gap information.
		ev.Uploaded = !ev.Lost
		rel = filepath.Join(eventDir(ev), "manifest.json")
		if err = writeArchiveManifest(filepath.Join(s.Root, rel), ev, parts); err != nil {
			return false, err
		}
		sha, md, size, e = HashFile(filepath.Join(s.Root, rel))
		if e != nil {
			return false, e
		}
	}
	key := path.Join(c.Prefix, filepath.ToSlash(rel))
	// Event.Uploaded is false when any part was lost, even after its manifest
	// was acknowledged. A separate receipt stops recovery from republishing an
	// unchanged manifest after backend retention has removed it from S3.
	receiptFile := filepath.Join(s.Root, eventDir(uploadedEvent), "manifest.receipt")
	receipt := manifestReceipt{Target: TargetFingerprint(c), SHA256: sha}
	acknowledged := false
	if j.Kind == "event" {
		data, err := os.ReadFile(receiptFile)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, err
		}
		if err == nil {
			var previous manifestReceipt
			if err = json.Unmarshal(data, &previous); err != nil {
				return false, err
			}
			acknowledged = previous == receipt
		}
	}
	if !acknowledged {
		if e = remote.Put(ctx, key, filepath.Join(s.Root, rel), sha, md, size); e != nil {
			_ = s.Retry(j, "S3 недоступен или отказал в доступе; повтор запланирован")
			return false, e
		}
		if j.Kind == "event" {
			if e = s.Guard.Check(); e != nil {
				return false, e
			}
			if e = WriteJSON(receiptFile, receipt); e != nil {
				return false, e
			}
		}
	}
	s.mutate.Lock()
	defer s.mutate.Unlock()
	if j.Kind == "part" {
		p, err := s.Part(j.Ref)
		if err != nil {
			return false, err
		}
		p.Uploaded = true
		wasLost := p.Lost
		p.Lost = false // A file unlinked during its in-flight upload may still arrive.
		if e = s.SavePart(p); e != nil {
			return false, e
		}
		if wasLost {
			ev, err := s.Event(p.EventID)
			if err != nil {
				return false, err
			}
			parts, err := s.Parts(ev.ID)
			if err != nil {
				return false, err
			}
			ev.Lost = false
			for _, part := range parts {
				ev.Lost = ev.Lost || part.Lost
			}
			if e = s.SaveEvent(ev); e != nil {
				return false, e
			}
		}
	} else {
		ev, err := s.Event(j.Ref)
		if err != nil {
			return false, err
		}
		// A disconnect or late tail can revise the manifest during its PUT.
		// Keep the job until the new recording metadata reaches S3 as well.
		if ev != uploadedEvent {
			return true, nil
		}
		ev.Uploaded = !ev.Lost
		if e = s.SaveEvent(ev); e != nil {
			return false, e
		}
	}
	return true, s.DoneJob(j.ID)
}

var _ io.ReadSeeker = (*rateReader)(nil)

// Preserve an equivalent old manifest byte-for-byte, including its part order.
// Receipts from older releases remain valid when tied timestamps change SQL order.
func writeArchiveManifest(file string, ev Event, parts []Part) error {
	type manifest struct {
		Event Event  `json:"event"`
		Parts []Part `json:"parts"`
	}
	var previous manifest
	if data, err := os.ReadFile(file); err == nil && json.Unmarshal(data, &previous) == nil && previous.Event == ev && len(previous.Parts) == len(parts) {
		before := append([]Part(nil), previous.Parts...)
		after := append([]Part(nil), parts...)
		sort.Slice(before, func(i, j int) bool { return before[i].ID < before[j].ID })
		sort.Slice(after, func(i, j int) bool { return after[i].ID < after[j].ID })
		if reflect.DeepEqual(before, after) {
			return nil
		}
	}
	return WriteJSON(file, manifest{ev, parts})
}
