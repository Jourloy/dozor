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

type updateStatus struct {
	Message string    `json:"message"`
	Running bool      `json:"running"`
	At      time.Time `json:"at"`
}

func writeUpdateStatus(state, message string, running bool) error {
	b, err := json.Marshal(updateStatus{Message: message, Running: running, At: time.Now().UTC()})
	if err != nil {
		return err
	}
	// The updater runs as root, but the application must be able to read progress.
	return AtomicWrite(filepath.Join(state, "update-status.json"), b, 0644)
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
	if status.Running && time.Since(status.At) > 26*time.Minute {
		status.Running = false
		status.Message = "Проверка обновлений прервана; повторите попытку"
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
		_ = writeUpdateStatus(a.StateDir, message, false)
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
