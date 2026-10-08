package dozor

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type HealthCheck struct {
	Ready bool   `json:"ready"`
	State string `json:"state"`
	Error string `json:"error,omitempty"`
}
type ApplicationHealth struct {
	Version            string           `json:"version"`
	Ready              bool             `json:"ready"`
	Core               HealthCheck      `json:"core"`
	Database           HealthCheck      `json:"database"`
	Updater            HealthCheck      `json:"updater"`
	Recorder           HealthCheck      `json:"recorder"`
	Recovery           RecoveryProgress `json:"recovery"`
	CheckedAt          int64            `json:"checked_at"`
	ArchiveWorkerReady bool             `json:"archive_worker_ready"`
	StartupStages      map[string]int64 `json:"startup_stages_ms,omitempty"`
	StartupMS          int64            `json:"startup_ms"`
}

func checkResult(err error) HealthCheck {
	if err != nil {
		return HealthCheck{State: "error", Error: err.Error()}
	}
	return HealthCheck{Ready: true, State: "ready"}
}
func (a *App) Health() ApplicationHealth {
	if h := a.healthView.Load(); h != nil {
		return h.(ApplicationHealth)
	}
	return ApplicationHealth{Version: a.Version, Core: HealthCheck{State: "starting"}, Database: HealthCheck{State: "starting"}, Updater: HealthCheck{State: "starting"}}
}
func (a *App) checkPublicAPI(ctx context.Context) error {
	if a.Development {
		return nil
	}
	c := a.Config.Get()
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	if host == "::" {
		host = "::1"
	}
	scheme := "http"
	transport := &http.Transport{Proxy: nil}
	defer transport.CloseIdleConnections()
	if c.TLSCert != "" {
		scheme = "https"
		certPEM, err := os.ReadFile(c.TLSCert)
		if err != nil {
			return err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(certPEM) {
			return errors.New("invalid local TLS certificate")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
		if block, _ := pem.Decode(certPEM); block != nil {
			if cert, err := x509.ParseCertificate(block.Bytes); err == nil {
				if len(cert.DNSNames) > 0 {
					transport.TLSClientConfig.ServerName = strings.Replace(cert.DNSNames[0], "*", "dozor", 1)
				} else if len(cert.IPAddresses) > 0 {
					transport.TLSClientConfig.ServerName = cert.IPAddresses[0].String()
				}
			}
		}
	}
	req, err := http.NewRequestWithContext(ctx, "GET", scheme+"://"+net.JoinHostPort(host, port)+"/api/v1/auth", nil)
	if err != nil {
		return err
	}
	res, err := (&http.Client{Transport: transport, Timeout: 2 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	var result map[string]any
	if res.StatusCode != 200 || json.NewDecoder(res.Body).Decode(&result) != nil {
		return errors.New("public API health check failed")
	}
	if _, ok := result["authenticated"].(bool); !ok {
		return errors.New("public API returned invalid authentication state")
	}
	return nil
}
func (a *App) runHealth(ctx context.Context) {
	var stateChecked, updaterChecked bool
	var stateErr, updaterErr error
	var readyAt int64
	for ctx.Err() == nil {
		h := ApplicationHealth{Version: a.Version, CheckedAt: time.Now().UnixMilli()}
		if !stateChecked {
			probe := filepath.Join(a.StateDir, ".health-"+ID())
			stateErr = AtomicWrite(probe, []byte("ready\n"), 0600)
			if stateErr == nil {
				stateErr = removeDurable(probe)
			}
			stateChecked = stateErr == nil
		}
		coreErr := stateErr
		if coreErr == nil {
			coreErr = ValidateConfig(a.Config.Get())
		}
		if coreErr == nil {
			coreErr = a.checkPublicAPI(ctx)
		}
		h.Core = checkResult(coreErr)
		if !updaterChecked {
			if a.Development {
				updaterErr = nil
			} else {
				checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				output, err := exec.CommandContext(checkCtx, a.Bins.Self, "system", "update-check").CombinedOutput()
				cancel()
				updaterErr = err
				if err != nil && len(output) > 0 {
					updaterErr = errors.New(strings.TrimSpace(string(output)))
				}
			}
			updaterChecked = updaterErr == nil
		}
		h.Updater = checkResult(updaterErr)
		h.Database = HealthCheck{State: "starting"}
		if a.mu.TryRLock() {
			r := a.runtime
			storageErr := a.storageError
			if r != nil {
				dbErr := r.Store.ProbeHealth(ctx)
				h.Database = checkResult(dbErr)
				if corruptCatalog(dbErr) {
					a.Reload()
				}
				h.ArchiveWorkerReady = r.workerReady.Load()
				h.StartupStages = map[string]int64{"catalog": r.catalogMS, "journal": r.journalMS.Load(), "buffer": r.bufferMS.Load()}
				h.Recovery = r.Store.RecoveryProgress()
				h.Recorder = HealthCheck{Ready: r.mediaSince.Load() > 0, State: "stopped"}
				if h.Recorder.Ready {
					h.Recorder.State = "running"
				}
			} else if storageErr != "" {
				// Disk absence is an environmental condition, not a broken candidate.
				if err := (Guard{Root: a.Config.Get().Archive, UUID: a.Config.Get().DiskUUID, Development: a.Development}).Check(); err != nil {
					h.Database = HealthCheck{Ready: true, State: "disk_unavailable", Error: storageErr}
				} else {
					h.Database = HealthCheck{State: "error", Error: storageErr}
				}
			}
			a.mu.RUnlock()
		}
		h.Ready = h.Core.Ready && h.Database.Ready && h.Updater.Ready
		if h.Ready && readyAt == 0 {
			readyAt = time.Now().UnixMilli()
			if started := a.runStarted.Load(); started > 0 {
				readyAt -= started
			}
		}
		h.StartupMS = readyAt
		a.healthView.Store(h)
		if h.Ready && !a.Development {
			a.reconcileUpdaterAfterLegacy(ctx)
		}
		if !pause(ctx, 2*time.Second) {
			return
		}
	}
}
func (a *App) reconcileUpdaterAfterLegacy(ctx context.Context) {
	unit, err := os.ReadFile("/etc/systemd/system/dozor-update.service")
	if err != nil || strings.Contains(string(unit), "ExecStart=/usr/local/libexec/dozor-system system update") || readUpdateStatus(a.StateDir).Running {
		return
	}
	// The old updater must finish before a second, maintenance-only invocation.
	marker := filepath.Join(a.StateDir, "update-reconcile-request")
	if err = AtomicWrite(marker, []byte("reconcile\n"), 0600); err != nil {
		return
	}
	child, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_ = exec.CommandContext(child, "systemctl", "--no-block", "start", "dozor-update.service").Run()
}
