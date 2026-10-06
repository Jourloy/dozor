package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	project "dozor"
	"dozor/internal/dozor"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var version = project.Version()

func main() {
	if e := run(); e != nil {
		log.Print(e)
		os.Exit(1)
	}
}
func run() error {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version":
			fmt.Println(version)
			return nil
		case "hook":
			return hook()
		case "keygen":
			return keygen()
		case "sign":
			return signRelease()
		case "system":
			return system()
		case "serve":
			os.Args = append(os.Args[:1], os.Args[2:]...)
		}
	}
	f := flag.NewFlagSet("dozor serve", flag.ContinueOnError)
	config := f.String("config", "/var/lib/dozor/config.json", "configuration file")
	state := f.String("state", "/var/lib/dozor", "local state directory")
	socket := f.String("socket", "/run/dozor/control.sock", "private control socket")
	dev := f.Bool("development", false, "explicitly allow a directory instead of an ext4/exFAT mount; localhost only")
	archive := f.String("archive", "", "override archive in development mode")
	listen := f.String("listen", "", "override listen address")
	if e := f.Parse(os.Args[1:]); e != nil {
		return e
	}
	cfg, e := dozor.LoadConfig(*config)
	if e != nil {
		return e
	}
	c := cfg.Get()
	if *listen != "" {
		c.Listen = *listen
	}
	if *dev {
		host, _, e := net.SplitHostPort(c.Listen)
		if e != nil || host != "127.0.0.1" && host != "::1" && host != "localhost" {
			return errors.New("development mode must listen on loopback")
		}
		if *archive != "" {
			c.Archive, *archive = filepath.Clean(*archive), filepath.Clean(*archive)
		}
		if e = os.MkdirAll(c.Archive, 0700); e != nil {
			return e
		}
	} else if *archive != "" {
		return errors.New("archive override requires development mode")
	}
	if e = cfg.Save(c); e != nil {
		return e
	}
	app, e := dozor.NewApp(cfg, dozor.BundleBinaries(), *dev, *state, *socket, version)
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	server := &http.Server{Addr: c.Listen, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16384}
	errch := make(chan error, 2)
	appDone := make(chan struct{})
	go func() { errch <- app.Run(ctx); close(appDone) }()
	go func() {
		if c.TLSCert != "" && c.TLSKey != "" {
			errch <- server.ListenAndServeTLS(c.TLSCert, c.TLSKey)
		} else {
			errch <- server.ListenAndServe()
		}
	}()
	log.Printf("Dozor %s listening on %s", version, c.Listen)
	if c.PasswordHash == "" {
		log.Printf("First-run token file: %s", filepath.Join(*state, "setup-token"))
	}
	select {
	case <-ctx.Done():
	case e = <-errch:
		if errors.Is(e, http.ErrServerClosed) {
			e = nil
		}
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	_ = server.Shutdown(shutdown)
	// Wait for media processes and the archive database to close before exit.
	select {
	case <-appDone:
	case <-time.After(30 * time.Second):
		return errors.New("shutdown timed out")
	}
	return e
}
func hook() error {
	f := flag.NewFlagSet("hook", flag.ContinueOnError)
	socket := f.String("socket", "/run/dozor/control.sock", "control socket")
	if e := f.Parse(os.Args[2:]); e != nil {
		return e
	}
	duration, e := strconv.ParseFloat(os.Getenv("MTX_SEGMENT_DURATION"), 64)
	if e != nil {
		return e
	}
	b, _ := json.Marshal(map[string]any{"path": os.Getenv("MTX_SEGMENT_PATH"), "duration": duration})
	res, e := dozor.UnixClient(*socket).Post("http://unix/segment", "application/json", bytes.NewReader(b))
	if e != nil {
		return errors.New("segment notification deferred to reconciliation")
	}
	defer res.Body.Close()
	io.Copy(io.Discard, res.Body)
	if res.StatusCode != 204 {
		return errors.New("segment notification rejected")
	}
	return nil
}
func system() error {
	if len(os.Args) < 3 {
		return errors.New("system select-disk UUID | update | recover | integration")
	}
	switch os.Args[2] {
	case "integration":
		return dozor.PrintSystemIntegration(os.Stdout, version)
	case "select-disk":
		if len(os.Args) != 4 {
			return errors.New("UUID required")
		}
		return dozor.MountDisk(os.Args[3])
	case "update":
		return dozor.SystemUpdate(context.Background(), "/var/lib/dozor/config.json", false)
	case "recover":
		return dozor.SystemUpdate(context.Background(), "/var/lib/dozor/config.json", true)
	}
	return errors.New("unknown system operation")
}
func keygen() error {
	if len(os.Args) != 3 {
		return errors.New("usage: dozor keygen PRIVATE_KEY_PATH")
	}
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		return e
	}
	path := os.Args[2]
	if _, e = os.Stat(path); !errors.Is(e, os.ErrNotExist) {
		return errors.New("refusing to overwrite key")
	}
	if e = dozor.AtomicWrite(path, []byte(base64.StdEncoding.EncodeToString(priv)), 0600); e != nil {
		return e
	}
	return dozor.AtomicWrite(path+".pub", []byte(base64.StdEncoding.EncodeToString(pub)), 0644)
}
func signRelease() error {
	f := flag.NewFlagSet("sign", flag.ContinueOnError)
	key := f.String("key", "", "private key file")
	bundle := f.String("bundle", "", "bundle tar.gz")
	v := f.String("version", version, "release version (defaults to project VERSION)")
	address := f.String("url", "", "HTTPS bundle URL")
	out := f.String("out", "release.json", "signed metadata")
	if e := f.Parse(os.Args[2:]); e != nil {
		return e
	}
	if *v != version {
		return fmt.Errorf("release version must match project VERSION (%s)", version)
	}
	b, e := os.ReadFile(*key)
	if e != nil {
		return e
	}
	priv, e := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if e != nil || len(priv) != ed25519.PrivateKeySize {
		return errors.New("invalid private key")
	}
	sha, _, size, e := dozor.HashFile(*bundle)
	if e != nil {
		return e
	}
	r := dozor.Release{Version: *v, Platform: "linux-arm64", URL: *address, SHA256: sha, Size: size}
	payload, _ := json.Marshal(r)
	s := dozor.SignedRelease{Release: r, Signature: base64.StdEncoding.EncodeToString(ed25519.Sign(priv, payload))}
	if e = dozor.VerifyRelease(s, ed25519.PrivateKey(priv).Public().(ed25519.PublicKey)); e != nil {
		return e
	}
	return dozor.WriteJSON(*out, s)
}
