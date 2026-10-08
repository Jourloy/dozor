package dozor

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

type StoragePolicy struct {
	MaxDiskUsagePercent int `json:"max_disk_usage_percent"`
}

func (p StoragePolicy) validate() error {
	if p.MaxDiskUsagePercent < 1 || p.MaxDiskUsagePercent > 99 {
		return errors.New("укажите целое число от 1 до 99")
	}
	return nil
}

type StoragePolicyFile struct {
	mu     sync.RWMutex
	path   string
	policy StoragePolicy
}

func loadStoragePolicy(stateDir string) (*StoragePolicyFile, error) {
	f := &StoragePolicyFile{path: filepath.Join(stateDir, "storage-policy.json"), policy: StoragePolicy{MaxDiskUsagePercent: 80}}
	data, err := os.ReadFile(f.path)
	if errors.Is(err, os.ErrNotExist) {
		err = WriteJSON(f.path, f.policy)
	} else if err == nil {
		// An existing invalid policy must not silently become a destructive default.
		f.policy = StoragePolicy{}
		err = json.Unmarshal(data, &f.policy)
	}
	if err != nil {
		return nil, err
	}
	if err = f.policy.validate(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *StoragePolicyFile) Get() StoragePolicy {
	f.mu.RLock()
	defer f.mu.RUnlock()
	return f.policy
}

func (f *StoragePolicyFile) Save(policy StoragePolicy) error {
	if err := policy.validate(); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := WriteJSON(f.path, policy); err != nil {
		return err
	}
	f.policy = policy
	return nil
}

func (a *App) storagePolicy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		jsonOut(w, 200, a.storage.Get())
		return
	}
	var policy StoragePolicy
	if !decode(w, r, &policy) {
		return
	}
	if err := policy.validate(); err != nil {
		apiError(w, 400, err.Error())
		return
	}
	if err := a.storage.Save(policy); err != nil {
		log.Printf("save storage policy: %v", err)
		apiError(w, 500, "не удалось сохранить лимит диска")
		return
	}
	// The next cleanup tick reads this policy; camera processes keep running.
	jsonOut(w, 200, policy)
}
