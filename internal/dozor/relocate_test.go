package dozor

import (
	"bufio"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/textproto"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// fakeNetwork maps hosts to the login, path and serial of the device there.
type fakeDevice struct{ login, path, serial string }

func testRelocator(t *testing.T, network map[string]fakeDevice) *relocator {
	t.Helper()
	l := newRelocator(filepath.Join(t.TempDir(), "camera-serials.json"))
	l.gap = 0
	l.subnets = func() ([]*net.IPNet, error) {
		return []*net.IPNet{{IP: net.ParseIP("192.168.1.10").To4(), Mask: net.CIDRMask(24, 32)}}, nil
	}
	l.open = func(_ context.Context, address string) dialOutcome {
		host, _, _ := net.SplitHostPort(address)
		if _, ok := network[host]; ok {
			return dialOpen
		}
		return dialRefused
	}
	device := func(cam Camera) (fakeDevice, bool) {
		d, ok := network[cameraIP(cam).String()]
		return d, ok
	}
	l.accepts = func(_ context.Context, cam Camera) rtspVerdict {
		d, ok := device(cam)
		switch {
		case !ok:
			return rtspVerdict{note: "refused"}
		case d.login != cam.Username+":"+cam.Password:
			return rtspReply(401)
		case !strings.HasSuffix(cam.URL, d.path):
			return rtspReply(404)
		}
		return rtspReply(200)
	}
	l.serial = func(_ context.Context, cam Camera) (string, error) {
		d, ok := device(cam)
		if !ok {
			return "", net.ErrClosed
		}
		return d.serial, nil
	}
	// Tests never send real multicast.
	l.discover = func(context.Context, net.IP) ([]string, error) { return nil, nil }
	return l
}

func relocateCamera() Camera {
	return Camera{ID: strings.Repeat("a", 32), Name: "Двор", URL: "rtsp://192.168.1.20:554/main", SubURL: "rtsp://192.168.1.20/sub", ONVIF: "http://192.168.1.20:8000/onvif/device_service", Username: "admin", Password: "secret", Motion: "auto", Sensitivity: 0.5, Enabled: true}
}

func offlineSince(cam Camera, start time.Time) map[string]AvailabilityInterval {
	return map[string]AvailabilityInterval{cam.ID: {State: "offline", Start: start.UnixMilli()}}
}

func TestRelocatorFindsCameraByLogin(t *testing.T) {
	cam := relocateCamera()
	network := map[string]fakeDevice{
		"192.168.1.5":  {login: "admin:other", path: "/main"},
		"192.168.1.37": {login: "admin:secret", path: "/main", serial: "SN1"},
	}
	l := testRelocator(t, network)
	now := time.Now()
	offline := offlineSince(cam, now)
	if moves, _ := l.step(context.Background(), []Camera{cam}, offline, now.Add(time.Minute)); len(moves) != 0 {
		t.Fatal("searched too early", moves)
	}
	moves, _ := l.step(context.Background(), []Camera{cam}, offline, now.Add(3*time.Minute))
	if len(moves) != 1 || moves[0].From != "192.168.1.20" || moves[0].To != "192.168.1.37" {
		t.Fatal(moves)
	}
	if again, _ := l.step(context.Background(), []Camera{cam}, offline, now.Add(4*time.Minute)); len(again) != 0 {
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
}

func TestRelocatorLeavesCameraThatKeptItsAddress(t *testing.T) {
	cam := relocateCamera()
	l := testRelocator(t, map[string]fakeDevice{
		"192.168.1.20": {login: "admin:secret", path: "/main"},
		"192.168.1.37": {login: "admin:secret", path: "/main"},
	})
	now := time.Now()
	offline := offlineSince(cam, now.Add(-time.Hour))
	if moves, notices := l.step(context.Background(), []Camera{cam}, offline, now); len(moves) != 0 || len(notices) != 0 {
		t.Fatal("moved a camera that still answers at its address", moves, notices)
	}
}

func TestRelocatorTellsSameLoginCamerasApartBySerial(t *testing.T) {
	cam := relocateCamera()
	other := relocateCamera()
	other.ID, other.Name, other.URL, other.SubURL, other.ONVIF = strings.Repeat("b", 32), "Гараж", "rtsp://192.168.1.21/main", "rtsp://192.168.1.21/sub", ""
	network := map[string]fakeDevice{
		"192.168.1.20": {login: "admin:secret", path: "/main", serial: "SN1"},
		"192.168.1.21": {login: "admin:secret", path: "/main", serial: "SN2"},
		"192.168.1.40": {login: "admin:secret", path: "/main", serial: "SN3"},
	}
	l := testRelocator(t, network)
	now := time.Now()
	online := map[string]AvailabilityInterval{cam.ID: {State: "online"}, other.ID: {State: "online"}}
	l.step(context.Background(), []Camera{cam, other}, online, now)
	if l.serials[cam.ID] != "SN1" || newRelocator(l.path).serials[cam.ID] != "SN1" {
		t.Fatal("serial was not remembered", l.serials)
	}
	// The camera moved to .41; .40 is an unconfigured camera with the same
	// login, .21 belongs to another configured camera and is never probed.
	delete(network, "192.168.1.20")
	network["192.168.1.41"] = fakeDevice{login: "admin:secret", path: "/main", serial: "SN1"}
	offline := map[string]AvailabilityInterval{cam.ID: {State: "offline", Start: now.Add(-time.Hour).UnixMilli()}, other.ID: {State: "online"}}
	moves, notices := l.step(context.Background(), []Camera{cam, other}, offline, now.Add(time.Hour))
	if len(moves) != 1 || moves[0].To != "192.168.1.41" || len(notices) != 0 {
		t.Fatal(moves, notices)
	}
	// Without a serial the same picture is ambiguous and nothing moves.
	l.serials = map[string]string{}
	l.tried = map[string]time.Time{}
	l.warned = map[string]bool{}
	cam.ONVIF = ""
	moves, notices = l.step(context.Background(), []Camera{cam, other}, offline, now.Add(2*time.Hour))
	if len(moves) != 0 || len(notices) != 1 || !strings.Contains(notices[0], "нескольким камерам") {
		t.Fatal(moves, notices)
	}
	// The warning is given once per disappearance.
	if _, notices = l.step(context.Background(), []Camera{cam, other}, offline, now.Add(3*time.Hour)); len(notices) != 0 {
		t.Fatal("warned twice", notices)
	}
}

func TestRelocatorNoticesFailedSearchOncePerOfflinePeriod(t *testing.T) {
	cam := relocateCamera()
	l := testRelocator(t, map[string]fakeDevice{})
	now := time.Now()
	step := func(start time.Time, after time.Duration) []string {
		moves, notices := l.step(context.Background(), []Camera{cam}, offlineSince(cam, start), start.Add(after))
		if len(moves) != 0 {
			t.Fatal(moves)
		}
		return notices
	}
	first := step(now, 3*time.Minute)
	want := "Камера «Двор» не отвечает по адресу 192.168.1.20. Поиск в сети 192.168.1.0/24: ни один адрес не ответил на порту RTSP. Поиск повторяется каждые 5 минут"
	if len(first) != 1 || first[0] != want {
		t.Fatal(first)
	}
	if n := step(now, 8*time.Minute); len(n) != 0 {
		t.Fatal("repeated the notice at the next attempt", n)
	}
	if n := step(now, 5*time.Hour); len(n) != 0 {
		t.Fatal("repeated the notice within 6 hours", n)
	}
	if n := step(now, 6*time.Hour+3*time.Minute); len(n) != 1 {
		t.Fatal("no reminder after 6 hours", n)
	}
	if n := step(now, 6*time.Hour+10*time.Minute); len(n) != 0 {
		t.Fatal("reminded twice", n)
	}
	// The camera came back and went away again: a new period, a new notice.
	later := now.Add(6*time.Hour + 20*time.Minute)
	l.step(context.Background(), []Camera{cam}, map[string]AvailabilityInterval{cam.ID: {State: "online"}}, later)
	if n := step(later, 3*time.Minute); len(n) != 1 {
		t.Fatal("no notice for a new offline period", n)
	}
}

// After a day offline the availability interval starts at now-24h and moves
// with every tick: it cannot tell one offline period from the next.
func TestRelocatorPeriodSurvivesSlidingStart(t *testing.T) {
	cam := relocateCamera()
	ctx := context.Background()
	count := func(l *relocator, cameras []Camera, attempts int) (moves, notices int) {
		t0 := time.Now()
		for i := range attempts {
			at := t0.Add(time.Duration(i) * relocateRetry)
			m, n := l.step(ctx, cameras, offlineSince(cam, at.Add(-24*time.Hour)), at)
			moves, notices = moves+len(m), notices+len(n)
		}
		return
	}
	// Nothing found: the first attempt and one reminder after 6 hours.
	l := testRelocator(t, map[string]fakeDevice{})
	if _, n := count(l, []Camera{cam}, 80); n != 2 {
		t.Fatal("notices over 6h40m:", n)
	}
	// Two hosts for one login and no serial: the warning is given once.
	same := fakeDevice{login: "admin:secret", path: "/main"}
	l = testRelocator(t, map[string]fakeDevice{"192.168.1.5": same, "192.168.1.6": same})
	if _, n := count(l, []Camera{cam}, 80); n != 1 {
		t.Fatal("ambiguity warnings:", n)
	}
	// Back online: the next period is reported again.
	l.step(ctx, []Camera{cam}, map[string]AvailabilityInterval{cam.ID: {State: "online"}}, time.Now())
	if len(l.tried)+len(l.warned)+len(l.notified) != 0 {
		t.Fatal("online camera keeps its offline period", l.tried, l.warned, l.notified)
	}
	// A camera that is disabled or removed does not keep one either.
	l = testRelocator(t, map[string]fakeDevice{})
	count(l, []Camera{cam}, 1)
	cam.Enabled = false
	l.step(ctx, []Camera{cam}, offlineSince(cam, time.Now()), time.Now())
	if len(l.tried)+len(l.warned)+len(l.notified) != 0 {
		t.Fatal("disabled camera keeps its offline period", l.tried, l.notified)
	}
}

func TestRelocatorChecksSerialOfLoneHost(t *testing.T) {
	cam := relocateCamera()
	other := relocateCamera()
	other.ID, other.Name, other.URL, other.SubURL, other.ONVIF = strings.Repeat("b", 32), "Гараж", "rtsp://192.168.1.21/main", "rtsp://192.168.1.21/sub", ""
	now := time.Now()
	states := map[string]AvailabilityInterval{cam.ID: {State: "offline", Start: now.UnixMilli()}, other.ID: {State: "online"}}
	// run lets the only accepting host (.41) show serial got, remembers want for
	// the lost camera and ownSN2 for the other camera.
	run := func(want, got string, readable bool, otherSerial string) ([]CameraMove, []string) {
		l := testRelocator(t, map[string]fakeDevice{"192.168.1.41": {login: "admin:secret", path: "/main", serial: got}})
		if !readable {
			l.serial = func(context.Context, Camera) (string, error) { return "", errors.New("no answer") }
		}
		if want != "" {
			l.serials[cam.ID] = want
		}
		if otherSerial != "" {
			l.serials[other.ID] = otherSerial
		}
		return l.step(context.Background(), []Camera{cam, other}, states, now.Add(3*time.Minute))
	}
	if moves, _ := run("SN1", "SN1", true, ""); len(moves) != 1 || moves[0].To != "192.168.1.41" {
		t.Fatal("lost the camera that matches its serial", moves)
	}
	moves, notices := run("SN1", "OTHER-CAMERA", true, "")
	if len(moves) != 0 || len(notices) != 1 || !strings.Contains(notices[0], "это другая камера") {
		t.Fatal("moved to a device with another serial", moves, notices)
	}
	if moves, _ := run("SN1", "SN1", false, ""); len(moves) != 1 {
		t.Fatal("an unreadable serial rejected the only host", moves)
	}
	// The lost camera's own serial is unknown, but .41 shows the Garage's.
	moves, notices = run("", "SN2", true, "SN2")
	if len(moves) != 0 || len(notices) != 1 {
		t.Fatal("moved to the device of another camera", moves, notices)
	}
	if moves, _ = run("", "SN9", true, "SN2"); len(moves) != 1 {
		t.Fatal("rejected a device that belongs to no camera", moves)
	}
}

func TestRelocatorFailureNoticeNamesWhatWasFound(t *testing.T) {
	cam := relocateCamera()
	wrong := fakeDevice{login: "admin:other", path: "/main"}
	cases := []struct {
		network map[string]fakeDevice
		part    string
	}{
		{map[string]fakeDevice{"192.168.1.5": wrong}, "на 1 адресе открыт порт RTSP, логин камеры не подошёл."},
		{map[string]fakeDevice{"192.168.1.5": wrong, "192.168.1.6": wrong, "192.168.1.7": wrong}, "на 3 адресах открыт порт RTSP, логин камеры не подошёл ни к одному."},
	}
	for _, c := range cases {
		l := testRelocator(t, c.network)
		now := time.Now()
		_, notices := l.step(context.Background(), []Camera{cam}, offlineSince(cam, now), now.Add(3*time.Minute))
		if len(notices) != 1 || !strings.Contains(notices[0], c.part) || strings.Contains(notices[0], "secret") {
			t.Fatal(notices)
		}
	}
	// A host found only by WS-Discovery is counted too.
	l := testRelocator(t, map[string]fakeDevice{"192.168.1.5": wrong})
	l.open = func(context.Context, string) dialOutcome { return dialRefused }
	l.discover = func(context.Context, net.IP) ([]string, error) { return []string{"192.168.1.5"}, nil }
	now := time.Now()
	_, notices := l.step(context.Background(), []Camera{cam}, offlineSince(cam, now), now.Add(3*time.Minute))
	if len(notices) != 1 || !strings.Contains(notices[0], "на 1 адресе отвечает RTSP или ONVIF, логин камеры не подошёл") {
		t.Fatal(notices)
	}
	// Several accepting hosts that all differ in serial number.
	good := fakeDevice{login: "admin:secret", path: "/main", serial: "SNx"}
	l = testRelocator(t, map[string]fakeDevice{"192.168.1.5": good, "192.168.1.6": good})
	l.serials[cam.ID] = "SN1"
	_, notices = l.step(context.Background(), []Camera{cam}, offlineSince(cam, now), now.Add(3*time.Minute))
	if len(notices) != 1 || !strings.Contains(notices[0], "серийный номер ONVIF у каждого не совпал или не читается") {
		t.Fatal(notices)
	}
}

// The notice says what the hosts did, not that the login was wrong when they
// only timed out, hung up or did not know the path.
func TestRelocatorFailureNoticeFollowsTheDominantRefusal(t *testing.T) {
	cam := relocateCamera()
	wrong := fakeDevice{login: "admin:other", path: "/main"}
	otherPath := fakeDevice{login: "admin:secret", path: "/other"}
	silent := func(l *relocator, hosts ...string) {
		accepts := l.accepts
		l.accepts = func(ctx context.Context, c Camera) rtspVerdict {
			if slices.Contains(hosts, cameraIP(c).String()) {
				return rtspVerdict{note: "timeout"}
			}
			return accepts(ctx, c)
		}
	}
	cases := []struct {
		name    string
		network map[string]fakeDevice
		silent  []string
		part    string
	}{
		{"one slow", map[string]fakeDevice{"192.168.1.5": wrong}, []string{"192.168.1.5"}, "на 1 адресе открыт порт RTSP, устройство не отдало видеопоток камеры."},
		{"all slow", map[string]fakeDevice{"192.168.1.5": wrong, "192.168.1.6": wrong}, []string{"192.168.1.5", "192.168.1.6"}, "на 2 адресах открыт порт RTSP, ни одно устройство не отдало видеопоток камеры."},
		{"mostly slow", map[string]fakeDevice{"192.168.1.5": wrong, "192.168.1.6": wrong, "192.168.1.7": wrong}, []string{"192.168.1.6", "192.168.1.7"}, "ни одно устройство не отдало видеопоток камеры."},
		{"mostly login", map[string]fakeDevice{"192.168.1.5": wrong, "192.168.1.6": wrong, "192.168.1.7": wrong}, []string{"192.168.1.7"}, "логин камеры не подошёл ни к одному."},
		{"one path", map[string]fakeDevice{"192.168.1.5": otherPath}, nil, "на 1 адресе открыт порт RTSP, путь к видеопотоку камеры не подошёл."},
		{"paths", map[string]fakeDevice{"192.168.1.5": otherPath, "192.168.1.6": otherPath, "192.168.1.7": wrong}, nil, "путь к видеопотоку камеры не подошёл ни к одному."},
		{"tie", map[string]fakeDevice{"192.168.1.5": otherPath, "192.168.1.6": wrong}, nil, "ни одно устройство не отдало видеопоток камеры."},
	}
	for _, c := range cases {
		l := testRelocator(t, c.network)
		silent(l, c.silent...)
		now := time.Now()
		_, notices := l.step(context.Background(), []Camera{cam}, offlineSince(cam, now), now.Add(3*time.Minute))
		if len(notices) != 1 || !strings.Contains(notices[0], c.part) || strings.Contains(notices[0], "secret") {
			t.Fatal(c.name, notices)
		}
	}
}

// A notice that could not be written is not spent: the next attempt reports
// again, and so does an ambiguity warning.
func TestRelocatorRetractTakesBackTheNotices(t *testing.T) {
	cam := relocateCamera()
	ctx := context.Background()
	now := time.Now()
	for name, network := range map[string]map[string]fakeDevice{
		"not found": {},
		"ambiguous": {"192.168.1.5": {login: "admin:secret", path: "/main"}, "192.168.1.6": {login: "admin:secret", path: "/main"}},
	} {
		l := testRelocator(t, network)
		offline := offlineSince(cam, now)
		_, first := l.step(ctx, []Camera{cam}, offline, now.Add(3*time.Minute))
		if len(first) != 1 {
			t.Fatal(name, first)
		}
		l.retract(nil)
		if len(l.tried) != 1 {
			t.Fatal(name, "retract forgot the attempt itself", l.tried)
		}
		delete(l.tried, cam.ID) // the pause between attempts is not what this test is about
		if _, again := l.step(ctx, []Camera{cam}, offline, now.Add(4*time.Minute)); len(again) != 1 || again[0] != first[0] {
			t.Fatal(name, "not reported again", again)
		}
		// Delivered this time: the throttle holds.
		if _, third := l.step(ctx, []Camera{cam}, offline, now.Add(10*time.Minute)); len(third) != 0 {
			t.Fatal(name, "reported twice", third)
		}
	}
	// A reminder that was lost keeps its place: it is due at the next attempt.
	l := testRelocator(t, map[string]fakeDevice{})
	offline := offlineSince(cam, now)
	l.step(ctx, []Camera{cam}, offline, now.Add(3*time.Minute))
	_, reminder := l.step(ctx, []Camera{cam}, offline, now.Add(6*time.Hour+4*time.Minute))
	if len(reminder) != 1 {
		t.Fatal(reminder)
	}
	l.retract(nil)
	delete(l.tried, cam.ID)
	if _, again := l.step(ctx, []Camera{cam}, offline, now.Add(6*time.Hour+9*time.Minute)); len(again) != 1 {
		t.Fatal("lost reminder", again)
	}
	// A camera that was found is searched again when its move could not be made.
	l = testRelocator(t, map[string]fakeDevice{"192.168.1.37": {login: "admin:secret", path: "/main"}})
	moves, _ := l.step(ctx, []Camera{cam}, offline, now.Add(3*time.Minute))
	if len(moves) != 1 {
		t.Fatal(moves)
	}
	if again, _ := l.step(ctx, []Camera{cam}, offline, now.Add(4*time.Minute)); len(again) != 0 {
		t.Fatal("searched before the pause", again)
	}
	l.retract(moves)
	if again, _ := l.step(ctx, []Camera{cam}, offline, now.Add(4*time.Minute)); len(again) != 1 {
		t.Fatal("not searched again", again)
	}
}

func TestRelocatorProbesSilentHostsAgain(t *testing.T) {
	cam := relocateCamera()
	l := testRelocator(t, map[string]fakeDevice{"192.168.1.37": {login: "admin:secret", path: "/main"}})
	var mu sync.Mutex
	calls := map[string]int{}
	l.open = func(_ context.Context, address string) dialOutcome {
		host, _, _ := net.SplitHostPort(address)
		mu.Lock()
		defer mu.Unlock()
		calls[host]++
		switch {
		case host == "192.168.1.37" && calls[host] == 2:
			return dialOpen
		case host == "192.168.1.37" || host == "192.168.1.38":
			return dialTimeout // silent
		case host == "192.168.1.39":
			return dialOther
		}
		return dialRefused
	}
	now := time.Now()
	moves, _ := l.step(context.Background(), []Camera{cam}, offlineSince(cam, now), now.Add(3*time.Minute))
	if len(moves) != 1 || moves[0].To != "192.168.1.37" {
		t.Fatal(moves)
	}
	for host, want := range map[string]int{"192.168.1.37": 2, "192.168.1.38": 2, "192.168.1.39": 2, "192.168.1.40": 1, "192.168.1.45": 1} {
		if calls[host] != want {
			t.Fatal(host, calls[host], "probes, want", want)
		}
	}
}

func TestRelocatorUsesDiscoveredHosts(t *testing.T) {
	cam := relocateCamera()
	other := relocateCamera()
	other.ID, other.Name, other.URL, other.SubURL, other.ONVIF = strings.Repeat("b", 32), "Гараж", "rtsp://192.168.1.21/main", "rtsp://192.168.1.21/sub", ""
	same := fakeDevice{login: "admin:secret", path: "/main"}
	// Everything takes the login, but only .50 is a candidate: .21 belongs to
	// another camera, .10 is the Pi, 10.9.9.9 is outside the swept network.
	network := map[string]fakeDevice{"192.168.1.50": same, "192.168.1.21": same, "192.168.1.10": same, "10.9.9.9": same}
	l := testRelocator(t, network)
	l.open = func(context.Context, string) dialOutcome { return dialTimeout }
	l.discover = func(_ context.Context, local net.IP) ([]string, error) {
		if !local.Equal(net.ParseIP("192.168.1.10")) {
			t.Error("discovery not bound to the Pi's address in the swept network", local)
		}
		return []string{"192.168.1.50", "192.168.1.21", "192.168.1.10", "10.9.9.9", "camera.local", "192.168.1.50"}, nil
	}
	now := time.Now()
	states := map[string]AvailabilityInterval{cam.ID: {State: "offline", Start: now.UnixMilli()}, other.ID: {State: "online"}}
	moves, notices := l.step(context.Background(), []Camera{cam, other}, states, now.Add(3*time.Minute))
	if len(moves) != 1 || moves[0].To != "192.168.1.50" || len(notices) != 0 {
		t.Fatal(moves, notices)
	}
}

func ownNet(cidr string) *net.IPNet {
	ip, n, _ := net.ParseCIDR(cidr)
	return &net.IPNet{IP: ip.To4(), Mask: n.Mask}
}

func TestSearchNets(t *testing.T) {
	list := func(ip string, own ...string) string {
		nets := []*net.IPNet{}
		for _, o := range own {
			nets = append(nets, ownNet(o))
		}
		got := []string{}
		for _, n := range searchNets(net.ParseIP(ip).To4(), nets) {
			got = append(got, n.String())
		}
		return strings.Join(got, " ")
	}
	cases := []struct {
		name, ip, want string
		own            []string
	}{
		{"the Pi's subnet of the old address first, then its other private subnets", "192.168.1.20", "192.168.1.0/24 10.0.0.0/24 172.16.0.0/24",
			[]string{"192.168.1.10/24", "10.0.0.5/24", "172.16.0.2/24", "8.8.8.8/24", "100.64.0.1/10", "10.8.0.2/32"}},
		{"an old address outside the Pi's subnets is reached through the gateway, last", "192.168.31.26", "192.168.1.0/24 10.0.0.0/24 192.168.31.0/24",
			[]string{"192.168.1.10/24", "10.0.0.5/24"}},
		{"no subnets known", "192.168.31.26", "192.168.31.0/24", nil},
		{"a wide subnet is narrowed", "10.0.4.20", "10.0.4.0/24", []string{"10.0.4.2/16"}},
		{"the Pi's own /24 of a wide subnet is another network", "10.0.4.20", "10.0.4.0/24 10.0.9.0/24", []string{"10.0.4.2/16", "10.0.9.2/16"}},
		{"one subnet twice", "192.168.1.20", "192.168.1.0/24", []string{"192.168.1.10/24", "192.168.1.11/24"}},
		{"a bounded number of hosts", "192.168.1.20", "192.168.1.0/24 192.168.2.0/24 192.168.3.0/24 192.168.4.0/24 192.168.5.0/24",
			[]string{"192.168.1.10/24", "192.168.2.1/24", "192.168.3.1/24", "192.168.4.1/24", "192.168.5.1/24", "192.168.6.1/24"}},
	}
	for _, c := range cases {
		if got := list(c.ip, c.own...); got != c.want {
			t.Fatal(c.name, "\n got:", got, "\nwant:", c.want)
		}
	}
}

// After a router change the camera may sit in a subnet that its old address is
// not in. The Pi's own subnets are searched too.
func TestRelocatorSearchesOtherSubnetsOfThePi(t *testing.T) {
	cam := relocateCamera()
	cam.URL, cam.SubURL, cam.ONVIF = "rtsp://192.168.31.26/main", "rtsp://192.168.31.26/sub", "http://192.168.31.26:8000/onvif/device_service"
	good := fakeDevice{login: "admin:secret", path: "/main"}
	now := time.Now()
	run := func(network map[string]fakeDevice, subnets ...string) ([]CameraMove, []string, []net.IP) {
		l := testRelocator(t, network)
		l.subnets = func() ([]*net.IPNet, error) {
			nets := []*net.IPNet{}
			for _, o := range subnets {
				nets = append(nets, ownNet(o))
			}
			return nets, nil
		}
		var mu sync.Mutex
		var locals []net.IP
		l.discover = func(_ context.Context, local net.IP) ([]string, error) {
			mu.Lock()
			defer mu.Unlock()
			locals = append(locals, local)
			return nil, nil
		}
		moves, notices := l.step(context.Background(), []Camera{cam}, offlineSince(cam, now), now.Add(3*time.Minute))
		return moves, notices, locals
	}
	// The old /24 is not local: the camera is found in the Pi's subnet.
	moves, _, locals := run(map[string]fakeDevice{"192.168.1.37": good}, "192.168.1.10/24")
	if len(moves) != 1 || moves[0].To != "192.168.1.37" || moves[0].From != "192.168.31.26" {
		t.Fatal(moves)
	}
	if len(locals) != 1 || !locals[0].Equal(net.ParseIP("192.168.1.10")) {
		t.Fatal("discovery of the Pi's subnet was not bound to the Pi's address", locals)
	}
	// The old /24 is local and holds nothing: the Pi's other subnet does.
	moves, _, locals = run(map[string]fakeDevice{"10.0.0.37": good}, "192.168.31.10/24", "10.0.0.5/24")
	if len(moves) != 1 || moves[0].To != "10.0.0.37" || len(locals) != 2 {
		t.Fatal(moves, locals)
	}
	// Found in the first network: the others are left alone.
	moves, _, locals = run(map[string]fakeDevice{"192.168.31.37": good, "10.0.0.37": good}, "192.168.31.10/24", "10.0.0.5/24")
	if len(moves) != 1 || moves[0].To != "192.168.31.37" || len(locals) != 1 {
		t.Fatal(moves, locals)
	}
	// Nothing anywhere: the notice names every network that was searched.
	moves, notices, _ := run(map[string]fakeDevice{}, "192.168.31.10/24", "10.0.0.5/24")
	if len(moves) != 0 || len(notices) != 1 || !strings.Contains(notices[0], "Поиск в сетях 192.168.31.0/24, 10.0.0.0/24: ни один адрес не ответил на порту RTSP.") {
		t.Fatal(notices)
	}
}

// Cameras that are lost together look in the same network: it is swept once.
func TestRelocatorSweepsANetworkOncePerStep(t *testing.T) {
	cam := relocateCamera()
	other := relocateCamera()
	other.ID, other.Name, other.URL, other.SubURL, other.ONVIF = strings.Repeat("b", 32), "Гараж", "rtsp://192.168.1.21/main", "rtsp://192.168.1.21/sub", ""
	third := other
	third.ID, third.Name, third.URL, third.SubURL, third.Password = strings.Repeat("c", 32), "Сад", "rtsp://192.168.1.22/main", "rtsp://192.168.1.22/sub", "other"
	l := testRelocator(t, map[string]fakeDevice{"192.168.1.50": {login: "admin:secret", path: "/main"}, "192.168.1.51": {login: "admin:other", path: "/main"}})
	var mu sync.Mutex
	dials, discoveries := map[string]int{}, 0
	open := l.open
	l.open = func(ctx context.Context, address string) dialOutcome {
		mu.Lock()
		dials[address]++
		mu.Unlock()
		return open(ctx, address)
	}
	l.discover = func(context.Context, net.IP) ([]string, error) {
		mu.Lock()
		defer mu.Unlock()
		discoveries++
		return nil, nil
	}
	accepts := l.accepts
	logins := map[string]int{}
	l.accepts = func(ctx context.Context, c Camera) rtspVerdict {
		mu.Lock()
		logins[c.ID+" "+cameraIP(c).String()]++
		mu.Unlock()
		return accepts(ctx, c)
	}
	now := time.Now()
	states := map[string]AvailabilityInterval{}
	for _, c := range []Camera{cam, other, third} {
		states[c.ID] = AvailabilityInterval{State: "offline", Start: now.UnixMilli()}
	}
	moves, _ := l.step(context.Background(), []Camera{cam, other, third}, states, now.Add(3*time.Minute))
	// .50 takes the first login and is then taken: the second camera finds only
	// .51, which has another password, and the third camera has that password.
	if len(moves) != 2 || moves[0].To != "192.168.1.50" || moves[1].To != "192.168.1.51" {
		t.Fatal(moves)
	}
	if len(dials) != 254-1-3 || discoveries != 1 { // the network without the Pi and the three cameras
		t.Fatal(len(dials), "addresses dialled,", discoveries, "discoveries")
	}
	for address, n := range dials {
		if n != 1 {
			t.Fatal(address, "dialled", n, "times")
		}
	}
	// Every camera still checks the hosts with its own login.
	if logins[cam.ID+" 192.168.1.51"] != 1 || logins[other.ID+" 192.168.1.51"] != 1 || logins[third.ID+" 192.168.1.51"] != 1 {
		t.Fatal(logins)
	}
}

func TestRelocatorSurvivesFailedDiscovery(t *testing.T) {
	cam := relocateCamera()
	l := testRelocator(t, map[string]fakeDevice{"192.168.1.37": {login: "admin:secret", path: "/main"}})
	l.discover = func(context.Context, net.IP) ([]string, error) { return nil, errors.New("network is unreachable") }
	now := time.Now()
	moves, _ := l.step(context.Background(), []Camera{cam}, offlineSince(cam, now), now.Add(3*time.Minute))
	if len(moves) != 1 || moves[0].To != "192.168.1.37" {
		t.Fatal(moves)
	}
}

func TestSweepHosts(t *testing.T) {
	own := []*net.IPNet{{IP: net.ParseIP("10.0.4.2").To4(), Mask: net.CIDRMask(16, 32)}}
	hosts := sweepHosts(sweepNet(net.ParseIP("10.0.4.20").To4(), own), own)
	if len(hosts) != 253 || hosts[0] != "10.0.4.1" || hosts[len(hosts)-1] != "10.0.4.254" {
		t.Fatal(len(hosts), hosts[0], hosts[len(hosts)-1])
	}
	if n := sweepNet(net.ParseIP("10.0.4.20").To4(), own); n.String() != "10.0.4.0/24" {
		t.Fatal(n)
	}
	own = []*net.IPNet{{IP: net.ParseIP("192.168.0.10").To4(), Mask: net.CIDRMask(23, 32)}}
	if hosts = sweepHosts(sweepNet(net.ParseIP("192.168.1.20").To4(), own), own); len(hosts) != 509 {
		t.Fatal(len(hosts))
	}
	if n := sweepNet(net.ParseIP("192.168.1.20").To4(), own); n.String() != "192.168.0.0/23" {
		t.Fatal(n)
	}
}

func TestRelocateAddressForms(t *testing.T) {
	for n, want := range map[int]string{1: "1 адресе", 2: "2 адресах", 5: "5 адресах", 11: "11 адресах", 21: "21 адресе", 22: "22 адресах", 111: "111 адресах"} {
		if got := addresses(n); got != want {
			t.Fatal(n, got)
		}
	}
}

func TestRelocateDialOutcome(t *testing.T) {
	refused := &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	unreachable := &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", syscall.EHOSTUNREACH)}
	for err, want := range map[error]dialOutcome{
		nil:                      dialOpen,
		refused:                  dialRefused,
		os.ErrDeadlineExceeded:   dialTimeout,
		context.DeadlineExceeded: dialTimeout,
		unreachable:              dialOther,
		errors.New("other"):      dialOther,
	} {
		if got := classifyDial(err); got != want {
			t.Fatal(err, got, want)
		}
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	address := l.Addr().String()
	if got := dialTCP(context.Background(), address); got != dialOpen {
		t.Fatal(got)
	}
	l.Close()
	if got := dialTCP(context.Background(), address); got != dialRefused {
		t.Fatal(got)
	}
}

// startRaw serves every connection with serve and returns the RTSP base URL.
func startRaw(t *testing.T, serve func(net.Conn)) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go serve(conn)
		}
	}()
	return "rtsp://" + l.Addr().String()
}

// rtspFake is an RTSP server for /main that wants the login admin:secret.
type rtspFake struct {
	challenges []string // WWW-Authenticate lines of a 401; Digest by default
	accept     string   // login scheme it takes: "digest" (default) or "basic"
	drop       bool     // closes the connection after a 401
	closeHdr   bool     // and says so with "Connection: close"
	dropFresh  bool     // closes the connection only after a 401 to a request without login
	perConn    bool     // every connection has its own nonce and accepts no other
	rotate     bool     // every 401 brings a new nonce, the old one is void
	staleFirst bool     // the first login of a connection gets a 401 with stale=true and a new nonce
	conns      atomic.Int32
	nonces     atomic.Int32
	logins     atomic.Int32 // requests that carried a login
}

func (f *rtspFake) authorized(auth, uri, nonce string) bool {
	scheme, rest, _ := strings.Cut(auth, " ")
	switch {
	case f.accept == "basic":
		return strings.EqualFold(scheme, "basic") && rest == base64.StdEncoding.EncodeToString([]byte("admin:secret"))
	case !strings.EqualFold(scheme, "digest"):
		return false
	}
	want, _ := digestAuth(`Digest realm="cam", nonce="`+nonce+`"`, "DESCRIBE", uri, "admin", "secret")
	return responseField(auth) == responseField(want)
}

func (f *rtspFake) serve(conn net.Conn) {
	defer conn.Close()
	nonce := "n1"
	if n := f.conns.Add(1); f.perConn {
		nonce = "c" + strconv.Itoa(int(n))
	}
	r := textproto.NewReader(bufio.NewReader(conn))
	staled := false
	for {
		line, err := r.ReadLine()
		if err != nil {
			return
		}
		h, err := r.ReadMIMEHeader()
		if err != nil {
			return
		}
		seq, uri := h.Get("Cseq"), strings.Fields(line)[1]
		if h.Get("Authorization") != "" {
			f.logins.Add(1)
		}
		switch {
		case !strings.Contains(line, "/main "):
			conn.Write([]byte("RTSP/1.0 404 Not Found\r\nCSeq: " + seq + "\r\n\r\n"))
		case f.staleFirst && !staled && h.Get("Authorization") != "":
			staled, nonce = true, "fresh"
			conn.Write([]byte("RTSP/1.0 401 Unauthorized\r\nCSeq: " + seq + "\r\nWWW-Authenticate: Digest realm=\"cam\", nonce=\"fresh\", stale=true\r\n\r\n"))
		case !f.authorized(h.Get("Authorization"), uri, nonce):
			if f.rotate {
				nonce = "r" + strconv.Itoa(int(f.nonces.Add(1)))
			}
			reply := "RTSP/1.0 401 Unauthorized\r\nCSeq: " + seq + "\r\n"
			challenges := f.challenges
			if challenges == nil {
				challenges = []string{`Digest realm="cam", nonce="` + nonce + `"`}
			}
			for _, c := range challenges {
				reply += "WWW-Authenticate: " + c + "\r\n"
			}
			if f.closeHdr {
				reply += "Connection: close\r\n"
			}
			conn.Write([]byte(reply + "\r\n"))
			if f.drop || f.dropFresh && h.Get("Authorization") == "" {
				return
			}
		default:
			conn.Write([]byte("RTSP/1.0 200 OK\r\nCSeq: " + seq + "\r\nContent-Length: 4\r\n\r\nv=0\n"))
		}
	}
}

func TestRTSPAcceptsDigestLogin(t *testing.T) {
	base := startRaw(t, (&rtspFake{}).serve)
	ctx := context.Background()
	login := func(path, password string) rtspVerdict {
		return rtspAccepts(ctx, Camera{URL: base + path, Username: "admin", Password: password})
	}
	if v := login("/main", "secret"); !v.ok() || v.String() != "accepted" {
		t.Fatal("right login refused", v)
	}
	if v := login("/main", "wrong"); v.ok() || v.String() != "401 auth rejected" {
		t.Fatal("wrong password", v)
	}
	if v := login("/other", "secret"); v.ok() || v.String() != "404 wrong path" {
		t.Fatal("wrong path", v)
	}
}

func TestRTSPAcceptsReconnectsAfterRejection(t *testing.T) {
	for name, closeHdr := range map[string]bool{"silent close": false, "Connection close": true} {
		f := &rtspFake{drop: true, closeHdr: closeHdr}
		base := startRaw(t, f.serve)
		cam := Camera{URL: base + "/main", Username: "admin", Password: "secret"}
		if v := rtspAccepts(context.Background(), cam); !v.ok() {
			t.Fatal(name, v)
		}
		// The first connection is closed after its 401, the second asks for a
		// challenge of its own and is closed too, the third carries the login.
		if n := f.conns.Load(); n != 3 {
			t.Fatal(name, n, "connections")
		}
		if n := f.logins.Load(); n != 1 {
			t.Fatal(name, n, "logins")
		}
		cam.Password = "wrong"
		if v := rtspAccepts(context.Background(), cam); v.ok() || v.String() != "401 auth rejected" {
			t.Fatal(name, "wrong password", v)
		}
		if n := f.logins.Load(); n != 2 {
			t.Fatal(name, n, "logins for two checks")
		}
	}
}

// The camera closes the connection after a 401 to a request without login and
// ties the nonce to the connection. The only login that can get through is
// sent on a connection the challenge did not come from, and its refusal brings
// the challenge of that connection, which is answered.
func TestRTSPAcceptsAnswersChallengeOfNewConnection(t *testing.T) {
	f := &rtspFake{perConn: true, dropFresh: true}
	base := startRaw(t, f.serve)
	cam := Camera{URL: base + "/main", Username: "admin", Password: "secret"}
	if v := rtspAccepts(context.Background(), cam); !v.ok() {
		t.Fatal(v)
	}
	if n := f.conns.Load(); n != 3 {
		t.Fatal(n, "connections")
	}
	cam.Password = "wrong"
	if v := rtspAccepts(context.Background(), cam); v.ok() || v.String() != "401 auth rejected" {
		t.Fatal("wrong password", v)
	}
	// An open connection with a nonce of its own is no different.
	base = startRaw(t, (&rtspFake{perConn: true}).serve)
	cam = Camera{URL: base + "/main", Username: "admin", Password: "secret"}
	if v := rtspAccepts(context.Background(), cam); !v.ok() {
		t.Fatal(v)
	}
}

// A camera that is not ours, with another password, gets one wrong login per
// check: more would lock out cameras that count them.
func TestRTSPAcceptsSendsOneWrongLogin(t *testing.T) {
	cases := map[string]*rtspFake{
		"nonce renewed by every 401": {rotate: true},
		"nonce of the connection":    {perConn: true},
		"fixed nonce":                {},
		"closes after a 401":         {drop: true},
		"closes and says so":         {drop: true, closeHdr: true},
		"Basic":                      {challenges: []string{`Basic realm="cam"`}, accept: "basic"},
	}
	for name, f := range cases {
		base := startRaw(t, f.serve)
		cam := Camera{URL: base + "/main", Username: "admin", Password: "wrong"}
		if v := rtspAccepts(context.Background(), cam); v.ok() || v.String() != "401 auth rejected" {
			t.Fatal(name, v)
		}
		if n := f.logins.Load(); n != 1 {
			t.Fatal(name, n, "wrong logins, want 1")
		}
		cam.Password = "secret"
		if v := rtspAccepts(context.Background(), cam); !v.ok() {
			t.Fatal(name, "right login refused", v)
		}
	}
}

// A stale nonce is no verdict on the login: it is sent again, once.
func TestRTSPAcceptsAnswersStaleNonce(t *testing.T) {
	f := &rtspFake{staleFirst: true}
	base := startRaw(t, f.serve)
	cam := Camera{URL: base + "/main", Username: "admin", Password: "secret"}
	if v := rtspAccepts(context.Background(), cam); !v.ok() {
		t.Fatal(v)
	}
	if n := f.logins.Load(); n != 2 {
		t.Fatal(n, "logins")
	}
	f = &rtspFake{staleFirst: true}
	base = startRaw(t, f.serve)
	cam = Camera{URL: base + "/main", Username: "admin", Password: "wrong"}
	if v := rtspAccepts(context.Background(), cam); v.ok() || v.String() != "401 auth rejected" {
		t.Fatal(v)
	}
	if n := f.logins.Load(); n != 2 {
		t.Fatal(n, "logins, the second only for the stale nonce")
	}
}

// A refused login is tried once more only when the camera sends a new challenge.
func TestRTSPAcceptsRepeatedChallengeIsRefusal(t *testing.T) {
	var logins atomic.Int32
	base := startRaw(t, func(c net.Conn) {
		defer c.Close()
		r := textproto.NewReader(bufio.NewReader(c))
		for {
			if _, err := r.ReadLine(); err != nil {
				return
			}
			h, err := r.ReadMIMEHeader()
			if err != nil {
				return
			}
			if h.Get("Authorization") != "" {
				logins.Add(1)
			}
			c.Write([]byte("RTSP/1.0 401 Unauthorized\r\nCSeq: " + h.Get("Cseq") + "\r\nWWW-Authenticate: Digest realm=\"cam\", nonce=\"same\"\r\n\r\n"))
		}
	})
	if v := rtspAccepts(context.Background(), Camera{URL: base + "/main", Username: "admin", Password: "secret"}); v.ok() || v.String() != "401 auth rejected" {
		t.Fatal(v)
	}
	if n := logins.Load(); n != 1 {
		t.Fatal(n, "login attempts")
	}
}

func TestRTSPAcceptsStopsWhenCancelled(t *testing.T) {
	mute := startRaw(t, func(c net.Conn) { defer c.Close(); io.Copy(io.Discard, c) })
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(100*time.Millisecond, cancel)
	start := time.Now()
	v := rtspAccepts(ctx, Camera{URL: mute + "/main", Username: "admin", Password: "secret"})
	if v.ok() || time.Since(start) > 2*time.Second {
		t.Fatal("kept waiting for a silent host after the cancel", v, time.Since(start))
	}
}

func TestRTSPAcceptsSchemeNames(t *testing.T) {
	cases := map[string]*rtspFake{
		"lowercase digest":    {challenges: []string{`digest realm="cam", nonce="n1"`}},
		"lowercase basic":     {challenges: []string{`basic realm="cam"`}, accept: "basic"},
		"digest after basic":  {challenges: []string{`Basic realm="cam"`, `Digest realm="cam", nonce="n1"`}},
		"digest before basic": {challenges: []string{`Digest realm="cam", nonce="n1"`, `Basic realm="cam"`}},
	}
	for name, f := range cases {
		base := startRaw(t, f.serve)
		if v := rtspAccepts(context.Background(), Camera{URL: base + "/main", Username: "admin", Password: "secret"}); !v.ok() {
			t.Fatal(name, v)
		}
	}
	// Only an unsupported scheme on offer.
	f := &rtspFake{challenges: []string{`Negotiate`}}
	base := startRaw(t, f.serve)
	if v := rtspAccepts(context.Background(), Camera{URL: base + "/main", Username: "admin", Password: "secret"}); v.ok() || v.String() != "401 no supported login method" {
		t.Fatal(v)
	}
}

func TestRTSPAcceptsExplainsFailure(t *testing.T) {
	cam := func(base string) Camera { return Camera{URL: base + "/main", Username: "admin", Password: "secret"} }
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	gone := "rtsp://" + l.Addr().String()
	l.Close()
	if v := rtspAccepts(context.Background(), cam(gone)); v.String() != "refused" {
		t.Fatal(v)
	}
	closed := startRaw(t, func(c net.Conn) { c.Close() })
	if v := rtspAccepts(context.Background(), cam(closed)); v.String() != "connection closed" {
		t.Fatal(v)
	}
	web := startRaw(t, func(c net.Conn) {
		defer c.Close()
		textproto.NewReader(bufio.NewReader(c)).ReadLine()
		c.Write([]byte("HTTP/1.1 400 Bad Request\r\n\r\n"))
	})
	if v := rtspAccepts(context.Background(), cam(web)); v.String() != "not RTSP" {
		t.Fatal(v)
	}
	mute := startRaw(t, func(c net.Conn) { defer c.Close(); io.Copy(io.Discard, c) })
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if v := rtspAccepts(ctx, cam(mute)); v.String() != "timeout" {
		t.Fatal(v)
	}
}

func TestRelocateWSProbe(t *testing.T) {
	lo := net.IPv4(127, 0, 0, 1)
	srv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: lo})
	must(t, err)
	defer srv.Close()
	go func() {
		buf := make([]byte, 65535)
		n, from, err := srv.ReadFromUDP(buf)
		if err != nil || !strings.Contains(string(buf[:n]), "discovery/Probe") {
			return
		}
		srv.WriteToUDP([]byte("not a probe match"), from)
		srv.WriteToUDP([]byte(`<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery"><s:Body><d:ProbeMatches><d:ProbeMatch><d:Scopes>onvif://www.onvif.org/name/cam</d:Scopes><d:XAddrs>http://192.168.1.50:8000/onvif/device_service http://[fe80::1]/onvif http://camera.local/x</d:XAddrs></d:ProbeMatch></d:ProbeMatches></s:Body></s:Envelope>`), from)
	}()
	replies, err := wsProbe(context.Background(), srv.LocalAddr().(*net.UDPAddr), lo, 100*time.Millisecond)
	must(t, err)
	if len(replies) != 1 || !replies[0].from.Equal(lo) || len(replies[0].matches) != 3 || replies[0].matches[0].Scopes != "onvif://www.onvif.org/name/cam" {
		t.Fatal(replies)
	}
	// The UDP source and the XAddrs that are IPv4 addresses.
	if hosts := wsHosts(replies); len(hosts) != 2 || hosts[0] != "127.0.0.1" || hosts[1] != "192.168.1.50" {
		t.Fatal(hosts)
	}
	// A cancelled context ends the wait early.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(20*time.Millisecond, cancel)
	start := time.Now()
	if _, err = wsProbe(ctx, srv.LocalAddr().(*net.UDPAddr), lo, 10*time.Second); err != nil || time.Since(start) > 5*time.Second {
		t.Fatal(err, time.Since(start))
	}
}

func TestRelocateWSProbeSendsAgain(t *testing.T) {
	lo := net.IPv4(127, 0, 0, 1)
	srv, err := net.ListenUDP("udp4", &net.UDPAddr{IP: lo})
	must(t, err)
	defer srv.Close()
	// Nobody answers the first Probes: the multicast may have been lost.
	if replies, err := wsProbe(context.Background(), srv.LocalAddr().(*net.UDPAddr), lo, 400*time.Millisecond); err != nil || len(replies) != 0 {
		t.Fatal(replies, err)
	}
	ids := map[string]bool{}
	buf := make([]byte, 65535)
	_ = srv.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
	for {
		n, _, err := srv.ReadFromUDP(buf)
		if err != nil {
			break
		}
		msg := string(buf[:n])
		_, rest, _ := strings.Cut(msg, "<a:MessageID>")
		id, _, _ := strings.Cut(rest, "</a:MessageID>")
		ids[id] = true
	}
	if len(ids) != 3 || ids[""] {
		t.Fatal("want 3 probes with their own MessageID, got", ids)
	}
}

func responseField(auth string) string {
	for _, m := range digestFields.FindAllStringSubmatch(auth, -1) {
		if m[1] == "response" {
			return m[2]
		}
	}
	return ""
}
