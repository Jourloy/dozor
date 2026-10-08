package dozor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"dozor/ops"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const systemHelperPath = "usr/local/libexec/dozor-system"
const recoveryHelperPath = "usr/local/libexec/dozor-recover"
const systemPolicyPath = "etc/polkit-1/rules.d/50-dozor.rules"

// SystemIntegration manages a fixed set of root-owned files. Recovery stays
// independent of both current and the working helper throughout a transaction.
type SystemIntegration struct {
	Root           string
	RecoveryBinary string
	Reload         func() error
}

type systemManifest struct {
	Protocol        int    `json:"protocol"`
	UpdaterProtocol int    `json:"updater_protocol,omitempty"`
	Version         string `json:"version"`
	Polkit          string `json:"polkit"`
}

func PrintSystemIntegration(w io.Writer, version string) error {
	rules, err := ops.Files.ReadFile("50-dozor.rules")
	if err != nil {
		return err
	}
	return json.NewEncoder(w).Encode(systemManifest{Protocol: 1, UpdaterProtocol: 1, Version: version, Polkit: string(rules)})
}

func releaseSystemIntegration(ctx context.Context, release, version string) (systemManifest, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	b, err := exec.CommandContext(ctx, filepath.Join(release, "bin/dozor"), "system", "integration").Output()
	if err != nil {
		return systemManifest{}, fmt.Errorf("не удалось получить системные правила выпуска: %w", err)
	}
	var m systemManifest
	if len(b) > 64<<10 || json.Unmarshal(b, &m) != nil || m.Protocol != 1 || m.Version != version || m.Polkit == "" {
		return m, errors.New("несовместимые системные правила выпуска")
	}
	return m, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("system file is not a regular file")
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func sameSystemFile(source, target string, mode os.FileMode) bool {
	info, err := os.Lstat(target)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != mode {
		return false
	}
	a, err := fileSHA256(source)
	if err != nil {
		return false
	}
	b, err := fileSHA256(target)
	return err == nil && a == b
}

// Never truncate a running executable. The previous inode remains usable until
// the replacement and its parent directory have both been synced.
func copySystemFile(source, target string, mode os.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return errors.New("system file is not a regular file")
	}
	if err = os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return err
	}
	out, err := os.CreateTemp(filepath.Dir(target), ".system-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Chmod(mode)
	}
	if err == nil {
		err = out.Sync()
	}
	closeErr := out.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(out.Name(), target); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(target))
}

func (s *SystemIntegration) ensureRecovery() error {
	// Use the installed release, never the untried candidate. In particular,
	// resolve current after recovery: this process may have started before rollback.
	backup := filepath.Join(s.Root, recoveryHelperPath)
	if !sameSystemFile(s.RecoveryBinary, backup, 0755) {
		if err := copySystemFile(s.RecoveryBinary, backup, 0755); err != nil {
			return err
		}
	}
	for _, name := range []string{"dozor-recover.service", "dozor-rollback.service"} {
		b, err := ops.Files.ReadFile(name)
		if err != nil {
			return err
		}
		path := filepath.Join(s.Root, "etc/systemd/system", name)
		old, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if !bytes.Equal(old, b) {
			if err = AtomicWrite(path, b, 0644); err != nil {
				return err
			}
		}
	}
	// Also retry daemon-reload after an interrupted bootstrap whose files were
	// already written. Do not start a transaction with stale recovery units.
	if s.Reload != nil {
		return s.Reload()
	}
	return nil
}

func (s *SystemIntegration) matches(release string, m systemManifest) bool {
	if !sameSystemFile(filepath.Join(release, "bin/dozor"), filepath.Join(s.Root, systemHelperPath), 0755) {
		return false
	}
	path := filepath.Join(s.Root, systemPolicyPath)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0644 {
		return false
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != m.Polkit {
		return false
	}
	unit, err := os.ReadFile(filepath.Join(s.Root, "etc/systemd/system/dozor-update.service"))
	return err == nil && bytes.Equal(unit, updaterUnit(m))
}

func (s *SystemIntegration) install(release string, m systemManifest) error {
	if err := copySystemFile(filepath.Join(release, "bin/dozor"), filepath.Join(s.Root, systemHelperPath), 0755); err != nil {
		return err
	}
	if err := AtomicWrite(filepath.Join(s.Root, systemPolicyPath), []byte(m.Polkit), 0644); err != nil {
		return err
	}
	if err := AtomicWrite(filepath.Join(s.Root, "etc/systemd/system/dozor-update.service"), updaterUnit(m), 0644); err != nil {
		return err
	}
	if s.Reload != nil {
		return s.Reload()
	}
	return nil
}

func updaterUnit(m systemManifest) []byte {
	unit, _ := ops.Files.ReadFile("dozor-update.service")
	if m.UpdaterProtocol == 0 {
		unit = bytes.ReplaceAll(unit, []byte("/usr/local/libexec/dozor-system system update"), []byte("/opt/dozor/current/bin/dozor system update"))
	}
	return unit
}

var managedSystemFiles = [...]string{systemHelperPath, systemPolicyPath, "etc/systemd/system/dozor-update.service"}

type systemFileBackup struct {
	Present bool   `json:"present"`
	Mode    uint32 `json:"mode"`
	SHA256  string `json:"sha256"`
}

type systemBackup struct {
	Protocol int                `json:"protocol"`
	Files    []systemFileBackup `json:"files"`
}

func systemBackupPath(root, name string) (string, error) {
	const prefix = ".system-backup-"
	if len(name) != len(prefix)+32 || name[:len(prefix)] != prefix || !safeID.MatchString(name[len(prefix):]) {
		return "", errors.New("invalid system backup path")
	}
	return filepath.Join(root, name), nil
}

func (s *SystemIntegration) snapshot(root string) (string, error) {
	name := ".system-backup-" + ID()
	dir := filepath.Join(root, name)
	if err := os.Mkdir(dir, 0700); err != nil {
		return "", err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(dir)
		}
	}()
	backup := systemBackup{Protocol: 2}
	for i, path := range managedSystemFiles {
		file := systemFileBackup{}
		source := filepath.Join(s.Root, path)
		info, err := os.Lstat(source)
		if err == nil {
			if !info.Mode().IsRegular() {
				return "", errors.New("cannot back up non-regular system file")
			}
			file.Present, file.Mode = true, uint32(info.Mode().Perm())
			target := filepath.Join(dir, fmt.Sprint(i))
			if err = copySystemFile(source, target, 0600); err != nil {
				return "", err
			}
			file.SHA256, err = fileSHA256(target)
			if err != nil {
				return "", err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		backup.Files = append(backup.Files, file)
	}
	if err := WriteJSON(filepath.Join(dir, "backup.json"), backup); err != nil {
		return "", err
	}
	if err := syncDirectory(root); err != nil {
		return "", err
	}
	complete = true
	return name, nil
}

func (s *SystemIntegration) restore(root, name string) error {
	dir, err := systemBackupPath(root, name)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(filepath.Join(dir, "backup.json"))
	if err != nil {
		return err
	}
	var backup systemBackup
	if json.Unmarshal(b, &backup) != nil || !((backup.Protocol == 1 && len(backup.Files) == 2) || (backup.Protocol == 2 && len(backup.Files) == len(managedSystemFiles))) {
		return errors.New("invalid system backup")
	}
	// Validate the entire snapshot before replacing any live system file.
	for i, file := range backup.Files {
		if file.Mode > 0777 {
			return errors.New("invalid system backup permissions")
		}
		if file.Present {
			hash, err := fileSHA256(filepath.Join(dir, fmt.Sprint(i)))
			if err != nil || hash != file.SHA256 {
				return errors.New("corrupt system backup")
			}
		}
	}
	for i, file := range backup.Files {
		target := filepath.Join(s.Root, managedSystemFiles[i])
		if file.Present {
			if err = copySystemFile(filepath.Join(dir, fmt.Sprint(i)), target, os.FileMode(file.Mode)); err != nil {
				return err
			}
		} else if err = os.Remove(target); err == nil {
			if err = syncDirectory(filepath.Dir(target)); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if s.Reload != nil {
		return s.Reload()
	}
	return nil
}

func (u *Updater) finishUpdate(j UpdateJournal) error {
	if err := u.clearJournal(); err != nil {
		return err
	}
	if j.SystemBackup != "" {
		if path, err := systemBackupPath(u.Root, j.SystemBackup); err == nil {
			_ = os.RemoveAll(path)
		}
	}
	return nil
}

// Older updaters only switch current. The first run of the new updater also
// repairs that installation when no newer release exists or the network is down.
func (u *Updater) reconcileSystem(ctx context.Context, version string) error {
	if u.System == nil {
		return nil
	}
	current, err := filepath.EvalSymlinks(filepath.Join(u.Root, "current"))
	if err != nil {
		return err
	}
	m, err := releaseSystemIntegration(ctx, current, version)
	if err != nil {
		return err
	}
	if err = u.System.ensureRecovery(); err != nil {
		return err
	}
	if u.System.matches(current, m) {
		return AtomicWrite(filepath.Join(current, ".confirmed-version"), []byte(version), 0644)
	}
	backup, err := u.System.snapshot(u.Root)
	if err != nil {
		return err
	}
	j := UpdateJournal{Previous: current, Candidate: current, SystemBackup: backup}
	if err = WriteJSON(filepath.Join(u.Root, "pending-update.json"), j); err != nil {
		return err
	}
	if err = u.System.install(current, m); err != nil {
		if recoveryErr := u.Recover(); recoveryErr != nil {
			return fmt.Errorf("system rollback failed: %w", recoveryErr)
		}
		return err
	}
	if err = u.finishUpdate(j); err != nil {
		return err
	}
	return AtomicWrite(filepath.Join(current, ".confirmed-version"), []byte(version), 0644)
}
