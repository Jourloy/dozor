package dozor

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func systemFixtureBinary(version, rules string) string {
	b, _ := json.Marshal(systemManifest{Protocol: 1, Version: version, Polkit: rules})
	return fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = version ]; then\n  echo %s\nelif [ \"$1\" = system ] && [ \"$2\" = integration ]; then\n  cat <<'INTEGRATION'\n%s\nINTEGRATION\nelse\n  exit 1\nfi\n", version, b)
}

func setupSystemFixture(t *testing.T, u *Updater) *SystemIntegration {
	t.Helper()
	previous := filepath.Join(u.Root, "releases/v1.0.0/bin/dozor")
	must(t, AtomicWrite(previous, []byte(systemFixtureBinary("v1.0.0", "previous policy")), 0755))
	system := &SystemIntegration{Root: t.TempDir(), RecoveryBinary: previous, Reload: func() error { return nil }}
	must(t, AtomicWrite(filepath.Join(system.Root, systemHelperPath), []byte("old independently installed helper\n"), 0750))
	must(t, AtomicWrite(filepath.Join(system.Root, systemPolicyPath), []byte("old independently installed policy\n"), 0640))
	u.System = system
	return system
}

func assertSystemFile(t *testing.T, path, want string, mode os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(path)
	must(t, err)
	info, err := os.Stat(path)
	must(t, err)
	if string(b) != want || info.Mode().Perm() != mode {
		t.Fatalf("%s: content %q, mode %04o; want %q, %04o", path, b, info.Mode().Perm(), want, mode)
	}
}

func assertOriginalSystem(t *testing.T, u *Updater) {
	t.Helper()
	assertSystemFile(t, filepath.Join(u.System.Root, systemHelperPath), "old independently installed helper\n", 0750)
	assertSystemFile(t, filepath.Join(u.System.Root, systemPolicyPath), "old independently installed policy\n", 0640)
	target, err := filepath.EvalSymlinks(filepath.Join(u.Root, "current"))
	must(t, err)
	if filepath.Base(target) != "v1.0.0" {
		t.Fatalf("app not rolled back: %s", target)
	}
}

func assertNoSystemJournal(t *testing.T, u *Updater) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(u.Root, "pending-update.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remains: %v", err)
	}
	backups, err := filepath.Glob(filepath.Join(u.Root, ".system-backup-*"))
	must(t, err)
	if len(backups) != 0 {
		t.Fatalf("completed backups remain: %v", backups)
	}
}

func TestUpdateCommitsHelperAndPolicyBeforeStartup(t *testing.T) {
	u, release := releaseFixture(t, "")
	system := setupSystemFixture(t, u)
	reloads := 0
	system.Reload = func() error { reloads++; return nil }
	starts := 0
	u.Start = func() error {
		starts++
		assertSystemFile(t, filepath.Join(system.Root, systemHelperPath), systemFixtureBinary("v1.1.0", "candidate policy"), 0755)
		assertSystemFile(t, filepath.Join(system.Root, systemPolicyPath), "candidate policy", 0644)
		assertSystemFile(t, filepath.Join(system.Root, recoveryHelperPath), systemFixtureBinary("v1.0.0", "previous policy"), 0755)
		b, err := os.ReadFile(filepath.Join(u.Root, "pending-update.json"))
		must(t, err)
		var journal UpdateJournal
		must(t, json.Unmarshal(b, &journal))
		if journal.SystemBackup == "" {
			t.Fatal("system recovery not durable before startup")
		}
		return nil
	}
	u.Healthy = func(context.Context, string) bool { return true }
	oldMask := syscall.Umask(0077)
	defer syscall.Umask(oldMask)
	must(t, u.Apply(context.Background(), release))
	if starts != 1 || reloads != 2 {
		t.Fatalf("starts=%d reloads=%d", starts, reloads)
	}
	for _, unit := range []string{"dozor-recover.service", "dozor-rollback.service"} {
		b, err := os.ReadFile(filepath.Join(system.Root, "etc/systemd/system", unit))
		must(t, err)
		if !strings.Contains(string(b), "ExecStart=/usr/local/libexec/dozor-recover system recover\n") {
			t.Fatalf("%s still depends on candidate helper", unit)
		}
	}
	assertNoSystemJournal(t, u)
}

func TestUpdateRollsBackSystemFiles(t *testing.T) {
	for _, phase := range []string{"start", "health", "policy write"} {
		t.Run(phase, func(t *testing.T) {
			u, release := releaseFixture(t, "")
			system := setupSystemFixture(t, u)
			starts, stops := 0, 0
			policy := filepath.Join(system.Root, systemPolicyPath)
			u.Stop = func() error {
				stops++
				if phase == "policy write" {
					if stops == 1 {
						must(t, os.Remove(policy))
						must(t, os.Mkdir(policy, 0700))
					} else {
						// A partial install already replaced the helper, but its
						// independent recovery executable must remain usable.
						assertSystemFile(t, filepath.Join(system.Root, systemHelperPath), systemFixtureBinary("v1.1.0", "candidate policy"), 0755)
						must(t, os.Remove(policy))
					}
				}
				return nil
			}
			u.Start = func() error {
				starts++
				if phase == "start" && starts == 1 {
					return errors.New("candidate failed to start")
				}
				return nil
			}
			u.Healthy = func(context.Context, string) bool { return false }
			if err := u.Apply(context.Background(), release); err == nil {
				t.Fatal("failed update accepted")
			}
			assertOriginalSystem(t, u)
			assertNoSystemJournal(t, u)
		})
	}
}

func TestSystemBootRecoveryAtEachInstallBoundary(t *testing.T) {
	for phase := 0; phase < 4; phase++ {
		t.Run(fmt.Sprint(phase), func(t *testing.T) {
			u, release := releaseFixture(t, "")
			system := setupSystemFixture(t, u)
			staged, err := u.Stage(context.Background(), release)
			must(t, err)
			must(t, system.ensureRecovery())
			backup, err := system.snapshot(u.Root)
			must(t, err)
			previous := filepath.Join(u.Root, "releases/v1.0.0")
			must(t, WriteJSON(filepath.Join(u.Root, "pending-update.json"), UpdateJournal{Previous: previous, Candidate: staged, SystemBackup: backup}))
			if phase >= 1 {
				must(t, u.switchTo(staged))
			}
			if phase >= 2 {
				must(t, copySystemFile(filepath.Join(staged, "bin/dozor"), filepath.Join(system.Root, systemHelperPath), 0755))
			}
			if phase >= 3 {
				must(t, AtomicWrite(filepath.Join(system.Root, systemPolicyPath), []byte("candidate policy"), 0644))
			}
			// Neither the candidate nor the working helper can be relied on
			// during boot recovery; do not execute either one to restore state.
			must(t, AtomicWrite(filepath.Join(staged, "bin/dozor"), []byte("broken candidate"), 0755))
			recovered := &Updater{Root: u.Root, System: &SystemIntegration{Root: system.Root}}
			must(t, recovered.Recover())
			must(t, recovered.Recover())
			assertOriginalSystem(t, recovered)
			assertNoSystemJournal(t, recovered)
			assertSystemFile(t, filepath.Join(system.Root, recoveryHelperPath), systemFixtureBinary("v1.0.0", "previous policy"), 0755)
		})
	}
}

func TestLegacyUpdaterInstallationCatchesUpWithoutNewRelease(t *testing.T) {
	for _, offline := range []bool{false, true} {
		t.Run(fmt.Sprintf("offline=%t", offline), func(t *testing.T) {
			u, release := releaseFixture(t, "")
			system := setupSystemFixture(t, u)
			// The legacy updater only replaced the app; its separate helper
			// and polkit rule are still the original installed copies.
			u.System = nil
			must(t, u.Apply(context.Background(), release))
			u.System = system
			system.RecoveryBinary = filepath.Join(u.Root, "current/bin/dozor")
			manifest, err := json.Marshal(release)
			must(t, err)
			u.Client = &http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if offline {
					return nil, errors.New("offline")
				}
				return updateResponse(r, 200, string(manifest)), nil
			})}
			client := &http.Client{Transport: updateRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/health" {
					t.Fatalf("must not interrupt recording for helper catch-up: %s", r.URL.Path)
				}
				return updateResponse(r, 200, `{"version":"v1.1.0","ready":true}`), nil
			})}
			u.Stop = func() error { t.Fatal("catch-up stopped the application"); return nil }
			err = u.updateRunning(context.Background(), "https://updates.example/release.json", client, func(string) {})
			if (err != nil) != offline {
				t.Fatalf("unexpected update result: %v", err)
			}
			assertSystemFile(t, filepath.Join(system.Root, systemHelperPath), systemFixtureBinary("v1.1.0", "candidate policy"), 0755)
			assertSystemFile(t, filepath.Join(system.Root, systemPolicyPath), "candidate policy", 0644)
			assertNoSystemJournal(t, u)
			before, err := os.Stat(filepath.Join(system.Root, systemHelperPath))
			must(t, err)
			must(t, u.reconcileSystem(context.Background(), "v1.1.0"))
			after, err := os.Stat(filepath.Join(system.Root, systemHelperPath))
			must(t, err)
			if !os.SameFile(before, after) {
				t.Fatal("matching helper was needlessly rewritten")
			}
		})
	}
}

func TestSystemRecoveryRetainsCorruptBackupJournal(t *testing.T) {
	u, _ := releaseFixture(t, "")
	system := setupSystemFixture(t, u)
	backup, err := system.snapshot(u.Root)
	must(t, err)
	previous := filepath.Join(u.Root, "releases/v1.0.0")
	must(t, WriteJSON(filepath.Join(u.Root, "pending-update.json"), UpdateJournal{Previous: previous, Candidate: previous, SystemBackup: backup}))
	must(t, AtomicWrite(filepath.Join(u.Root, backup, "1"), []byte("corrupt policy backup"), 0600))
	if err := u.Recover(); err == nil {
		t.Fatal("corrupt rollback snapshot accepted")
	}
	assertOriginalSystem(t, u)
	if _, err := os.Stat(filepath.Join(u.Root, "pending-update.json")); err != nil {
		t.Fatalf("recovery evidence was removed: %v", err)
	}
}

func TestSystemUpdateRequiresLoadedRecoveryUnits(t *testing.T) {
	u, release := releaseFixture(t, "")
	system := setupSystemFixture(t, u)
	system.Reload = func() error { return errors.New("daemon-reload failed") }
	u.Stop = func() error { t.Fatal("update started without recovery units"); return nil }
	if err := u.Apply(context.Background(), release); err == nil {
		t.Fatal("failed recovery bootstrap accepted")
	}
	assertOriginalSystem(t, u)
	assertNoSystemJournal(t, u)
}

func TestSystemRecoveryRestoresAbsentFiles(t *testing.T) {
	u, release := releaseFixture(t, "")
	system := setupSystemFixture(t, u)
	for _, path := range managedSystemFiles {
		if err := os.Remove(filepath.Join(system.Root, path)); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
	}
	u.Healthy = func(context.Context, string) bool { return false }
	if err := u.Apply(context.Background(), release); err == nil {
		t.Fatal("failed update accepted")
	}
	for _, path := range managedSystemFiles {
		if _, err := os.Stat(filepath.Join(system.Root, path)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("new system file survived rollback: %s, %v", path, err)
		}
	}
	assertNoSystemJournal(t, u)
}

func TestSystemUpdateRejectsIncompatibleIntegration(t *testing.T) {
	for _, output := range []string{"v1.1.0", `{"protocol":2,"version":"v1.1.0","polkit":"rules"}`, `{"protocol":1,"version":"v0.0.1","polkit":"rules"}`} {
		t.Run(output, func(t *testing.T) {
			u, release := releaseFixture(t, "", map[string]string{"bin/dozor": "#!/bin/sh\ncat <<'OUT'\n" + output + "\nOUT\n"})
			setupSystemFixture(t, u)
			u.Stop = func() error { t.Fatal("incompatible integration stopped the app"); return nil }
			if err := u.Apply(context.Background(), release); err == nil {
				t.Fatal("incompatible integration accepted")
			}
			assertOriginalSystem(t, u)
			assertNoSystemJournal(t, u)
		})
	}
}

func TestEmbeddedSystemIntegration(t *testing.T) {
	var out bytes.Buffer
	must(t, PrintSystemIntegration(&out, "v1.2.3"))
	var manifest systemManifest
	must(t, json.Unmarshal(out.Bytes(), &manifest))
	rules, err := os.ReadFile("../../ops/50-dozor.rules")
	must(t, err)
	if manifest.Protocol != 1 || manifest.Version != "v1.2.3" || manifest.Polkit != string(rules) {
		t.Fatal("signed binary does not carry the release's actual polkit policy")
	}
}
