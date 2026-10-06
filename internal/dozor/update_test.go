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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func releaseFixture(t *testing.T, malicious string, overrides ...map[string]string) (*Updater, SignedRelease) {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	files := map[string]string{"VERSION": "v1.1.0\n", "bin/dozor": systemFixtureBinary("v1.1.0", "candidate policy"), "bin/mediamtx": "#!/bin/sh\nexit 0\n", "bin/ffmpeg": "#!/bin/sh\nexit 0\n", "bin/ffprobe": "#!/bin/sh\nexit 0\n"}
	for _, override := range overrides {
		for path, data := range override {
			files[path] = data
		}
	}
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
	must(t, WriteJSON(filepath.Join(u.Root, "pending-update.json"), UpdateJournal{Previous: previous, Candidate: target}))
	must(t, u.Recover())
	target, e = filepath.EvalSymlinks(filepath.Join(u.Root, "current"))
	must(t, e)
	if target != previous {
		t.Fatal(target)
	}
}
func TestVersionOrdering(t *testing.T) {
	for _, tc := range []struct {
		candidate, current string
		newer              bool
	}{
		{"v1.0.0", "v1.0.0", false},
		{"v1.0.0", "1.0.0", false},
		{"v1.9.0", "v1.10.0", false},
		{"v1.10.0", "v1.9.0", true},
		{"v2.0.0", "v1.10.0", true},
		{"v1.0.1", "v1.0.0", true},
		{"v1.0.0", "v1.0.1", false},
		{"v1.0.0", "dev", true},
		{"../../etc", "dev", false},
		{"v2.0.0-beta.1", "v1.0.0", false},
		{"v01.0.0", "v1.0.0", false},
		{"v1.0.18446744073709551616", "v1.0.18446744073709551615", true},
	} {
		if got := Newer(tc.candidate, tc.current); got != tc.newer {
			t.Errorf("Newer(%q, %q) = %v, want %v", tc.candidate, tc.current, got, tc.newer)
		}
	}
}

type updateRoundTripFunc func(*http.Request) (*http.Response, error)

func (f updateRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func updateResponse(r *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}
}

func TestFetchLatestGitHubRelease(t *testing.T) {
	for _, tc := range []struct {
		name, latest, wantError string
		status                  int
		tamper                  bool
	}{
		{"stable", `{"tag_name":"v1.1.0","body":"release notes"}`, "", 200, false},
		{"wrong tag", `{"tag_name":"v1.2.0"}`, "не совпадает с тегом", 200, false},
		{"draft", `{"tag_name":"v1.1.0","draft":true}`, "стабильного тега", 200, false},
		{"prerelease", `{"tag_name":"v1.1.0","prerelease":true}`, "стабильного тега", 200, false},
		{"beta tag", `{"tag_name":"v1.1.0-beta.1"}`, "стабильного тега", 200, false},
		{"bad response", `{`, "некорректный релиз", 200, false},
		{"missing release", `{}`, "(404)", 404, false},
		{"rate limit", `{}`, "(403)", 403, false},
		{"bad signature", `{"tag_name":"v1.1.0"}`, "неверная подпись", 200, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, s := releaseFixture(t, "")
			if tc.tamper {
				s.Release.Size++
			}
			manifest, e := json.Marshal(s)
			must(t, e)
			var requests []string
			u.Client = &http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				requests = append(requests, r.URL.String())
				if r.URL.String() == "https://api.github.com/repos/Jourloy/dozor/releases/latest" {
					return updateResponse(r, tc.status, tc.latest), nil
				}
				if r.URL.Host == "github.com" && strings.HasPrefix(r.URL.Path, "/Jourloy/dozor/releases/download/") {
					return updateResponse(r, 200, string(manifest)), nil
				}
				t.Fatalf("unexpected update request: %s", r.URL)
				return nil, errors.New("unexpected request")
			})}
			got, e := u.Fetch(context.Background(), DefaultReleaseURL)
			if tc.wantError != "" {
				if e == nil || !strings.Contains(e.Error(), tc.wantError) {
					t.Fatalf("Fetch error = %v, want %q", e, tc.wantError)
				}
				return
			}
			must(t, e)
			if got.Release != s.Release || len(requests) != 2 || requests[1] != "https://github.com/Jourloy/dozor/releases/download/v1.1.0/release.json" {
				t.Fatalf("latest release was not pinned to its tag: %+v, %v", got, requests)
			}
		})
	}
}

func TestFetchCustomReleaseManifest(t *testing.T) {
	u, s := releaseFixture(t, "")
	manifest, e := json.Marshal(s)
	must(t, e)
	u.Client = &http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://updates.example/release.json" {
			t.Fatalf("unexpected request: %s", r.URL)
		}
		return updateResponse(r, 200, string(manifest)), nil
	})}
	got, e := u.Fetch(context.Background(), "https://updates.example/release.json")
	must(t, e)
	if got.Release != s.Release {
		t.Fatal("custom release source was ignored")
	}
}

func TestUpdateComparesRunningVersion(t *testing.T) {
	for _, tc := range []struct {
		name, running, disk, afterDownload string
		busy, apply                        bool
	}{
		{name: "new release", running: "v1.0.0", disk: "v9.0.0", apply: true},
		{name: "same release", running: "v1.1.0", disk: "v0.1.0"},
		{name: "running version ahead", running: "v1.2.0", disk: "v0.1.0"},
		{name: "legacy version without prefix", running: "1.1.0", disk: "v0.1.0"},
		{name: "updated during download", running: "v1.0.0", disk: "v0.1.0", afterDownload: "v1.2.0"},
		{name: "active recording", running: "v1.0.0", disk: "v0.1.0", busy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u, s := releaseFixture(t, "")
			must(t, os.WriteFile(filepath.Join(u.Root, "current", "VERSION"), []byte(tc.disk), 0644))
			manifest, e := json.Marshal(s)
			must(t, e)
			transport := u.Client.Transport
			u.Client.Transport = updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Host {
				case "api.github.com":
					return updateResponse(r, 200, `{"tag_name":"v1.1.0"}`), nil
				case "github.com":
					return updateResponse(r, 200, string(manifest)), nil
				default:
					return transport.RoundTrip(r)
				}
			})
			reads, prepares, starts, stops := 0, 0, 0, 0
			client := &http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				switch r.URL.Path {
				case "/health":
					reads++
					version := tc.running
					if reads > 1 && tc.afterDownload != "" {
						version = tc.afterDownload
					}
					return updateResponse(r, 200, `{"version":"`+version+`","ready":true}`), nil
				case "/prepare-update":
					prepares++
					if tc.busy {
						return updateResponse(r, 409, ""), nil
					}
					return updateResponse(r, 204, ""), nil
				default:
					t.Fatalf("unexpected control request: %s", r.URL)
					return nil, errors.New("unexpected request")
				}
			})}
			u.Start = func() error { starts++; return nil }
			u.Stop = func() error { stops++; return nil }
			u.Healthy = func(_ context.Context, version string) bool { return version == s.Release.Version }
			var status string
			must(t, u.updateRunning(context.Background(), "", client, func(s string) { status = s }))
			target, e := filepath.EvalSymlinks(filepath.Join(u.Root, "current"))
			must(t, e)
			if tc.apply {
				if filepath.Base(target) != s.Release.Version || prepares != 1 || starts != 1 || stops != 1 || !strings.HasPrefix(status, "Установлена v1.1.0 · ") {
					t.Fatalf("update not applied: %s prepares=%d starts=%d stops=%d status=%q", target, prepares, starts, stops, status)
				}
			} else if filepath.Base(target) != "v1.0.0" || starts != 0 || stops != 0 {
				t.Fatalf("unexpected update: %s starts=%d stops=%d", target, starts, stops)
			}
			if tc.busy && (prepares != 1 || !strings.HasPrefix(status, "Обновление загружено")) {
				t.Fatalf("active recording did not defer update: %q", status)
			}
			if _, e := os.Stat(filepath.Join(u.Root, "pending-update.json")); !errors.Is(e, os.ErrNotExist) {
				t.Fatalf("unexpected recovery journal after update: %v", e)
			}
		})
	}
}

func TestUpdateDefersWhenRunningVersionUnavailable(t *testing.T) {
	for _, body := range []string{`{}`, `{"version":""}`, `{"version":"invalid"}`, `{`} {
		t.Run(body, func(t *testing.T) {
			u, _ := releaseFixture(t, "")
			u.Client = &http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				t.Fatal("must not fetch a release without the running version")
				return nil, errors.New("unexpected request")
			})}
			client := &http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				return updateResponse(r, 200, body), nil
			})}
			var status string
			if e := u.updateRunning(context.Background(), DefaultReleaseURL, client, func(s string) { status = s }); e == nil || !strings.Contains(status, "версию работающей") {
				t.Fatalf("unknown running version was accepted: %v, %q", e, status)
			}
		})
	}
}

func TestPreflightRejectsWrongBinaryVersion(t *testing.T) {
	u, s := releaseFixture(t, "")
	staged, e := u.Stage(context.Background(), s)
	must(t, e)
	if e = preflightRelease(context.Background(), staged, "v1.2.0"); e == nil || !strings.Contains(e.Error(), "версия бинарника") {
		t.Fatalf("mislabeled binary accepted: %v", e)
	}
}

func TestUpdateReleasePermissionsWithPrivateUmask(t *testing.T) {
	u, s := releaseFixture(t, "LICENSES/test.txt")
	previous := syscall.Umask(0077)
	defer syscall.Umask(previous)
	staged, e := u.Stage(context.Background(), s)
	must(t, e)
	for path, want := range map[string]os.FileMode{
		filepath.Dir(staged):                       0755,
		staged:                                     0755,
		filepath.Join(staged, "bin"):               0755,
		filepath.Join(staged, "bin/dozor"):         0555,
		filepath.Join(staged, "bin/mediamtx"):      0555,
		filepath.Join(staged, "bin/ffmpeg"):        0555,
		filepath.Join(staged, "bin/ffprobe"):       0555,
		filepath.Join(staged, "VERSION"):           0444,
		filepath.Join(staged, "LICENSES"):          0755,
		filepath.Join(staged, "LICENSES/test.txt"): 0444,
	} {
		info, err := os.Stat(path)
		must(t, err)
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s permissions = %04o, want %04o", path, got, want)
		}
	}
}

func TestDefaultReleaseSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c, e := LoadConfig(path)
	must(t, e)
	if c.Get().ReleaseURL != DefaultReleaseURL {
		t.Fatal("new installations have no release source")
	}
	for _, source := range []string{"", "https://updates.example/release.json"} {
		next := c.Get()
		next.AutoUpdate = false
		next.ReleaseURL = source
		// Simulate a legacy config saved before a default source existed.
		must(t, WriteJSON(path, next))
		loaded, e := LoadConfig(path)
		must(t, e)
		want := source
		if want == "" {
			want = DefaultReleaseURL
		}
		if loaded.Get().ReleaseURL != want || loaded.Get().AutoUpdate {
			t.Fatalf("legacy config changed incorrectly: %+v", loaded.Get())
		}
		must(t, c.Save(next))
		if c.Get().ReleaseURL != want || c.Get().AutoUpdate {
			t.Fatalf("saved source changed incorrectly: %+v", c.Get())
		}
	}
}
