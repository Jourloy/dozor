package dozor

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A camera on DHCP can come back at another address. Once a camera has been
// offline for a while, Dozor sweeps the Pi's subnets for hosts that accept the
// camera's RTSP path with its login and password, and moves the camera there.
// Hosts come from a TCP probe of the RTSP port and from ONVIF WS-Discovery
// replies. MAC addresses are not used: mesh nodes may bridge or proxy them.
// When several hosts accept the same login, the ONVIF serial number
// remembered while the camera was online tells them apart.
const (
	relocateAfter    = 2 * time.Minute
	relocateRetry    = 5 * time.Minute
	relocateNotice   = 6 * time.Hour
	relocateTick     = 30 * time.Second
	relocateDial     = 3 * time.Second // Wi-Fi cameras behind a mesh node answer slowly
	relocateGap      = 2 * time.Second // between the TCP passes: the first one wakes sleeping devices
	relocateDiscover = 4 * time.Second
	relocateRTSP     = 8 * time.Second
	relocateWorkers  = 64
	relocateChecks   = 16
	relocateMaxHosts = 1024
)

type CameraMove struct {
	ID, Name, From, To string
}

type dialOutcome uint8

const (
	dialSkipped dialOutcome = iota // never probed
	dialOpen
	dialRefused
	dialTimeout
	dialOther
)

func (o dialOutcome) String() string {
	return [...]string{"skipped", "open", "refused", "timeout", "unreachable"}[o]
}

func classifyDial(err error) dialOutcome {
	var ne net.Error
	switch {
	case err == nil:
		return dialOpen
	case errors.Is(err, syscall.ECONNREFUSED):
		return dialRefused
	case errors.As(err, &ne) && ne.Timeout():
		return dialTimeout
	}
	return dialOther
}

// rtspVerdict is the answer of a host to the DESCRIBE login check: the RTSP
// status when one arrived, otherwise the reason why none did.
type rtspVerdict struct {
	status int
	note   string
}

func (v rtspVerdict) ok() bool { return v.status == 200 }

func (v rtspVerdict) String() string {
	switch {
	case v.ok():
		return "accepted"
	case v.status > 0:
		return strconv.Itoa(v.status) + " " + v.note
	case v.note == "":
		return "not checked"
	}
	return v.note
}

func rtspReply(status int) rtspVerdict {
	v := rtspVerdict{status: status}
	switch status {
	case 200:
	case 401, 403:
		v.note = "auth rejected"
	case 404, 454:
		v.note = "wrong path"
	default:
		v.note = "unexpected reply"
	}
	return v
}

type relocator struct {
	path     string
	serials  map[string]string // camera ID -> ONVIF serial seen while online
	tried    map[string]time.Time
	asked    map[string]time.Time
	warned   map[string]bool      // reported as ambiguous in the current offline period
	notified map[string]time.Time // last "not found" notice of the current offline period
	undo     []func()             // takes back what the notices of the last step marked as reported
	gap      time.Duration        // pause between the two TCP passes
	subnets  func() ([]*net.IPNet, error)
	open     func(ctx context.Context, address string) dialOutcome
	accepts  func(ctx context.Context, cam Camera) rtspVerdict
	serial   func(ctx context.Context, cam Camera) (string, error)
	discover func(ctx context.Context, local net.IP) ([]string, error)
}

func newRelocator(path string) *relocator {
	l := &relocator{path: path, serials: map[string]string{}, tried: map[string]time.Time{}, asked: map[string]time.Time{}, warned: map[string]bool{}, notified: map[string]time.Time{}, gap: relocateGap, subnets: localSubnets, open: dialTCP, accepts: rtspAccepts, serial: onvifSerial, discover: discoverHosts}
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
		r := a.currentRuntime()
		if r == nil || r.mediaSince.Load() == 0 {
			continue
		}
		moves, notices := l.step(ctx, a.Config.Get().Cameras, r.streamSnapshot(), time.Now())
		// A step takes a minute or more. A Reload meanwhile closes the Store of the
		// runtime it began with, so the journal is the one of the runtime of now.
		if r = a.currentRuntime(); r == nil {
			l.retract(moves) // Nothing to write to: search again once the runtime is back.
			continue
		}
		for _, message := range notices {
			r.Store.Notice(message)
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

func (a *App) currentRuntime() *Runtime {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.runtime
}

// step returns the cameras to move and the journal notices to write. When the
// notices cannot be written, retract takes back that they were reported.
func (l *relocator) step(ctx context.Context, cameras []Camera, streams map[string]AvailabilityInterval, now time.Time) (moves []CameraMove, notices []string) {
	l.undo = nil
	nets, _ := l.subnets()
	// Configured cameras and the Pi itself are never candidates.
	taken := map[string]bool{}
	for _, o := range nets {
		taken[o.IP.String()] = true
	}
	live := map[string]bool{}
	for _, cam := range cameras {
		live[cam.ID] = cam.Enabled
		if ip := cameraIP(cam); ip != nil {
			taken[ip.String()] = true
		}
	}
	for id := range l.tried {
		if !live[id] {
			l.forget(id)
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
			l.forget(cam.ID)
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
	// The cameras lost together look in the same networks: a network is swept
	// once per step, each camera only checks the hosts with its own login.
	scans := map[string]*scan{}
	for _, cam := range lost {
		l.tried[cam.ID] = now
		old := cameraIP(cam).String()
		verdict := l.accepts(ctx, cam)
		if verdict.ok() {
			log.Printf("relocate: camera %q, old address %s: accepted, staying", cam.Name, old)
			continue // Still at its address: the stream fails for another reason.
		}
		foreign := map[string]bool{}
		for _, o := range cameras {
			if sn := l.serials[o.ID]; o.ID != cam.ID && sn != "" {
				foreign[sn] = true
			}
		}
		s := l.search(ctx, cam, old, verdict, nets, taken, foreign, scans)
		if ctx.Err() != nil {
			break // Shutting down: the picture is incomplete.
		}
		log.Print(s.summary(cam.Name, old))
		switch {
		case len(s.candidates) == 1:
			moves = append(moves, CameraMove{ID: cam.ID, Name: cam.Name, From: old, To: s.candidates[0]})
			taken[s.candidates[0]] = true
		case len(s.candidates) > 1 && !l.warned[cam.ID]:
			l.warned[cam.ID] = true
			l.undo = append(l.undo, func() { delete(l.warned, cam.ID) })
			notices = append(notices, fmt.Sprintf("Камера «%s» не отвечает по своему адресу, а её логин подходит к нескольким камерам в сети; укажите новый адрес вручную", cam.Name))
		case len(s.candidates) == 0 && l.noticeDue(cam.ID, now):
			notices = append(notices, s.failure(cam.Name, old))
		}
	}
	return moves, notices
}

// An offline period is what the relocator has seen since the camera was last
// online. The availability interval cannot mark it: its start slides along the
// 24 hour history window.
func (l *relocator) forget(id string) {
	delete(l.tried, id)
	delete(l.warned, id)
	delete(l.notified, id)
}

// The first failed search of an offline period is reported, then at most once
// per relocateNotice while the same period goes on.
func (l *relocator) noticeDue(id string, now time.Time) bool {
	at, had := l.notified[id]
	if had && now.Sub(at) < relocateNotice {
		return false
	}
	l.notified[id] = now
	l.undo = append(l.undo, func() {
		if had {
			l.notified[id] = at
		} else {
			delete(l.notified, id)
		}
	})
	return true
}

// retract is for a step whose notices and moves were not delivered: the
// notices count as unreported and the cameras are searched again at the next
// tick.
func (l *relocator) retract(moves []CameraMove) {
	for _, undo := range l.undo {
		undo()
	}
	l.undo = nil
	for _, m := range moves {
		delete(l.tried, m.ID)
	}
}

// hostNote is what one attempt learned about a host.
type hostNote struct {
	tcp   string // "tcp" or "tcp2" (second pass) when the RTSP port was open
	ws    bool   // answered WS-Discovery
	rtsp  rtspVerdict
	extra string // why an accepting host was still dropped
}

func (n *hostNote) String() string {
	via := []string{}
	if n.tcp != "" {
		via = append(via, n.tcp)
	}
	if n.ws {
		via = append(via, "ws")
	}
	s := "via " + strings.Join(via, "+") + ": " + n.rtsp.String()
	if n.extra != "" {
		s += ", " + n.extra
	}
	return s
}

// attempt is the outcome of one search for a lost camera.
type attempt struct {
	cidrs      []string // networks swept
	oldAddress rtspVerdict
	probed     int
	dial       map[dialOutcome]int
	second     int // hosts probed twice
	wsIn       int // WS-Discovery hosts inside the swept networks
	wsErr      error
	notes      map[string]*hostNote
	hosts      []string // open or discovered, sorted
	accepted   []string
	candidates []string
}

// scan is what one sweep of a network learned: the TCP probes of the RTSP port
// and the WS-Discovery answers. They depend on the network and the port, not on
// the camera, so the cameras of one step share them.
type scan struct {
	cidr   string
	probed int
	probes map[string]probe
	ws     []string
	wsErr  error
}

func (l *relocator) scan(ctx context.Context, n *net.IPNet, own []*net.IPNet, port string, taken map[string]bool, cache map[string]*scan) *scan {
	key := n.String() + "|" + port
	if sc := cache[key]; sc != nil {
		return sc
	}
	sc := &scan{cidr: n.String()}
	hosts := []string{}
	for _, h := range sweepHosts(n, own) {
		if !taken[h] {
			hosts = append(hosts, h)
		}
	}
	sc.probed = len(hosts)
	// Multicast leaves through the interface that owns the bound address.
	var local net.IP
	for _, o := range own {
		if n.Contains(o.IP) {
			local = o.IP
			break
		}
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		dctx, cancel := context.WithTimeout(ctx, relocateDiscover)
		defer cancel()
		sc.ws, sc.wsErr = l.discover(dctx, local)
	}()
	sc.probes = l.sweep(ctx, hosts, port)
	<-done
	if ctx.Err() == nil { // An interrupted sweep is no picture to reuse.
		cache[key] = sc
	}
	return sc
}

// read adds what the scan of n found to the attempt and returns the hosts it
// brought: those that were open or discovered and are not taken.
func (s *attempt) read(sc *scan, n *net.IPNet, taken map[string]bool) []string {
	s.cidrs = append(s.cidrs, sc.cidr)
	s.probed += sc.probed
	if s.wsErr == nil {
		s.wsErr = sc.wsErr
	}
	here := []string{}
	note := func(h string) *hostNote {
		if s.notes[h] == nil {
			s.notes[h] = &hostNote{}
			here = append(here, h)
		}
		return s.notes[h]
	}
	for h, p := range sc.probes {
		if taken[h] {
			continue
		}
		s.dial[p.out]++
		if p.again {
			s.second++
		}
		if p.out == dialOpen {
			note(h).tcp = "tcp"
			if p.again {
				note(h).tcp = "tcp2"
			}
		}
	}
	for _, h := range sc.ws {
		if ip := net.ParseIP(h).To4(); ip != nil && n.Contains(ip) && !taken[ip.String()] && !note(ip.String()).ws {
			note(ip.String()).ws = true
			s.wsIn++
		}
	}
	slices.Sort(here)
	s.hosts = append(s.hosts, here...)
	return here
}

// search looks for the camera in the networks of searchNets, one after the
// other, until one gives a candidate. foreign holds the ONVIF serials of the
// other configured cameras.
func (l *relocator) search(ctx context.Context, cam Camera, old string, oldAddress rtspVerdict, nets []*net.IPNet, taken, foreign map[string]bool, scans map[string]*scan) *attempt {
	s := &attempt{oldAddress: oldAddress, dial: map[dialOutcome]int{}, notes: map[string]*hostNote{}}
	for _, n := range searchNets(cameraIP(cam), nets) {
		here := s.read(l.scan(ctx, n, nets, rtspPort(cam), taken, scans), n, taken)
		forEach(ctx, here, relocateChecks, func(h string) {
			s.notes[h].rtsp = l.accepts(ctx, movedCamera(cam, old, h))
		})
		accepted := []string{}
		for _, h := range here {
			if s.notes[h].rtsp.ok() {
				accepted = append(accepted, h)
			}
		}
		s.accepted = append(s.accepted, accepted...)
		s.candidates = l.screen(ctx, s, accepted, cam, old, foreign)
		if len(s.candidates) > 0 || ctx.Err() != nil {
			break
		}
	}
	slices.Sort(s.hosts)
	return s
}

// screen keeps the accepting hosts that the ONVIF serial does not give away as
// another camera: a different serial than the remembered one, or the serial of
// another configured camera. A serial that cannot be read is no proof, so a
// lone accepting host stays; among several it cannot be told apart.
func (l *relocator) screen(ctx context.Context, s *attempt, accepted []string, cam Camera, old string, foreign map[string]bool) []string {
	want := l.serials[cam.ID]
	if len(accepted) == 0 || want == "" && len(foreign) == 0 {
		return accepted
	}
	candidates := []string{}
	for _, h := range accepted {
		got, err := "", errors.New("no ONVIF address")
		if cam.ONVIF != "" {
			got, err = l.serial(ctx, movedCamera(cam, old, h))
		}
		switch {
		case err != nil || got == "":
			if len(accepted) > 1 && want != "" {
				s.notes[h].extra = "serial unavailable"
				continue
			}
		case want != "" && got != want:
			s.notes[h].extra = "serial mismatch"
			continue
		case foreign[got]:
			s.notes[h].extra = "serial of another camera"
			continue
		}
		candidates = append(candidates, h)
	}
	return candidates
}

func (s *attempt) summary(name, old string) string {
	list := make([]string, 0, len(s.hosts))
	for _, h := range s.hosts {
		list = append(list, h+" "+s.notes[h].String())
	}
	ws := strconv.Itoa(s.wsIn)
	if s.wsErr != nil {
		ws += " (failed: " + s.wsErr.Error() + ")"
	}
	return fmt.Sprintf("relocate: camera %q, old address %s: %s; swept %s: probed %d, tcp open %d, refused %d, timeout %d, unreachable %d, second pass %d; ws %s; candidates %d; hosts: [%s]",
		name, old, s.oldAddress, strings.Join(s.cidrs, ", "), s.probed, s.dial[dialOpen], s.dial[dialRefused], s.dial[dialTimeout], s.dial[dialOther], s.second, ws, len(s.candidates), strings.Join(list, "; "))
}

// refusals tells what kept the hosts that were looked at from taking the
// camera's login, 401 and 403 being the login, 404 and 454 the path, and
// everything else, from a timeout to a closed connection, no video at all.
func (s *attempt) refusals() (login, path, silent int) {
	for _, h := range s.hosts {
		switch v := s.notes[h].rtsp; {
		case v.status == 401 || v.status == 403:
			login++
		case v.status == 404 || v.status == 454:
			path++
		default:
			silent++
		}
	}
	return
}

// failure is the journal text for a search that found no candidate.
func (s *attempt) failure(name, old string) string {
	var why string
	switch {
	case len(s.hosts) == 0:
		why = "ни один адрес не ответил на порту RTSP"
	case len(s.accepted) == 1:
		why = "логин камеры подошёл одному устройству, но серийный номер ONVIF показывает, что это другая камера"
	case len(s.accepted) > 1:
		why = "логин камеры подошёл нескольким устройствам, но серийный номер ONVIF у каждого не совпал или не читается"
	default:
		lead := "открыт порт RTSP"
		if s.dial[dialOpen] < len(s.hosts) {
			lead = "отвечает RTSP или ONVIF"
		}
		login, path, silent := s.refusals()
		one := len(s.hosts) == 1
		// A tie says only what is true of every host: no video was given.
		var cause string
		switch {
		case login > path && login > silent && one:
			cause = "логин камеры не подошёл"
		case login > path && login > silent:
			cause = "логин камеры не подошёл ни к одному"
		case path > login && path > silent && one:
			cause = "путь к видеопотоку камеры не подошёл"
		case path > login && path > silent:
			cause = "путь к видеопотоку камеры не подошёл ни к одному"
		case one:
			cause = "устройство не отдало видеопоток камеры"
		default:
			cause = "ни одно устройство не отдало видеопоток камеры"
		}
		why = fmt.Sprintf("на %s %s, %s", addresses(len(s.hosts)), lead, cause)
	}
	where := "сети "
	if len(s.cidrs) > 1 {
		where = "сетях "
	}
	// The minute form is right for 5 to 20.
	return fmt.Sprintf("Камера «%s» не отвечает по адресу %s. Поиск в %s%s: %s. Поиск повторяется каждые %d минут", name, old, where, strings.Join(s.cidrs, ", "), why, int(relocateRetry/time.Minute))
}

// addresses is "N адресе" or "N адресах" (prepositional case).
func addresses(n int) string {
	if n%10 == 1 && n%100 != 11 {
		return fmt.Sprintf("%d адресе", n)
	}
	return fmt.Sprintf("%d адресах", n)
}

type probe struct {
	out   dialOutcome
	again bool // probed a second time
}

// Dials the RTSP port of every host. Hosts that gave no TCP answer at all, not
// even a refusal, get one more probe after a pause once the first pass is over.
// A silent Wi-Fi device may time out or fail with "no route" by the same ARP
// retries, so both count as silent.
func (l *relocator) sweep(ctx context.Context, hosts []string, port string) map[string]probe {
	got := map[string]probe{}
	var mu sync.Mutex
	pass := func(hs []string, again bool) {
		forEach(ctx, hs, relocateWorkers, func(h string) {
			o := l.open(ctx, net.JoinHostPort(h, port))
			mu.Lock()
			got[h] = probe{o, again}
			mu.Unlock()
		})
	}
	pass(hosts, false)
	silent := []string{}
	for _, h := range hosts {
		if o := got[h].out; o == dialTimeout || o == dialOther {
			silent = append(silent, h)
		}
	}
	if len(silent) > 0 && pause(ctx, l.gap) {
		pass(silent, true)
	}
	return got
}

func forEach(ctx context.Context, items []string, workers int, fn func(string)) {
	jobs := make(chan string)
	var wg sync.WaitGroup
	for range min(workers, len(items)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for it := range jobs {
				fn(it)
			}
		}()
	}
	for _, it := range items {
		if ctx.Err() != nil {
			break
		}
		jobs <- it
	}
	close(jobs)
	wg.Wait()
}

// The hosts WS-Discovery replies point at: the UDP source and the XAddrs.
func wsHosts(replies []wsReply) []string {
	hosts := []string{}
	add := func(ip net.IP) {
		if ip = ip.To4(); ip != nil && !slices.Contains(hosts, ip.String()) {
			hosts = append(hosts, ip.String())
		}
	}
	for _, r := range replies {
		add(r.from)
		for _, m := range r.matches {
			if u, err := url.Parse(m.Endpoint); err == nil {
				add(net.ParseIP(u.Hostname()))
			}
		}
	}
	return hosts
}

func discoverHosts(ctx context.Context, local net.IP) ([]string, error) {
	replies, err := wsProbe(ctx, wsGroup, local, relocateDiscover)
	return wsHosts(replies), err
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
func sweepNet(ip net.IP, own []*net.IPNet) *net.IPNet {
	n := &net.IPNet{IP: ip.Mask(net.CIDRMask(24, 32)), Mask: net.CIDRMask(24, 32)}
	for _, o := range own {
		if ones, bits := o.Mask.Size(); bits == 32 && ones <= 30 && o.Contains(ip) && 1<<(bits-ones) <= relocateMaxHosts {
			n = &net.IPNet{IP: o.IP.Mask(o.Mask), Mask: o.Mask}
		}
	}
	return n
}

// The networks one search sweeps, in order, until one gives a candidate. The
// camera may have moved to another subnet of the Pi, and a refusal at its old
// address does not prove that the old address is local: it may come from a
// gateway. So the Pi's own private subnets are searched too, together up to
// relocateMaxHosts addresses. The network of the old address goes first when
// the Pi is in it; when it is not, that network is reached through the gateway
// and goes last.
func searchNets(ip net.IP, own []*net.IPNet) []*net.IPNet {
	old := sweepNet(ip, own)
	nets := []*net.IPNet{}
	budget := relocateMaxHosts
	for _, o := range own {
		ones, bits := o.Mask.Size()
		if bits != 32 || ones > 30 || !o.IP.IsPrivate() {
			continue
		}
		if 1<<(bits-ones) > relocateMaxHosts {
			ones = 24
		}
		mask := net.CIDRMask(ones, bits)
		n, size := &net.IPNet{IP: o.IP.Mask(mask), Mask: mask}, 1<<(bits-ones)
		if overlaps(n, old) || slices.ContainsFunc(nets, func(m *net.IPNet) bool { return overlaps(n, m) }) || size > budget {
			continue
		}
		budget -= size
		nets = append(nets, n)
	}
	if slices.ContainsFunc(own, func(o *net.IPNet) bool { return o.Contains(ip) }) {
		return append([]*net.IPNet{old}, nets...)
	}
	return append(nets, old)
}

func overlaps(a, b *net.IPNet) bool { return a.Contains(b.IP) || b.Contains(a.IP) }

// The hosts of n without the network, broadcast and Pi's own addresses.
func sweepHosts(n *net.IPNet, own []*net.IPNet) []string {
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

func dialTCP(ctx context.Context, address string) dialOutcome {
	conn, err := (&net.Dialer{Timeout: relocateDial}).DialContext(ctx, "tcp", address)
	if err == nil {
		conn.Close()
	}
	return classifyDial(err)
}

// rtspAccepts sends DESCRIBE for the camera's main stream and reports whether
// the host serves that path to the camera's login, or why it does not.
//
// Every address with an open RTSP port gets this check every few minutes, the
// ones of other people's cameras too, and some cameras lock out after a few
// wrong logins. So one login reaches a host. It answers the challenge of the
// connection it is sent on. A camera that closes the connection after a 401,
// with or without saying so, may tie the nonce to the connection: the new
// connection then asks for a challenge of its own first, once. The login is
// sent again only when the camera calls the nonce stale, or when the refused
// login was sent on another connection than the one that issued its challenge.
func rtspAccepts(ctx context.Context, cam Camera) rtspVerdict {
	u, err := url.Parse(cam.URL)
	if err != nil {
		return rtspVerdict{note: "bad URL"}
	}
	address := u.Host
	if u.Port() == "" {
		address = net.JoinHostPort(u.Hostname(), "554")
	}
	u.User = nil
	target := u.String()
	ctx, cancel := context.WithTimeout(ctx, relocateRTSP)
	defer cancel()
	var conn net.Conn
	var r *textproto.Reader
	var unwatch func() bool
	drop := func() {
		if conn != nil {
			unwatch()
			conn.Close()
			conn = nil
		}
	}
	defer drop()
	connect := func() error {
		drop()
		c, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
		if err != nil {
			return err
		}
		if d, ok := ctx.Deadline(); ok {
			_ = c.SetDeadline(d)
		}
		// A cancelled context must not wait for the read deadline.
		unwatch = context.AfterFunc(ctx, func() { _ = c.SetDeadline(time.Now()) })
		conn, r = c, textproto.NewReader(bufio.NewReader(c))
		return nil
	}
	seq := 0
	describe := func(auth string) (int, textproto.MIMEHeader, error) {
		seq++
		req := fmt.Sprintf("DESCRIBE %s RTSP/1.0\r\nCSeq: %d\r\nAccept: application/sdp\r\nUser-Agent: Dozor\r\n", target, seq)
		if auth != "" {
			req += "Authorization: " + auth + "\r\n"
		}
		if _, err := conn.Write([]byte(req + "\r\n")); err != nil {
			return 0, nil, err
		}
		return readRTSPResponse(r)
	}
	closes := func(h textproto.MIMEHeader) bool {
		return strings.Contains(strings.ToLower(h.Get("Connection")), "close")
	}
	if err := connect(); err != nil {
		return rtspVerdict{note: classifyDial(err).String()}
	}
	status, headers, err := describe("")
	if err != nil {
		return rtspFailure(err)
	}
	if status != 401 || cam.Username == "" {
		return rtspReply(status)
	}
	challenge, live := headers.Values("Www-Authenticate"), !closes(headers)
	own := true      // the challenge came from the connection the login goes to
	reused := true   // the connection has carried a reply already
	asked := false   // a new connection has asked for a challenge of its own
	retried := false // a refused login has been sent again
	for {
		if !live {
			if err := connect(); err != nil {
				return rtspVerdict{status: 401, note: "reconnect " + classifyDial(err).String()}
			}
			live, reused = true, false
			if !asked {
				asked = true
				if status, headers, err = describe(""); err != nil {
					return rtspFailure(err)
				}
				if status != 401 {
					return rtspReply(status)
				}
				challenge, live, own, reused = headers.Values("Www-Authenticate"), !closes(headers), true, true
				continue
			}
			own = false
		}
		auth := rtspAuthorization(challenge, cam, target)
		if auth == "" {
			return rtspVerdict{status: 401, note: "no supported login method"}
		}
		status, headers, err = describe(auth)
		if err != nil {
			// A connection that fails before any reply, after it had carried one, was
			// closed by the camera: the login never reached it.
			var ne net.Error
			timeout := errors.As(err, &ne) && ne.Timeout()
			if reused && !timeout && !errors.Is(err, errNotRTSP) {
				live = false
				continue
			}
			return rtspFailure(err)
		}
		next := headers.Values("Www-Authenticate")
		if status == 401 && !retried && len(next) > 0 && (staleNonce(next) || !own && !slices.Equal(next, challenge)) {
			retried = true
			challenge, live, own, reused = next, !closes(headers), true, true
			continue
		}
		return rtspReply(status)
	}
}

// The Digest challenge says the nonce is out of date, not the login wrong.
func staleNonce(challenges []string) bool {
	for _, c := range challenges {
		if strings.Contains(strings.NewReplacer(" ", "", `"`, "").Replace(strings.ToLower(c)), "stale=true") {
			return true
		}
	}
	return false
}

// Digest wins over Basic; scheme names are case-insensitive.
func rtspAuthorization(challenges []string, cam Camera, uri string) string {
	basic := ""
	for _, c := range challenges {
		scheme, rest, _ := strings.Cut(strings.TrimSpace(c), " ")
		switch strings.ToLower(scheme) {
		case "digest":
			if a, err := digestAuth("Digest "+rest, "DESCRIBE", uri, cam.Username, cam.Password); err == nil {
				return a
			}
		case "basic":
			basic = "Basic " + base64.StdEncoding.EncodeToString([]byte(cam.Username+":"+cam.Password))
		}
	}
	return basic
}

func rtspFailure(err error) rtspVerdict {
	var ne net.Error
	switch {
	case errors.Is(err, errNotRTSP):
		return rtspVerdict{note: "not RTSP"}
	case errors.As(err, &ne) && ne.Timeout():
		return rtspVerdict{note: "timeout"}
	}
	return rtspVerdict{note: "connection closed"}
}

var errNotRTSP = errors.New("not RTSP")

func readRTSPResponse(r *textproto.Reader) (int, textproto.MIMEHeader, error) {
	line, err := r.ReadLine()
	if err != nil {
		return 0, nil, err
	}
	f := strings.Fields(line)
	if len(f) < 2 || !strings.HasPrefix(f[0], "RTSP/") {
		return 0, nil, errNotRTSP
	}
	status, err := strconv.Atoi(f[1])
	if err != nil {
		return 0, nil, errNotRTSP
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
