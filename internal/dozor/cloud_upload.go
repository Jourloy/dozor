package dozor

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// This switch is independent of S3 credentials and of the recording runtime.
// changed_at also orders acknowledgements and status polls on the backend.
type CloudUploadState struct {
	Enabled   bool  `json:"enabled"`
	ChangedAt int64 `json:"changed_at"`
}

type CloudUploadControl struct {
	writes sync.Mutex
	mu     sync.Mutex
	path   string
	state  CloudUploadState
	cancel context.CancelFunc
	done   chan struct{}
}

func loadCloudUpload(stateDir string) (*CloudUploadControl, error) {
	f := &CloudUploadControl{path: filepath.Join(stateDir, "cloud-upload.json"), state: CloudUploadState{Enabled: true}}
	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		err = WriteJSON(f.path, f.state)
	} else if err == nil {
		var saved struct {
			Enabled   *bool  `json:"enabled"`
			ChangedAt *int64 `json:"changed_at"`
		}
		err = json.Unmarshal(data, &saved)
		if err == nil {
			if saved.Enabled == nil || saved.ChangedAt == nil || *saved.ChangedAt < 0 {
				return nil, errors.New("invalid cloud upload state")
			}
			f.state = CloudUploadState{*saved.Enabled, *saved.ChangedAt}
		}
	}
	return f, err
}

func (f *CloudUploadControl) Get() CloudUploadState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state
}

func (f *CloudUploadControl) Save(ctx context.Context, enabled bool) (CloudUploadState, error) {
	f.writes.Lock()
	defer f.writes.Unlock()
	f.mu.Lock()
	if f.state.Enabled != enabled {
		next := CloudUploadState{enabled, max(time.Now().UnixMilli(), f.state.ChangedAt+1)}
		if err := WriteJSON(f.path, next); err != nil {
			f.mu.Unlock()
			return CloudUploadState{}, err
		}
		f.state = next
	}
	state := f.state
	var done chan struct{}
	if !enabled && f.cancel != nil {
		f.cancel()
		done = f.done
	}
	f.mu.Unlock()
	// A successful pause acknowledges that the in-flight transfer has stopped.
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return state, ctx.Err()
		}
	}
	return state, nil
}

func (f *CloudUploadControl) uploadOne(ctx context.Context, s *Store, cfg S3Config, remote ObjectStore) (bool, error) {
	f.mu.Lock()
	if !f.state.Enabled || f.cancel != nil {
		f.mu.Unlock()
		return false, nil
	}
	child, cancel := context.WithCancel(ctx)
	f.cancel, f.done = cancel, make(chan struct{})
	f.mu.Unlock()
	defer func() {
		cancel()
		f.mu.Lock()
		close(f.done)
		f.cancel, f.done = nil, nil
		f.mu.Unlock()
	}()
	return UploadOne(child, s, cfg, remote)
}

func (a *App) cloudUploadView(state CloudUploadState) map[string]any {
	return map[string]any{"enabled": state.Enabled, "changed_at": state.ChangedAt, "configured": a.Config.Get().S3.Enabled}
}

func (a *App) cloudUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		jsonOut(w, 200, a.cloudUploadView(a.uploads.Get()))
		return
	}
	var input struct {
		Enabled *bool `json:"enabled"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Enabled == nil {
		apiError(w, 400, "укажите enabled: true или false")
		return
	}
	state, err := a.uploads.Save(r.Context(), *input.Enabled)
	if err != nil {
		log.Printf("save cloud upload: %v", err)
		apiError(w, 503, "не удалось подтвердить состояние выгрузки")
		return
	}
	jsonOut(w, 200, a.cloudUploadView(state))
}
