package dozor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
	_ "time/tzdata"
)

const rebootTimezone = "Europe/Moscow"

type RebootSchedule struct {
	Enabled      bool   `json:"enabled"`
	IntervalDays int    `json:"interval_days"`
	Time         string `json:"time"`
}

type RebootState struct {
	RebootSchedule
	Timezone     string `json:"timezone"`
	NextRebootAt *int64 `json:"next_reboot_at"`
	LastError    string `json:"last_error"`
}

type RebootScheduler struct {
	mu    sync.Mutex
	path  string
	state RebootState
}

func (s RebootSchedule) validate() error {
	if s.IntervalDays < 1 || s.IntervalDays > 365 {
		return errors.New("укажите интервал от 1 до 365 дней")
	}
	if parsed, err := time.Parse("15:04", s.Time); err != nil || parsed.Format("15:04") != s.Time {
		return errors.New("укажите время в формате ЧЧ:ММ")
	}
	return nil
}

func (s RebootSchedule) firstAfter(now time.Time) int64 {
	zone, _ := time.LoadLocation(rebootTimezone) // Bundled tzdata, independent of the host timezone.
	clock, _ := time.Parse("15:04", s.Time)
	local := now.In(zone)
	next := time.Date(local.Year(), local.Month(), local.Day(), clock.Hour(), clock.Minute(), 0, 0, zone)
	if !next.After(now) {
		next = next.AddDate(0, 0, 1)
	}
	return next.UnixMilli()
}

// Preserve the original interval after downtime instead of restarting its cadence.
func (s RebootSchedule) advance(next int64, now time.Time) int64 {
	zone, _ := time.LoadLocation(rebootTimezone)
	date := time.UnixMilli(next).In(zone)
	for !date.After(now) {
		date = date.AddDate(0, 0, s.IntervalDays)
	}
	return date.UnixMilli()
}

func loadRebootScheduler(stateDir string, now time.Time) (*RebootScheduler, error) {
	s := &RebootScheduler{path: filepath.Join(stateDir, "reboot-schedule.json")}
	s.state = RebootState{RebootSchedule: RebootSchedule{Enabled: true, IntervalDays: 1, Time: "03:15"}, Timezone: rebootTimezone}
	b, err := os.ReadFile(s.path)
	if err == nil {
		err = json.Unmarshal(b, &s.state)
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if err != nil {
		return nil, err
	}
	if err = s.state.RebootSchedule.validate(); err != nil {
		return nil, err
	}
	s.state.Timezone = rebootTimezone
	if !s.state.Enabled {
		s.state.NextRebootAt = nil
	} else if s.state.NextRebootAt == nil {
		next := s.state.firstAfter(now)
		s.state.NextRebootAt = &next
	} else {
		// A reboot or service restart must never cause an immediate catch-up reboot.
		next := s.state.advance(*s.state.NextRebootAt, now)
		s.state.NextRebootAt = &next
	}
	return s, WriteJSON(s.path, s.state)
}

func (s *RebootScheduler) Get() RebootState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *RebootScheduler) Save(schedule RebootSchedule, now time.Time) (RebootState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := schedule.validate(); err != nil {
		return RebootState{}, err
	}
	// An identical PUT is idempotent, including after a lost HTTP response.
	if schedule == s.state.RebootSchedule {
		return s.state, nil
	}
	next := RebootState{RebootSchedule: schedule, Timezone: rebootTimezone}
	if schedule.Enabled {
		at := schedule.firstAfter(now)
		next.NextRebootAt = &at
	}
	if err := WriteJSON(s.path, next); err != nil {
		return RebootState{}, err
	}
	s.state = next
	return next, nil
}

func (s *RebootScheduler) tick(now time.Time, reboot func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.state.Enabled || s.state.NextRebootAt == nil || now.UnixMilli() < *s.state.NextRebootAt {
		return nil
	}
	next := s.state
	at := next.advance(*next.NextRebootAt, now)
	next.NextRebootAt = &at
	next.LastError = ""
	// Commit the next occurrence BEFORE asking the OS to reboot: neither a crash
	// nor a rejected command can create a reboot loop within the current slot.
	if err := WriteJSON(s.path, next); err != nil {
		return err
	}
	missed := now.Sub(time.UnixMilli(*s.state.NextRebootAt)) >= time.Minute
	s.state = next
	if missed {
		return nil
	}
	if err := reboot(); err != nil {
		s.state.LastError = err.Error()
		return errors.Join(err, WriteJSON(s.path, s.state))
	}
	return nil
}

func (a *App) runRebootScheduler(ctx context.Context) {
	if a.Development || runtime.GOOS != "linux" {
		return
	}
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.reboots.tick(time.Now(), func() error { return a.reboot(ctx) }); err != nil {
				log.Printf("Scheduled reboot: %v", err)
			}
		}
	}
}

func (a *App) reboot(ctx context.Context) error {
	a.updateMu.Lock()
	defer a.updateMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	state, err := exec.CommandContext(ctx, "systemctl", "show", "--property=ActiveState", "--value", "dozor-update.service").Output()
	if err != nil {
		return errors.New("Не удалось проверить службу обновления. Перезапуск пропущен.")
	}
	if active := strings.TrimSpace(string(state)); (active != "inactive" && active != "failed") || readUpdateStatus(a.StateDir).Running {
		return errors.New("Перезапуск пропущен: выполняется обновление Dozor.")
	}
	// logind performs an orderly reboot and respects shutdown inhibitors. The
	// polkit rule grants only reboot, never poweroff or inhibitor bypass.
	if err = exec.CommandContext(ctx, "systemctl", "--no-ask-password", "--no-block", "reboot").Run(); err != nil {
		return errors.New("Не удалось перезапустить Raspberry Pi. Проверьте журнал системной службы.")
	}
	return nil
}

func (a *App) rebootSchedule(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		jsonOut(w, 200, a.reboots.Get())
		return
	}
	var input struct {
		Enabled      *bool  `json:"enabled"`
		IntervalDays int    `json:"interval_days"`
		Time         string `json:"time"`
	}
	if !decode(w, r, &input) {
		return
	}
	if input.Enabled == nil {
		apiError(w, 400, "укажите, включено ли расписание")
		return
	}
	schedule := RebootSchedule{Enabled: *input.Enabled, IntervalDays: input.IntervalDays, Time: input.Time}
	if err := schedule.validate(); err != nil {
		apiError(w, 400, err.Error())
		return
	}
	state, err := a.reboots.Save(schedule, time.Now())
	if err != nil {
		log.Print(fmt.Errorf("save reboot schedule: %w", err))
		apiError(w, 500, "не удалось сохранить расписание перезапуска")
		return
	}
	jsonOut(w, 200, state)
}
