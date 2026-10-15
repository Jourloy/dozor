package dozor

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A camera on DHCP can come back at another address. While a camera is online
// Dozor remembers the MAC behind its IP; once it has been offline for a while,
// Dozor sweeps the Pi's own subnet and moves the camera to the IP that now
// carries that MAC. Only cameras configured by an IP on a directly connected
// subnet take part: behind a router the ARP table shows the router's MAC.
const (
	relocateAfter    = 2 * time.Minute
	relocateRetry    = 5 * time.Minute
	relocateTick     = 30 * time.Second
	relocateDial     = 800 * time.Millisecond
	relocateWorkers  = 64
	relocateMaxHosts = 1024
)

type CameraMove struct {
	ID, Name, From, To string
}

type relocator struct {
	path    string
	macs    map[string]string // camera ID -> MAC seen while it was online
	tried   map[string]time.Time
	arp     func() (map[string]string, error)
	subnets func() ([]*net.IPNet, error)
	dial    func(ctx context.Context, address string) bool
}

func newRelocator(path string) *relocator {
	l := &relocator{path: path, macs: map[string]string{}, tried: map[string]time.Time{}, arp: readARP, subnets: localSubnets, dial: dialTCP}
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, &l.macs)
	}
	return l
}

func (a *App) runRelocation(ctx context.Context) {
	if a.Development {
		return
	}
	l := newRelocator(filepath.Join(a.StateDir, "camera-macs.json"))
	for pause(ctx, relocateTick) {
		a.mu.RLock()
		r := a.runtime
		a.mu.RUnlock()
		if r == nil || r.mediaSince.Load() == 0 {
			continue
		}
		moves := l.step(ctx, a.Config.Get().Cameras, r.streamSnapshot(), time.Now())
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

func (l *relocator) step(ctx context.Context, cameras []Camera, streams map[string]AvailabilityInterval, now time.Time) []CameraMove {
	table, err := l.arp()
	if err != nil {
		return nil
	}
	nets, err := l.subnets()
	if err != nil {
		return nil
	}
	learned := false
	lost := []Camera{}
	for _, cam := range cameras {
		ip := cameraIP(cam)
		state, ok := streams[cam.ID]
		if !cam.Enabled || ip == nil || !ok || subnetOf(nets, ip) == nil {
			continue
		}
		switch {
		case state.State == "online":
			delete(l.tried, cam.ID)
			if mac := table[ip.String()]; mac != "" && l.macs[cam.ID] != mac {
				l.macs[cam.ID] = mac
				learned = true
			}
		case state.State == "offline" && l.macs[cam.ID] != "" &&
			now.Sub(time.UnixMilli(state.Start)) >= relocateAfter && now.Sub(l.tried[cam.ID]) >= relocateRetry:
			lost = append(lost, cam)
		}
	}
	if learned {
		l.save(cameras)
	}
	if len(lost) == 0 {
		return nil
	}
	hosts := map[string]bool{}
	for _, cam := range lost {
		l.tried[cam.ID] = now
		for _, h := range sweepHosts(subnetOf(nets, cameraIP(cam)), cameraIP(cam), nets) {
			hosts[h] = true
		}
	}
	answered := map[string]bool{}
	ports := map[string]bool{}
	for _, cam := range lost {
		ports[rtspPort(cam)] = true
	}
	for port := range ports {
		for h := range l.sweep(ctx, hosts, port) {
			answered[h] = true
		}
	}
	if table, err = l.arp(); err != nil {
		return nil
	}
	moves := []CameraMove{}
	for _, cam := range lost {
		mac, old := l.macs[cam.ID], cameraIP(cam).String()
		if table[old] == mac || sharedMAC(l.macs, cam.ID) {
			continue // Still at its address (a stream fault), or MAC is ambiguous.
		}
		candidates, open := []string{}, []string{}
		for ip, m := range table {
			if m == mac && ip != old {
				candidates = append(candidates, ip)
				if answered[ip] {
					open = append(open, ip)
				}
			}
		}
		if len(open) > 0 {
			candidates = open
		}
		if len(candidates) == 1 {
			moves = append(moves, CameraMove{ID: cam.ID, Name: cam.Name, From: old, To: candidates[0]})
		}
	}
	return moves
}

// Connecting to every host makes the kernel resolve its MAC, even when the
// port is closed. Hosts that accept the RTSP port are reported back.
func (l *relocator) sweep(ctx context.Context, hosts map[string]bool, port string) map[string]bool {
	jobs := make(chan string)
	var mu sync.Mutex
	answered := map[string]bool{}
	var wg sync.WaitGroup
	for range relocateWorkers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for h := range jobs {
				if l.dial(ctx, net.JoinHostPort(h, port)) {
					mu.Lock()
					answered[h] = true
					mu.Unlock()
				}
			}
		}()
	}
	for h := range hosts {
		if ctx.Err() != nil {
			break
		}
		jobs <- h
	}
	close(jobs)
	wg.Wait()
	return answered
}

func (l *relocator) save(cameras []Camera) {
	known := map[string]bool{}
	for _, cam := range cameras {
		known[cam.ID] = true
	}
	for id := range l.macs {
		if !known[id] {
			delete(l.macs, id)
		}
	}
	_ = WriteJSON(l.path, l.macs)
}

func sharedMAC(macs map[string]string, id string) bool {
	for other, mac := range macs {
		if other != id && mac == macs[id] {
			return true
		}
	}
	return false
}

func applyCameraMoves(c *Config, moves []CameraMove) []CameraMove {
	applied := []CameraMove{}
	for _, m := range moves {
		for i := range c.Cameras {
			cam := &c.Cameras[i]
			// The user may have edited the camera while the sweep ran.
			if cam.ID != m.ID || cameraIP(*cam) == nil || cameraIP(*cam).String() != m.From {
				continue
			}
			cam.URL = replaceHost(cam.URL, m.From, m.To)
			cam.SubURL = replaceHost(cam.SubURL, m.From, m.To)
			cam.ONVIF = replaceHost(cam.ONVIF, m.From, m.To)
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

func subnetOf(nets []*net.IPNet, ip net.IP) *net.IPNet {
	for _, n := range nets {
		if n.Contains(ip) {
			return n
		}
	}
	return nil
}

// Hosts of the camera's subnet, without the network, broadcast and Pi's own
// addresses. A subnet wider than /22 is narrowed to the camera's /24.
func sweepHosts(n *net.IPNet, ip net.IP, own []*net.IPNet) []string {
	if n == nil {
		return nil
	}
	ones, bits := n.Mask.Size()
	if bits != 32 || ones > 30 {
		return nil
	}
	if 1<<(bits-ones) > relocateMaxHosts {
		n = &net.IPNet{IP: ip.Mask(net.CIDRMask(24, 32)), Mask: net.CIDRMask(24, 32)}
		ones = 24
	}
	self := map[string]bool{}
	for _, o := range own {
		self[o.IP.String()] = true
	}
	base := binary.BigEndian.Uint32(n.IP.To4().Mask(n.Mask))
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

func readARP() (map[string]string, error) {
	f, err := os.Open("/proc/net/arp")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return parseARP(f)
}

// Only complete entries count: a host that never answered stays as zeros.
func parseARP(r io.Reader) (map[string]string, error) {
	table := map[string]string{}
	s := bufio.NewScanner(r)
	s.Scan()
	for s.Scan() {
		f := strings.Fields(s.Text())
		if len(f) < 4 {
			continue
		}
		flags, err := strconv.ParseUint(strings.TrimPrefix(f[2], "0x"), 16, 32)
		mac := strings.ToLower(f[3])
		if err != nil || flags&0x2 == 0 || mac == "00:00:00:00:00:00" {
			continue
		}
		table[f[0]] = mac
	}
	return table, s.Err()
}

func dialTCP(ctx context.Context, address string) bool {
	conn, err := (&net.Dialer{Timeout: relocateDial}).DialContext(ctx, "tcp", address)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
