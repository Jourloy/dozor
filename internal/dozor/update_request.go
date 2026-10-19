package dozor

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Final outcomes of the last finished update attempt, exposed as update_outcome.
// An empty outcome means that an attempt is running or that nothing is known.
const (
	updateOutcomeOK       = "ok"       // a new version was installed
	updateOutcomeNoop     = "noop"     // already up to date, or nothing to install
	updateOutcomeDisabled = "disabled" // automatic updates are off
	updateOutcomeDeferred = "deferred" // downloaded, waiting for an idle window
	updateOutcomeFailed   = "failed"   // any error, rollback, failed start or interruption
)

// The systemd unit has a 25-minute limit; after that a running record is orphaned.
const staleUpdateAfter = 26 * time.Minute

type updateStatus struct {
	Message string    `json:"message"`
	Running bool      `json:"running"`
	At      time.Time `json:"at"`
	// Outcome and Target are absent in files written by older releases.
	Outcome string `json:"outcome,omitempty"`
	Target  string `json:"target,omitempty"`
}

// atMillis is the time the outcome was written, or 0 when no outcome is known.
// For files without an outcome the timestamp is only progress, not a result.
func (s updateStatus) atMillis() int64 {
	if s.Outcome == "" || s.At.IsZero() {
		return 0
	}
	return s.At.UnixMilli()
}

func writeUpdateRecord(state string, status updateStatus) error {
	status.At = time.Now().UTC()
	b, err := json.Marshal(status)
	if err != nil {
		return err
	}
	// The updater runs as root, but the application must be able to read progress.
	return AtomicWrite(filepath.Join(state, "update-status.json"), b, 0644)
}

// writeUpdateStatus records progress of a running attempt: it has no outcome yet.
func writeUpdateStatus(state, message string, running bool) error {
	return writeUpdateRecord(state, updateStatus{Message: message, Running: running})
}

// writeUpdateOutcome records the final state of an attempt. target is the
// version the attempt tried to install, or "" when unknown.
func writeUpdateOutcome(state, message, outcome, target string) error {
	return writeUpdateRecord(state, updateStatus{Message: message, Outcome: outcome, Target: target})
}

// recordUpdateProgress stores an intermediate state of a running attempt. It
// carries the target version once known, so an interrupted attempt keeps it.
func recordUpdateProgress(state, message string, u *Updater) error {
	return writeUpdateRecord(state, updateStatus{Message: message, Running: true, Target: u.Target})
}

// recordUpdateResult stores the final state of an attempt. An error always
// makes it "failed" and its text becomes the message; otherwise the outcome
// chosen by the updater is used, "" if none was set.
func recordUpdateResult(state, message string, u *Updater, err error) error {
	outcome := u.Outcome
	if err != nil {
		message, outcome = err.Error(), updateOutcomeFailed
	}
	return writeUpdateOutcome(state, message, outcome, u.Target)
}

// updateRecorder is the status bookkeeping of one SystemUpdate run: the
// progress callback given to the updater and the single final record. The
// last message is kept here, so the final record carries it unless the run
// failed. SystemUpdate and its tests share this code.
type updateRecorder struct {
	state   string
	u       *Updater
	message string
	// write stores the final record. It is a field only to let tests fail it.
	write func(state, message string, u *Updater, err error) error
}

func newUpdateRecorder(state string, u *Updater) *updateRecorder {
	return &updateRecorder{state: state, u: u, message: "Проверяем наличие обновлений", write: recordUpdateResult}
}

// status is the progress callback of the updater.
func (r *updateRecorder) status(message string) {
	r.message = message
	_ = recordUpdateProgress(r.state, message, r.u)
}

// finish writes the final record of the run; err is the error SystemUpdate
// returns. A failed write is repeated once. If the second write fails too, the
// last "running" record stays on disk: readUpdateStatus then reports an
// interrupted, failed attempt after 26 minutes, although the run has finished.
func (r *updateRecorder) finish(err error) error {
	if e := r.write(r.state, r.message, r.u, err); e == nil {
		return nil
	}
	return r.write(r.state, r.message, r.u, err)
}

func readUpdateStatus(state string) updateStatus {
	b, err := os.ReadFile(filepath.Join(state, "update-status.json"))
	if err != nil {
		return updateStatus{}
	}
	var status updateStatus
	if json.Unmarshal(b, &status) != nil {
		// Releases before manual updates wrote a plain-text message.
		return updateStatus{Message: string(b)}
	}
	// The systemd unit has a 25-minute limit. A killed updater cannot leave the
	// button disabled forever, even if it never got to write a final result.
	if status.Running && time.Since(status.At) > staleUpdateAfter {
		status.Running = false
		status.Message = "Проверка обновлений прервана; повторите попытку"
		status.Outcome = updateOutcomeFailed
		// The result is derived from the stored record, not from the clock of
		// this read, so it is the same on every read and the backend can dedupe.
		if !status.At.IsZero() {
			status.At = status.At.Add(staleUpdateAfter)
		}
	}
	return status
}

func (a *App) requestUpdate(w http.ResponseWriter, r *http.Request) {
	// Persist the request before starting the independent updater. Older helpers
	// consume it through the private API; newer helpers read the marker directly.
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	state, err := exec.CommandContext(ctx, "systemctl", "show", "--property=ActiveState", "--value", "dozor-update.service").Output()
	if err != nil {
		apiError(w, 503, "служба обновления недоступна")
		return
	}
	switch strings.TrimSpace(string(state)) {
	case "inactive", "failed":
	case "active", "activating", "deactivating", "reloading", "refreshing":
		apiError(w, 409, "проверка или установка обновления уже выполняется")
		return
	default:
		apiError(w, 503, "служба обновления недоступна")
		return
	}
	request := filepath.Join(a.StateDir, "update-request")
	if err = AtomicWrite(request, []byte("manual\n"), 0600); err != nil {
		apiError(w, 500, "не удалось сохранить запрос обновления")
		return
	}
	if err = writeUpdateStatus(a.StateDir, "Проверяем наличие обновлений", true); err != nil {
		_ = os.Remove(request)
		apiError(w, 500, "не удалось сохранить статус обновления")
		return
	}
	// Do not wait for the oneshot unit: it will stop this HTTP process to install.
	if err = exec.CommandContext(ctx, "systemctl", "--no-block", "start", "dozor-update.service").Run(); err != nil {
		_ = os.Remove(request)
		message := "Не удалось запустить обновление. Проверьте системную службу и права доступа."
		_ = writeUpdateOutcome(a.StateDir, message, updateOutcomeFailed, "")
		apiError(w, 503, message)
		return
	}
	jsonOut(w, http.StatusAccepted, map[string]any{"update_status": "Проверяем наличие обновлений", "update_running": true})
}

func (a *App) consumeUpdateRequest(w http.ResponseWriter, r *http.Request) {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	err := os.Remove(filepath.Join(a.StateDir, "update-request"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		apiError(w, 500, "не удалось прочитать запрос обновления")
		return
	}
	jsonOut(w, 200, map[string]bool{"immediate": err == nil})
}

func readUpdateRequest(ctx context.Context, client *http.Client) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", "http://unix/update-request", nil)
	if err != nil {
		return false, err
	}
	res, err := client.Do(req)
	if err != nil {
		return false, errors.New("Dozor недоступна; обновление отложено")
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return false, nil // An older running application only supports the timer.
	}
	var request struct {
		Immediate bool `json:"immediate"`
	}
	if res.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(res.Body, 4096)).Decode(&request) != nil {
		return false, errors.New("не удалось прочитать запрос обновления")
	}
	return request.Immediate, nil
}
