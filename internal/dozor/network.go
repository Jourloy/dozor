package dozor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"
)

const networkWindow = time.Minute
const networkRebootCooldown = 10 * time.Minute

// Kept outside the archive, so rebooting or losing the disk cannot lose the
// reason. The backend imports these facts idempotently after connectivity returns.
type NetworkReboot struct {
	At          int64  `json:"at"`
	RequestedAt int64  `json:"requested_at"`
	Affected    int    `json:"affected"`
	Total       int    `json:"total"`
	Error       string `json:"error,omitempty"`
}

type networkState struct {
	LastAttempt int64           `json:"last_attempt"`
	Reports     []NetworkReboot `json:"reports"`
}

type NetworkWatchdog struct {
	mu       sync.Mutex
	path     string
	state    networkState
	pending  *NetworkReboot
	inFlight bool
}

func loadNetworkWatchdog(stateDir string) (*NetworkWatchdog, error) {
	w := &NetworkWatchdog{path: filepath.Join(stateDir, "network-reboots.json")}
	b, err := os.ReadFile(w.path)
	if errors.Is(err, os.ErrNotExist) {
		return w, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &w.state)
	}
	return w, err
}

func (w *NetworkWatchdog) Reports() []NetworkReboot {
	w.mu.Lock()
	defer w.mu.Unlock()
	reports := w.state.Reports
	if w.inFlight {
		reports = reports[:len(reports)-1]
	}
	return append([]NetworkReboot{}, reports...)
}

func (w *NetworkWatchdog) queue(report NetworkReboot) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.pending != nil || (w.state.LastAttempt != 0 && report.At-w.state.LastAttempt < networkRebootCooldown.Milliseconds()) {
		return false
	}
	w.pending = &report
	return true
}

// Only the live observation loop uses this wrapper. Initial offline states,
// duplicate offline hooks, shutdown and configuration edits are not link losses.
func (r *Runtime) observeStream(ctx context.Context, camera string, online bool, at int64) error {
	previous := r.streams[camera]
	if err := r.setStream(ctx, camera, online, at); err != nil {
		return err
	}
	if r.network == nil || previous.State != "online" || r.streams[camera].State != "offline" {
		return nil
	}
	if r.breaks == nil || at < r.lastBreak {
		r.breaks = make(map[string][]int64)
	}
	r.lastBreak = at
	r.breaks[camera] = append(r.breaks[camera], at)
	total, affected := 0, 0
	for id, state := range r.streams {
		if state.State == "disabled" {
			delete(r.breaks, id)
			continue
		}
		total++
		breaks := r.breaks[id]
		for len(breaks) > 0 && breaks[0] <= at-networkWindow.Milliseconds() {
			breaks = breaks[1:]
		}
		// Three newest losses suffice to prove the threshold; bound memory even
		// for a camera rapidly alternating its hooks.
		if len(breaks) >= 3 {
			affected++
			breaks = breaks[len(breaks)-3:]
		}
		r.breaks[id] = breaks
	}
	if total > 0 && affected*2 >= total && r.network.queue(NetworkReboot{At: at, Affected: affected, Total: total}) {
		r.Store.Notice(fmt.Sprintf("Нестабильная сеть: не менее 3 разрывов за минуту у %d из %d камер. Запланирована перезагрузка регистратора.", affected, total))
	}
	return nil
}

func (w *NetworkWatchdog) tick(now time.Time, reboot func() error) error {
	w.mu.Lock()
	if w.pending == nil {
		w.mu.Unlock()
		return nil
	}
	if w.state.LastAttempt != 0 && now.UnixMilli()-w.state.LastAttempt < networkRebootCooldown.Milliseconds() {
		w.pending = nil
		w.mu.Unlock()
		return nil
	}
	report := *w.pending
	report.RequestedAt = now.UnixMilli()
	next := networkState{LastAttempt: report.RequestedAt}
	for _, previous := range w.state.Reports {
		if previous.RequestedAt > now.Add(-7*24*time.Hour).UnixMilli() {
			next.Reports = append(next.Reports, previous)
		}
	}
	next.Reports = append(next.Reports, report)
	// Save the cooldown before invoking the OS: crashes, command errors and a
	// reboot must never turn one unstable minute into a reboot loop.
	if err := WriteJSON(w.path, next); err != nil {
		w.mu.Unlock()
		return err
	}
	w.state, w.pending, w.inFlight = next, nil, true
	w.mu.Unlock()
	err := reboot()
	w.mu.Lock()
	defer w.mu.Unlock()
	w.inFlight = false
	if err != nil {
		w.state.Reports[len(w.state.Reports)-1].Error = err.Error()
		return errors.Join(err, WriteJSON(w.path, w.state))
	}
	return nil
}

func (a *App) runNetworkWatchdog(ctx context.Context) {
	if a.Development || runtime.GOOS != "linux" {
		return
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if err := a.network.tick(now, func() error { return a.reboot(ctx) }); err != nil {
				log.Printf("Network instability reboot: %v", err)
			}
		}
	}
}
