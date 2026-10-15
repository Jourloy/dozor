package dozor

import (
	"bufio"
	"context"
	"net"
	"net/textproto"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeNetwork maps hosts to the login, path and serial of the device there.
type fakeDevice struct{ login, path, serial string }

func testRelocator(t *testing.T, network map[string]fakeDevice) *relocator {
	t.Helper()
	l := newRelocator(filepath.Join(t.TempDir(), "camera-serials.json"))
	l.subnets = func() ([]*net.IPNet, error) {
		return []*net.IPNet{{IP: net.ParseIP("192.168.1.10").To4(), Mask: net.CIDRMask(24, 32)}}, nil
	}
	l.open = func(_ context.Context, address string) bool {
		host, _, _ := net.SplitHostPort(address)
		_, ok := network[host]
		return ok
	}
	device := func(cam Camera) (fakeDevice, bool) {
		d, ok := network[cameraIP(cam).String()]
		return d, ok
	}
	l.accepts = func(_ context.Context, cam Camera) bool {
		d, ok := device(cam)
		return ok && d.login == cam.Username+":"+cam.Password && strings.HasSuffix(cam.URL, d.path)
	}
	l.serial = func(_ context.Context, cam Camera) (string, error) {
		d, ok := device(cam)
		if !ok {
			return "", net.ErrClosed
		}
		return d.serial, nil
	}
	return l
}

func relocateCamera() Camera {
	return Camera{ID: strings.Repeat("a", 32), Name: "Двор", URL: "rtsp://192.168.1.20:554/main", SubURL: "rtsp://192.168.1.20/sub", ONVIF: "http://192.168.1.20:8000/onvif/device_service", Username: "admin", Password: "secret", Motion: "auto", Sensitivity: 0.5, Enabled: true}
}

func TestRelocatorFindsCameraByLogin(t *testing.T) {
	cam := relocateCamera()
	network := map[string]fakeDevice{
		"192.168.1.5":  {login: "admin:other", path: "/main"},
		"192.168.1.37": {login: "admin:secret", path: "/main", serial: "SN1"},
	}
	l := testRelocator(t, network)
	now := time.Now()
	offline := map[string]AvailabilityInterval{cam.ID: {State: "offline", Start: now.UnixMilli()}}
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
	offline := map[string]AvailabilityInterval{cam.ID: {State: "offline", Start: now.Add(-time.Hour).UnixMilli()}}
	if moves, _ := l.step(context.Background(), []Camera{cam}, offline, now); len(moves) != 0 {
		t.Fatal("moved a camera that still answers at its address", moves)
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
	moves, ambiguous := l.step(context.Background(), []Camera{cam, other}, offline, now.Add(time.Hour))
	if len(moves) != 1 || moves[0].To != "192.168.1.41" || len(ambiguous) != 0 {
		t.Fatal(moves, ambiguous)
	}
	// Without a serial the same picture is ambiguous and nothing moves.
	l.serials = map[string]string{}
	l.tried = map[string]time.Time{}
	l.warned = map[string]int64{}
	cam.ONVIF = ""
	moves, ambiguous = l.step(context.Background(), []Camera{cam, other}, offline, now.Add(2*time.Hour))
	if len(moves) != 0 || len(ambiguous) != 1 {
		t.Fatal(moves, ambiguous)
	}
	// The warning is given once per disappearance.
	if _, ambiguous = l.step(context.Background(), []Camera{cam, other}, offline, now.Add(3*time.Hour)); len(ambiguous) != 0 {
		t.Fatal("warned twice", ambiguous)
	}
}

func TestSweepHosts(t *testing.T) {
	own := []*net.IPNet{{IP: net.ParseIP("10.0.4.2").To4(), Mask: net.CIDRMask(16, 32)}}
	hosts := sweepHosts(net.ParseIP("10.0.4.20").To4(), own)
	if len(hosts) != 253 || hosts[0] != "10.0.4.1" || hosts[len(hosts)-1] != "10.0.4.254" {
		t.Fatal(len(hosts), hosts[0], hosts[len(hosts)-1])
	}
	own = []*net.IPNet{{IP: net.ParseIP("192.168.0.10").To4(), Mask: net.CIDRMask(23, 32)}}
	if hosts = sweepHosts(net.ParseIP("192.168.1.20").To4(), own); len(hosts) != 509 {
		t.Fatal(len(hosts))
	}
}

func TestRTSPAcceptsDigestLogin(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				r := textproto.NewReader(bufio.NewReader(conn))
				for {
					line, err := r.ReadLine()
					if err != nil {
						return
					}
					h, err := r.ReadMIMEHeader()
					if err != nil {
						return
					}
					seq := h.Get("Cseq")
					auth := h.Get("Authorization")
					switch {
					case !strings.Contains(line, "/main "):
						conn.Write([]byte("RTSP/1.0 404 Not Found\r\nCSeq: " + seq + "\r\n\r\n"))
					case auth == "":
						conn.Write([]byte("RTSP/1.0 401 Unauthorized\r\nCSeq: " + seq + "\r\nWWW-Authenticate: Digest realm=\"cam\", nonce=\"n1\"\r\n\r\n"))
					default:
						want, _ := digestAuth(`Digest realm="cam", nonce="n1"`, "DESCRIBE", strings.Fields(line)[1], "admin", "secret")
						if responseField(auth) == responseField(want) {
							conn.Write([]byte("RTSP/1.0 200 OK\r\nCSeq: " + seq + "\r\nContent-Length: 4\r\n\r\nv=0\n"))
						} else {
							conn.Write([]byte("RTSP/1.0 401 Unauthorized\r\nCSeq: " + seq + "\r\n\r\n"))
						}
					}
				}
			}()
		}
	}()
	base := "rtsp://" + listener.Addr().String()
	ctx := context.Background()
	if !rtspAccepts(ctx, Camera{URL: base + "/main", Username: "admin", Password: "secret"}) {
		t.Fatal("right login refused")
	}
	if rtspAccepts(ctx, Camera{URL: base + "/main", Username: "admin", Password: "wrong"}) {
		t.Fatal("wrong password accepted")
	}
	if rtspAccepts(ctx, Camera{URL: base + "/other", Username: "admin", Password: "secret"}) {
		t.Fatal("wrong path accepted")
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
