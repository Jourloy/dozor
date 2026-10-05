package dozor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func releaseFixture(t *testing.T, malicious string) (*Updater, SignedRelease) {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	files := map[string]string{"VERSION": "v1.1.0\n", "bin/dozor": "binary", "bin/mediamtx": "media", "bin/ffmpeg": "ffmpeg", "bin/ffprobe": "probe"}
	if malicious != "" {
		files[malicious] = "bad"
	}
	for p, data := range files {
		must(t, tw.WriteHeader(&tar.Header{Name: p, Typeflag: tar.TypeReg, Size: int64(len(data)), Mode: 0555}))
		_, e := tw.Write([]byte(data))
		must(t, e)
	}
	must(t, tw.Close())
	must(t, gz.Close())
	data := b.Bytes()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	t.Cleanup(srv.Close)
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	must(t, e)
	sum := sha256.Sum256(data)
	rel := Release{Version: "v1.1.0", Platform: "linux-arm64", URL: srv.URL + "/bundle.tar.gz", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}
	payload, _ := json.Marshal(rel)
	signed := SignedRelease{rel, base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload))}
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	previous := filepath.Join(root, "releases", "v1.0.0")
	must(t, os.MkdirAll(previous, 0755))
	must(t, os.Symlink("releases/v1.0.0", filepath.Join(root, "current")))
	return &Updater{Root: root, PublicKey: pub, Client: srv.Client()}, signed
}
func TestUpdateSignatureAndCorruption(t *testing.T) {
	u, s := releaseFixture(t, "")
	must(t, VerifyRelease(s, u.PublicKey))
	s.Release.Size++
	if VerifyRelease(s, u.PublicKey) == nil {
		t.Fatal("tampered signature accepted")
	}
	_, s = releaseFixture(t, "")
	if VerifyRelease(s, u.PublicKey) == nil {
		t.Fatal("wrong signer accepted")
	}
}
func TestUpdateRejectsTraversal(t *testing.T) {
	for _, p := range []string{"../escape", "/etc/passwd", "bin/../../escape"} {
		t.Run(p, func(t *testing.T) {
			u, s := releaseFixture(t, p)
			if _, e := u.Stage(context.Background(), s); e == nil {
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}
func TestUpdateRollbackOnFailedHealth(t *testing.T) {
	u, s := releaseFixture(t, "")
	starts := 0
	u.Start = func() error { starts++; return nil }
	u.Stop = func() error { return nil }
	u.Healthy = func(context.Context, string) bool { return false }
	if u.Apply(context.Background(), s) == nil {
		t.Fatal("unhealthy release accepted")
	}
	target, e := filepath.EvalSymlinks(filepath.Join(u.Root, "current"))
	must(t, e)
	if filepath.Base(target) != "v1.0.0" || starts != 2 {
		t.Fatalf("rollback: %s starts=%d", target, starts)
	}
}
func TestUpdateCommitAndBootRecovery(t *testing.T) {
	u, s := releaseFixture(t, "")
	u.Healthy = func(context.Context, string) bool { return true }
	must(t, u.Apply(context.Background(), s))
	target, e := filepath.EvalSymlinks(filepath.Join(u.Root, "current"))
	must(t, e)
	if filepath.Base(target) != "v1.1.0" {
		t.Fatal(target)
	}
	previous := filepath.Join(u.Root, "releases", "v1.0.0")
	must(t, WriteJSON(filepath.Join(u.Root, "pending-update.json"), UpdateJournal{previous, target}))
	must(t, u.Recover())
	target, e = filepath.EvalSymlinks(filepath.Join(u.Root, "current"))
	must(t, e)
	if target != previous {
		t.Fatal(target)
	}
}
func TestVersionOrdering(t *testing.T) {
	if Newer("v1.0.0", "v1.0.0") || Newer("v1.9.0", "v1.10.0") || !Newer("v2.0.0", "v1.10.0") || Newer("../../etc", "dev") {
		t.Fatal("version ordering")
	}
}
