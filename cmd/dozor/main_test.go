package main

import (
	"crypto/ed25519"
	"crypto/rand"
	project "dozor"
	"dozor/internal/dozor"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSignReleaseUsesProjectVersion(t *testing.T) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key, bundle, out := filepath.Join(dir, "signing.key"), filepath.Join(dir, "bundle.tar.gz"), filepath.Join(dir, "release.json")
	if err = os.WriteFile(key, []byte(base64.StdEncoding.EncodeToString(priv)), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(bundle, []byte("release fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	os.Args = []string{"dozor", "sign", "--key", key, "--bundle", bundle, "--url", "https://example.com/bundle.tar.gz", "--out", out}
	if err = signRelease(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var s dozor.SignedRelease
	if err = json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.Release.Version != project.Version() {
		t.Fatalf("signed version = %q, want %q", s.Release.Version, project.Version())
	}
	if err = dozor.VerifyRelease(s, pub); err != nil {
		t.Fatal(err)
	}
}

func TestSignRejectsVersionOverride(t *testing.T) {
	args := os.Args
	t.Cleanup(func() { os.Args = args })
	os.Args = []string{"dozor", "sign", "--version", "v999.0.0"}
	if err := signRelease(); err == nil || !strings.Contains(err.Error(), "must match project VERSION") {
		t.Fatalf("mismatching version accepted: %v", err)
	}
}
