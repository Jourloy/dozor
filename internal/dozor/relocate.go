package dozor

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A camera on DHCP can come back at another address. Once a camera has been
// offline for a while, Dozor sweeps its subnet for hosts that accept the
// camera's RTSP path with its login and password, and moves the camera there.
// MAC addresses are not used: mesh nodes may bridge or proxy them. When
// several hosts accept the same login, the ONVIF serial number remembered
// while the camera was online tells them apart.
const (
	relocateAfter    = 2 * time.Minute
	relocateRetry    = 5 * time.Minute
	relocateTick     = 30 * time.Second
	relocateDial     = 800 * time.Millisecond
	relocateRTSP     = 4 * time.Second
	relocateWorkers  = 64
	relocateMaxHosts = 1024
)

type CameraMove struct {
	ID, Name, From, To string
}

type relocator struct {
	path    string
	serials map[string]string // camera ID -> ONVIF serial seen while online
	tried   map[string]time.Time
	asked   map[string]time.Time
	warned  map[string]int64 // offline start already reported as ambiguous
	subnets func() ([]*net.IPNet, error)
	open    func(ctx context.Context, address string) bool
	accepts func(ctx context.Context, cam Camera) bool
	serial  func(ctx context.Context, cam Camera) (string, error)
}

func newRelocator(path string) *relocator {
	l := &relocator{path: path, serials: map[string]string{}, tried: map[string]time.Time{}, asked: map[string]time.Time{}, warned: map[string]int64{}, subnets: localSubnets, open: dialTCP, accepts: rtspAccepts, serial: onvifSerial}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &l.serials)
	}
	return l
}

func (a *App) runRelocation(ctx context.Context) {
	if a.Development {
		return
	}
	l := newRelocator(filepath.Join(a.StateDir, "camera-serials.json"))
	for pause(ctx, relocateTick) {
		a.mu.RLock()
		r := a.runtime
		a.mu.RUnlock()
		if r == nil || r.mediaSince.Load() == 0 {
			continue
		}
		moves, ambiguous := l.step(ctx, a.Config.Get().Cameras, r.streamSnapshot(), time.Now())
		for _, name := range ambiguous {
			r.Store.Notice(fmt.Sprintf("Камера «%s» не отвечает по своему адресу, а её логин подходит к нескольким камерам в сети; укажите новый адрес вручную", name))
		}
		if len(moves) == 0 {
			continue
		}
		c := a.Config.Get()
		applied := applyCameraMoves(&c, moves)
		if len(applied) == 0 {
			continue
		}
		if err := a.Config.Save(c); err != nil {
			r.Store.Notice("Не удалось сохранить новый адрес камеры: " + err.Error())
			continue
		}
		for _, m := range applied {
			message := fmt.Sprintf("Камера «%s» сменила адрес: %s → %s, настройки обновлены", m.Name, m.From, m.To)
			r.Store.Notice(message)
			log.Print(message)
		}
		a.Reload()
	}
}

func (l *relocator) step(ctx context.Context, cameras []Camera, streams map[string]AvailabilityInterval, now time.Time) (moves []CameraMove, ambiguous []string) {
	nets, _ := l.subnets()
	taken := map[string]bool{}
	for _, cam := range cameras {
		if ip := cameraIP(cam); ip != nil {
			taken[ip.String()] = true
		}
	}
	learned := false
	lost := []Camera{}
	for _, cam := range cameras {
		ip := cameraIP(cam)
		state, ok := streams[cam.ID]
		if !cam.Enabled || ip == nil || !ok {
			continue
		}
		switch {
		case state.State == "online":
			delete(l.tried, cam.ID)
			if cam.ONVIF != "" && l.serials[cam.ID] == "" && now.Sub(l.asked[cam.ID]) >= relocateRetry {
				l.asked[cam.ID] = now
				if s, err := l.serial(ctx, cam); err == nil && s != "" {
					l.serials[cam.ID] = s
					learned = true
				}
			}
		case state.State == "offline" && now.Sub(time.UnixMilli(state.Start)) >= relocateAfter && now.Sub(l.tried[cam.ID]) >= relocateRetry:
			lost = append(lost, cam)
		}
	}
	if learned {
		l.save(cameras)
	}
	for _, cam := range lost {
		l.tried[cam.ID] = now
		old := cameraIP(cam).String()
		if l.accepts(ctx, cam) {
			continue // Still at its address: the stream fails for another reason.
		}
		hosts := []string{}
		for _, h := range sweepHosts(cameraIP(cam), nets) {
			if !taken[h] {
				hosts = append(hosts, h)
			}
		}
		candidates := []string{}
		for _, h := range l.sweep(ctx, hosts, rtspPort(cam)) {
			if l.accepts(ctx, movedCamera(cam, old, h)) {
				candidates = append(candidates, h)
			}
		}
		if len(candidates) > 1 && l.serials[cam.ID] != "" {
			same := []string{}
			for _, h := range candidates {
				if s, err := l.serial(ctx, movedCamera(cam, old, h)); err == nil && s == l.serials[cam.ID] {
					same = append(same, h)
				}
			}
			candidates = same
		}
		switch {
		case len(candidates) == 1:
			moves = append(moves, CameraMove{ID: cam.ID, Name: cam.Name, From: old, To: candidates[0]})
			taken[candidates[0]] = true
		case len(candidates) > 1 && l.warned[cam.ID] != streams[cam.ID].Start:
			l.warned[cam.ID] = streams[cam.ID].Start
			ambiguous = append(ambiguous, cam.Name)
		}
	}
	return moves, ambiguous
}

// Hosts that accept a TCP connection on the RTSP port.
func (l *relocator) sweep(ctx context.Context, hosts []string, port string) []string {
	jobs := make(chan string)
	var mu sync.Mutex
	open := []string{}
	var wg sync.WaitGroup
	for range relocateWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range jobs {
				if l.open(ctx, net.JoinHostPort(h, port)) {
					mu.Lock()
					open = append(open, h)
					mu.Unlock()
				}
			}
		}()
	}
	for _, h := range hosts {
		if ctx.Err() != nil {
			break
		}
		jobs <- h
	}
	close(jobs)
	wg.Wait()
	return open
}

func (l *relocator) save(cameras []Camera) {
	known := map[string]bool{}
	for _, cam := range cameras {
		known[cam.ID] = true
	}
	for id := range l.serials {
		if !known[id] {
			delete(l.serials, id)
		}
	}
	_ = WriteJSON(l.path, l.serials)
}

func movedCamera(cam Camera, from, to string) Camera {
	cam.URL = replaceHost(cam.URL, from, to)
	cam.SubURL = replaceHost(cam.SubURL, from, to)
	cam.ONVIF = replaceHost(cam.ONVIF, from, to)
	return cam
}

func applyCameraMoves(c *Config, moves []CameraMove) []CameraMove {
	applied := []CameraMove{}
	for _, m := range moves {
		for i := range c.Cameras {
			// The user may have edited the camera while the sweep ran.
			if c.Cameras[i].ID != m.ID || cameraIP(c.Cameras[i]) == nil || cameraIP(c.Cameras[i]).String() != m.From {
				continue
			}
			c.Cameras[i] = movedCamera(c.Cameras[i], m.From, m.To)
			applied = append(applied, m)
		}
	}
	return applied
}

func replaceHost(raw, from, to string) string {
	u, err := url.Parse(raw)
	if raw == "" || err != nil || u.Hostname() != from {
		return raw
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(to, port)
	} else {
		u.Host = to
	}
	return u.String()
}

// Cameras addressed by a host name are left to DNS.
func cameraIP(cam Camera) net.IP {
	u, err := url.Parse(cam.URL)
	if err != nil {
		return nil
	}
	return net.ParseIP(u.Hostname()).To4()
}

func rtspPort(cam Camera) string {
	if u, err := url.Parse(cam.URL); err == nil && u.Port() != "" {
		return u.Port()
	}
	return "554"
}

// The Pi's subnet that holds the camera, or the camera's /24 when the camera
// sits elsewhere. Subnets wider than /22 are narrowed to the camera's /24.
// The network, broadcast and Pi's own addresses are skipped.
func sweepHosts(ip net.IP, own []*net.IPNet) []string {
	n := &net.IPNet{IP: ip.Mask(net.CIDRMask(24, 32)), Mask: net.CIDRMask(24, 32)}
	for _, o := range own {
		if ones, bits := o.Mask.Size(); bits == 32 && ones <= 30 && o.Contains(ip) && 1<<(bits-ones) <= relocateMaxHosts {
			n = &net.IPNet{IP: o.IP.Mask(o.Mask), Mask: o.Mask}
		}
	}
	self := map[string]bool{}
	for _, o := range own {
		self[o.IP.String()] = true
	}
	ones, _ := n.Mask.Size()
	base := binary.BigEndian.Uint32(n.IP.To4())
	size := uint32(1) << (32 - ones)
	hosts := []string{}
	for i := uint32(1); i < size-1; i++ {
		h := make(net.IP, 4)
		binary.BigEndian.PutUint32(h, base+i)
		if !self[h.String()] {
			hosts = append(hosts, h.String())
		}
	}
	return hosts
}

func localSubnets() ([]*net.IPNet, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	nets := []*net.IPNet{}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			if n, ok := a.(*net.IPNet); ok && n.IP.To4() != nil {
				nets = append(nets, &net.IPNet{IP: n.IP.To4(), Mask: n.Mask})
			}
		}
	}
	return nets, nil
}

func dialTCP(ctx context.Context, address string) bool {
	conn, err := (&net.Dialer{Timeout: relocateDial}).DialContext(ctx, "tcp", address)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// rtspAccepts sends DESCRIBE for the camera's main stream and reports whether
// the host serves that path to the camera's login.
func rtspAccepts(ctx context.Context, cam Camera) bool {
	u, err := url.Parse(cam.URL)
	if err != nil {
		return false
	}
	address := u.Host
	if u.Port() == "" {
		address = net.JoinHostPort(u.Hostname(), "554")
	}
	ctx, cancel := context.WithTimeout(ctx, relocateRTSP)
	defer cancel()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
	if err != nil {
		return false
	}
	defer conn.Close()
	if d, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(d)
	}
	r := textproto.NewReader(bufio.NewReader(conn))
	target := u.String()
	auth := ""
	for seq := 1; seq <= 2; seq++ {
		req := fmt.Sprintf("DESCRIBE %s RTSP/1.0\r\nCSeq: %d\r\nAccept: application/sdp\r\nUser-Agent: Dozor\r\n", target, seq)
		if auth != "" {
			req += "Authorization: " + auth + "\r\n"
		}
		if _, err = conn.Write([]byte(req + "\r\n")); err != nil {
			return false
		}
		status, headers, err := readRTSPResponse(r)
		if err != nil {
			return false
		}
		if status == 200 {
			return true
		}
		if status != 401 || seq == 2 || cam.Username == "" {
			return false
		}
		auth = ""
		for _, challenge := range headers.Values("Www-Authenticate") {
			if strings.HasPrefix(challenge, "Digest ") {
				auth, _ = digestAuth(challenge, "DESCRIBE", target, cam.Username, cam.Password)
				break
			}
			if strings.HasPrefix(challenge, "Basic ") {
				auth = "Basic " + base64.StdEncoding.EncodeToString([]byte(cam.Username+":"+cam.Password))
			}
		}
		if auth == "" {
			return false
		}
	}
	return false
}

func readRTSPResponse(r *textproto.Reader) (int, textproto.MIMEHeader, error) {
	line, err := r.ReadLine()
	if err != nil {
		return 0, nil, err
	}
	f := strings.Fields(line)
	if len(f) < 2 || !strings.HasPrefix(f[0], "RTSP/") {
		return 0, nil, fmt.Errorf("not RTSP")
	}
	status, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, nil, err
	}
	headers, err := r.ReadMIMEHeader()
	if err != nil {
		return 0, nil, err
	}
	if n, _ := strconv.Atoi(headers.Get("Content-Length")); n > 0 && n < 1<<20 {
		if _, err = r.R.Discard(n); err != nil {
			return 0, nil, err
		}
	}
	return status, headers, nil
}

func onvifSerial(ctx context.Context, cam Camera) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := soap(ctx, cam, cam.ONVIF, "http://www.onvif.org/ver10/device/wsdl/GetDeviceInformation", `<GetDeviceInformation xmlns="http://www.onvif.org/ver10/device/wsdl"/>`)
	if err != nil {
		return "", err
	}
	return n.text("SerialNumber"), nil
}
