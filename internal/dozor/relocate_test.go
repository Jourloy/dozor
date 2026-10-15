package dozor

import (
	"context"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const cameraMAC = "aa:bb:cc:00:00:01"

func testRelocator(t *testing.T, table map[string]string, reachable map[string]string) *relocator {
	t.Helper()
	_, lan, _ := net.ParseCIDR("192.168.1.0/24")
	lan.IP = net.ParseIP("192.168.1.10").To4()
	l := newRelocator(filepath.Join(t.TempDir(), "camera-macs.json"))
	l.arp = func() (map[string]string, error) {
		v := map[string]string{}
		for k, m := range table {
			v[k] = m
		}
		return v, nil
	}
	l.subnets = func() ([]*net.IPNet, error) { return []*net.IPNet{lan}, nil }
	l.dial = func(_ context.Context, address string) bool {
		host, _, _ := net.SplitHostPort(address)
		if mac, ok := reachable[host]; ok {
			table[host] = mac
			return true
		}
		return false
	}
	return l
}

func relocateCamera() Camera {
	return Camera{ID: strings.Repeat("a", 32), Name: "Двор", URL: "rtsp://192.168.1.20:554/main", SubURL: "rtsp://192.168.1.20/sub", ONVIF: "http://192.168.1.20:8000/onvif/device_service", Motion: "auto", Sensitivity: 0.5, Enabled: true}
}

func TestRelocatorFindsCameraAtNewAddress(t *testing.T) {
	cam := relocateCamera()
	table := map[string]string{"192.168.1.20": cameraMAC}
	l := testRelocator(t, table, map[string]string{"192.168.1.37": cameraMAC})
	now := time.Now()
	if moves := l.step(context.Background(), []Camera{cam}, map[string]AvailabilityInterval{cam.ID: {State: "online", Start: now.UnixMilli()}}, now); len(moves) != 0 {
		t.Fatal(moves)
	}
	if l.macs[cam.ID] != cameraMAC {
		t.Fatal("MAC was not learned while online")
	}
	// The camera left: its old entry is gone and the address is now free.
	delete(table, "192.168.1.20")
	offline := map[string]AvailabilityInterval{cam.ID: {State: "offline", Start: now.UnixMilli()}}
	if moves := l.step(context.Background(), []Camera{cam}, offline, now.Add(time.Minute)); len(moves) != 0 {
		t.Fatal("searched too early", moves)
	}
	moves := l.step(context.Background(), []Camera{cam}, offline, now.Add(3*time.Minute))
	if len(moves) != 1 || moves[0].From != "192.168.1.20" || moves[0].To != "192.168.1.37" {
		t.Fatal(moves)
	}
	if again := l.step(context.Background(), []Camera{cam}, offline, now.Add(4*time.Minute)); len(again) != 0 {
		t.Fatal("retried before the pause", again)
	}
	c := Config{Cameras: []Camera{cam}}
	if applied := applyCameraMoves(&c, moves); len(applied) != 1 {
		t.Fatal(applied)
	}
	got := c.Cameras[0]
	if got.URL != "rtsp://192.168.1.37:554/main" || got.SubURL != "rtsp://192.168.1.37/sub" || got.ONVIF != "http://192.168.1.37:8000/onvif/device_service" {
		t.Fatal(got)
	}
	if ValidateCamera(got) != nil {
		t.Fatal(ValidateCamera(got))
	}
	// The MAC survives a restart.
	if newRelocator(l.path).macs[cam.ID] != cameraMAC {
		t.Fatal("MAC was not persisted")
	}
}

func TestRelocatorLeavesCameraThatKeptItsAddress(t *testing.T) {
	cam := relocateCamera()
	table := map[string]string{"192.168.1.20": cameraMAC, "192.168.1.37": cameraMAC}
	l := testRelocator(t, table, nil)
	l.macs[cam.ID] = cameraMAC
	now := time.Now()
	offline := map[string]AvailabilityInterval{cam.ID: {State: "offline", Start: now.Add(-time.Hour).UnixMilli()}}
	if moves := l.step(context.Background(), []Camera{cam}, offline, now); len(moves) != 0 {
		t.Fatal("moved a camera that still answers at its address", moves)
	}
}

func TestRelocatorSkipsAmbiguousAndRemoteCameras(t *testing.T) {
	cam := relocateCamera()
	table := map[string]string{"192.168.1.37": cameraMAC, "192.168.1.38": cameraMAC}
	l := testRelocator(t, table, nil)
	l.macs[cam.ID] = cameraMAC
	now := time.Now()
	offline := map[string]AvailabilityInterval{cam.ID: {State: "offline", Start: now.Add(-time.Hour).UnixMilli()}}
	if moves := l.step(context.Background(), []Camera{cam}, offline, now); len(moves) != 0 {
		t.Fatal("picked one of two addresses", moves)
	}
	remote := cam
	remote.URL = "rtsp://10.0.0.5/main"
	l.tried = map[string]time.Time{}
	if moves := l.step(context.Background(), []Camera{remote}, offline, now); len(moves) != 0 {
		t.Fatal("moved a camera outside the Pi's subnet", moves)
	}
	named := cam
	named.URL = "rtsp://camera.local/main"
	if cameraIP(named) != nil {
		t.Fatal("a host name must be left to DNS")
	}
}

func TestSweepHostsAndARP(t *testing.T) {
	_, wide, _ := net.ParseCIDR("10.0.0.0/16")
	own := []*net.IPNet{{IP: net.ParseIP("10.0.4.2").To4(), Mask: wide.Mask}}
	hosts := sweepHosts(wide, net.ParseIP("10.0.4.20").To4(), own)
	if len(hosts) != 253 || hosts[0] != "10.0.4.1" || hosts[len(hosts)-1] != "10.0.4.254" {
		t.Fatal(len(hosts), hosts[0], hosts[len(hosts)-1])
	}
	table, err := parseARP(strings.NewReader(`IP address       HW type     Flags       HW address            Mask     Device
192.168.1.1      0x1         0x2         AA:BB:CC:DD:EE:FF     *        eth0
192.168.1.44     0x1         0x0         00:00:00:00:00:00     *        eth0
`))
	if err != nil || len(table) != 1 || table["192.168.1.1"] != "aa:bb:cc:dd:ee:ff" {
		t.Fatal(table, err)
	}
}
