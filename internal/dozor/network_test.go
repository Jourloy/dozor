package dozor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func networkRuntime(t *testing.T, enabled, disabled int) (*Runtime, []Camera) {
	t.Helper()
	s := testStore(t)
	w, err := loadNetworkWatchdog(t.TempDir())
	must(t, err)
	r := &Runtime{Store: s, Engine: NewEngine(s, Binaries{}), network: w}
	var cameras []Camera
	for i := 0; i < enabled+disabled; i++ {
		cameras = append(cameras, Camera{ID: ID(), Enabled: i < enabled})
	}
	must(t, r.initStreams(cameras, time.UnixMilli(1_000_000)))
	return r, cameras
}

func loseStream(t *testing.T, r *Runtime, camera string, at int64) {
	t.Helper()
	must(t, r.observeStream(context.Background(), camera, true, at-1))
	must(t, r.observeStream(context.Background(), camera, false, at))
}

func TestNetworkCountsActualLossesAndAtLeastHalfEnabledCameras(t *testing.T) {
	for _, count := range []int{1, 2, 3, 4, 5} {
		t.Run(string(rune('0'+count)), func(t *testing.T) {
			r, cameras := networkRuntime(t, count, 4)
			// Initial unavailability and repeated connection failures never count.
			for range 5 {
				must(t, r.observeStream(context.Background(), cameras[0].ID, false, 1_000_001))
			}
			for loss := 0; loss < 2; loss++ {
				for _, camera := range cameras {
					loseStream(t, r, camera.ID, 1_001_000+int64(loss)*10_000)
				}
			}
			if r.network.pending != nil {
				t.Fatal("two losses, disabled cameras or failed retries triggered a reboot")
			}
			threshold := (count + 1) / 2
			for i := 0; i < threshold; i++ {
				loseStream(t, r, cameras[i].ID, 1_021_000+int64(i))
				if i < threshold-1 && r.network.pending != nil {
					t.Fatal("less than half triggered a reboot")
				}
			}
			pending := r.network.pending
			if pending == nil || pending.Affected != threshold || pending.Total != count {
				t.Fatalf("missing exact threshold report: %+v", pending)
			}
		})
	}
}

func TestNetworkWindowExpiresAndManualChangesDoNotCount(t *testing.T) {
	r, cameras := networkRuntime(t, 2, 0)
	camera := cameras[0].ID
	for _, at := range []int64{1_001_000, 1_031_000, 1_061_000} {
		loseStream(t, r, camera, at)
	}
	if r.network.pending != nil {
		t.Fatal("a loss exactly one minute old counted")
	}
	// Configuration changes and shutdown use setStream, bypassing the watchdog.
	for at := int64(1_062_000); at < 1_062_003; at++ {
		must(t, r.setStream(context.Background(), camera, true, at))
		must(t, r.setStream(context.Background(), camera, false, at))
	}
	if r.network.pending != nil {
		t.Fatal("manual changes counted")
	}
	loseStream(t, r, camera, 1_063_000)
	if r.network.pending == nil {
		t.Fatal("three real losses in a rolling minute were missed")
	}
}

func TestNetworkRebootIsDurableAndRateLimitedAcrossRestarts(t *testing.T) {
	for _, fails := range []bool{false, true} {
		w, err := loadNetworkWatchdog(t.TempDir())
		must(t, err)
		now := time.UnixMilli(2_000_000)
		if !w.queue(NetworkReboot{At: now.UnixMilli(), Affected: 3, Total: 5}) {
			t.Fatal("not queued")
		}
		calls := 0
		err = w.tick(now, func() error {
			calls++
			// A poll must not import an unfinished command as a success.
			if len(w.Reports()) != 0 {
				t.Fatal("unfinished command exposed")
			}
			saved, err := loadNetworkWatchdog(filepath.Dir(w.path))
			must(t, err)
			if saved.state.LastAttempt != now.UnixMilli() || len(saved.Reports()) != 1 {
				t.Fatal("reboot called before its reason and cooldown reached disk")
			}
			if fails {
				return errors.New("systemctl refused")
			}
			return nil
		})
		if (err != nil) != fails || calls != 1 {
			t.Fatalf("unexpected command result: %d, %v", calls, err)
		}
		w, err = loadNetworkWatchdog(filepath.Dir(w.path))
		must(t, err)
		if len(w.Reports()) != 1 || (w.Reports()[0].Error != "") != fails {
			t.Fatal("outcome lost after restart", w.Reports())
		}
		must(t, w.tick(now.Add(time.Minute), func() error { t.Fatal("retried old command"); return nil }))
		if w.queue(NetworkReboot{At: now.Add(9 * time.Minute).UnixMilli()}) {
			t.Fatal("cooldown lost after restart")
		}
		if !w.queue(NetworkReboot{At: now.Add(networkRebootCooldown).UnixMilli(), Affected: 1, Total: 2}) {
			t.Fatal("new instability after cooldown ignored")
		}
	}
}

func TestNetworkNeverRebootsWithoutPersistingReason(t *testing.T) {
	w, err := loadNetworkWatchdog(t.TempDir())
	must(t, err)
	must(t, os.Mkdir(w.path, 0700))
	w.queue(NetworkReboot{At: 2_000_000, Affected: 1, Total: 2})
	if err = w.tick(time.UnixMilli(2_000_000), func() error { t.Fatal("unsafe reboot"); return nil }); err == nil {
		t.Fatal("expected persistence failure")
	}
	if w.pending == nil {
		t.Fatal("persistence failure lost the pending request")
	}
}

func TestNetworkTickConsumesAllShortHooksInOneBatch(t *testing.T) {
	r, cameras := networkRuntime(t, 2, 0)
	r.mediaSince.Store(1_000_000)
	for i := int64(0); i < 3; i++ {
		r.streamSignals <- StreamSignal{CameraID: cameras[0].ID, Online: true, At: 1_001_000 + i*1000}
		r.streamSignals <- StreamSignal{CameraID: cameras[0].ID, Online: false, At: 1_001_001 + i*1000}
	}
	must(t, r.tickStreams(context.Background(), time.UnixMilli(1_004_000)))
	if r.network.pending == nil {
		t.Fatal("short flaps between observations disappeared")
	}
}
