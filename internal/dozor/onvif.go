package dozor

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"
)

type xmlNode struct {
	Name     xml.Name
	Attr     []xml.Attr
	Text     string
	Children []*xmlNode
}

func parseXML(b []byte) (*xmlNode, error) {
	d := xml.NewDecoder(bytes.NewReader(b))
	root := &xmlNode{}
	stack := []*xmlNode{root}
	for {
		t, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, e
		}
		switch v := t.(type) {
		case xml.StartElement:
			n := &xmlNode{Name: v.Name, Attr: v.Attr}
			p := stack[len(stack)-1]
			p.Children = append(p.Children, n)
			stack = append(stack, n)
		case xml.EndElement:
			if len(stack) < 2 {
				return nil, errors.New("XML mismatch")
			}
			stack = stack[:len(stack)-1]
		case xml.CharData:
			stack[len(stack)-1].Text += string(v)
		}
	}
	return root, nil
}
func (n *xmlNode) find(name string) []*xmlNode {
	v := []*xmlNode{}
	if n.Name.Local == name {
		v = append(v, n)
	}
	for _, c := range n.Children {
		v = append(v, c.find(name)...)
	}
	return v
}
func (n *xmlNode) text(name string) string {
	v := n.find(name)
	if len(v) > 0 {
		return strings.TrimSpace(v[0].Text)
	}
	return ""
}
func (n *xmlNode) attr(name string) string {
	for _, a := range n.Attr {
		if a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}
func esc(s string) string { var b bytes.Buffer; _ = xml.EscapeText(&b, []byte(s)); return b.String() }
func uuidURN() string {
	s := ID()
	return "urn:uuid:" + s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}
func wsse(c Camera) string {
	nonce := make([]byte, 20)
	_, _ = rand.Read(nonce)
	created := time.Now().UTC().Format(time.RFC3339Nano)
	h := sha1.New()
	h.Write(nonce)
	h.Write([]byte(created))
	h.Write([]byte(c.Password))
	return `<wsse:Security xmlns:wsse="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-secext-1.0.xsd"><wsse:UsernameToken><wsse:Username>` + esc(c.Username) + `</wsse:Username><wsse:Password Type="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-username-token-profile-1.0#PasswordDigest">` + base64.StdEncoding.EncodeToString(h.Sum(nil)) + `</wsse:Password><wsse:Nonce EncodingType="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-soap-message-security-1.0#Base64Binary">` + base64.StdEncoding.EncodeToString(nonce) + `</wsse:Nonce><wsu:Created xmlns:wsu="http://docs.oasis-open.org/wss/2004/01/oasis-200401-wss-wssecurity-utility-1.0.xsd">` + created + `</wsu:Created></wsse:UsernameToken></wsse:Security>`
}

var digestFields = regexp.MustCompile(`(\w+)=(?:"([^"]*)"|([^,\s]+))`)

func digestAuth(challenge, method, uri, user, password string) (string, error) {
	if !strings.HasPrefix(challenge, "Digest ") {
		return "", errors.New("unsupported camera authentication")
	}
	v := map[string]string{}
	for _, m := range digestFields.FindAllStringSubmatch(challenge, -1) {
		v[m[1]] = m[2]
		if v[m[1]] == "" {
			v[m[1]] = m[3]
		}
	}
	alg := v["algorithm"]
	if alg == "" {
		alg = "MD5"
	}
	if alg != "MD5" && alg != "SHA-256" {
		return "", errors.New("unsupported digest algorithm")
	}
	hash := func(s string) string {
		if alg == "SHA-256" {
			h := sha256.Sum256([]byte(s))
			return hex.EncodeToString(h[:])
		}
		h := md5.Sum([]byte(s))
		return hex.EncodeToString(h[:])
	}
	a1 := hash(user + ":" + v["realm"] + ":" + password)
	a2 := hash(method + ":" + uri)
	cn := ID()
	response := hash(a1 + ":" + v["nonce"] + ":" + a2)
	qop := ""
	for _, x := range strings.Split(v["qop"], ",") {
		if strings.TrimSpace(x) == "auth" {
			qop = "auth"
		}
	}
	if v["qop"] != "" && qop == "" {
		return "", errors.New("unsupported digest qop")
	}
	if qop != "" {
		response = hash(a1 + ":" + v["nonce"] + ":00000001:" + cn + ":auth:" + a2)
	}
	auth := fmt.Sprintf(`Digest username=%q, realm=%q, nonce=%q, uri=%q, response=%q, algorithm=%s`, user, v["realm"], v["nonce"], uri, response, alg)
	if qop != "" {
		auth += fmt.Sprintf(`, qop=auth, nc=00000001, cnonce=%q`, cn)
	}
	if v["opaque"] != "" {
		auth += fmt.Sprintf(`, opaque=%q`, v["opaque"])
	}
	return auth, nil
}
func soap(ctx context.Context, c Camera, endpoint, action, body string) (*xmlNode, error) {
	original, e := url.Parse(c.ONVIF)
	if e != nil {
		return nil, e
	}
	u, e := url.Parse(endpoint)
	if e != nil || u.Hostname() != original.Hostname() || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, errors.New("ONVIF service changed camera host")
	}
	payload := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://www.w3.org/2005/08/addressing"><s:Header><a:Action s:mustUnderstand="true">` + esc(action) + `</a:Action><a:To s:mustUnderstand="true">` + esc(endpoint) + `</a:To><a:MessageID>` + uuidURN() + `</a:MessageID>` + wsse(c) + `</s:Header><s:Body>` + body + `</s:Body></s:Envelope>`
	client := &http.Client{Timeout: 18 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	auth := ""
	for attempt := 0; attempt < 2; attempt++ {
		req, e := http.NewRequestWithContext(ctx, "POST", endpoint, strings.NewReader(payload))
		if e != nil {
			return nil, e
		}
		req.Header.Set("Content-Type", `application/soap+xml; charset=utf-8; action="`+action+`"`)
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		res, e := client.Do(req)
		if e != nil {
			return nil, &cameraToolError{message: "ONVIF camera unavailable: " + cameraDiagnostic(e.Error(), CameraURL(c, false)), cause: e}
		}
		b, e := io.ReadAll(io.LimitReader(res.Body, 2<<20))
		res.Body.Close()
		if e != nil {
			return nil, e
		}
		if res.StatusCode == 401 && attempt == 0 {
			auth, e = digestAuth(res.Header.Get("WWW-Authenticate"), "POST", req.URL.RequestURI(), c.Username, c.Password)
			if e != nil {
				return nil, fmt.Errorf("ONVIF: HTTP 401 Unauthorized: %w", e)
			}
			continue
		}
		if res.StatusCode != 200 {
			return nil, fmt.Errorf("ONVIF: HTTP %d %s", res.StatusCode, http.StatusText(res.StatusCode))
		}
		n, e := parseXML(b)
		if e != nil {
			return nil, e
		}
		if len(n.find("Fault")) > 0 {
			return nil, errors.New("ONVIF operation unsupported")
		}
		return n, nil
	}
	return nil, errors.New("ONVIF authentication failed")
}

type Discovery struct {
	Endpoint string `json:"endpoint"`
	Scopes   string `json:"scopes"`
}

var wsGroup = &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 3702}

// wsReply is one WS-Discovery answer: the UDP source and every XAddr it lists.
type wsReply struct {
	from    net.IP
	matches []Discovery
}

// wsProbe multicasts a Probe from the local address (any when nil) and collects
// the ProbeMatch replies until the wait or the context ends. Multicast is lossy,
// above all over Wi-Fi, so the Probe is sent again at 3/8 and 3/4 of the wait,
// each time with a new MessageID.
func wsProbe(ctx context.Context, group *net.UDPAddr, local net.IP, wait time.Duration) ([]wsReply, error) {
	if local == nil {
		local = net.IPv4zero
	}
	conn, e := net.ListenUDP("udp4", &net.UDPAddr{IP: local})
	if e != nil {
		return nil, e
	}
	defer conn.Close()
	// Closing wakes the reader at once, whatever deadline it waits for.
	defer context.AfterFunc(ctx, func() { conn.Close() })()
	probe := func() error {
		msg := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery" xmlns:dn="http://www.onvif.org/ver10/network/wsdl"><s:Header><a:MessageID>` + uuidURN() + `</a:MessageID><a:To s:mustUnderstand="true">urn:schemas-xmlsoap-org:ws:2005:04:discovery</a:To><a:Action s:mustUnderstand="true">http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action></s:Header><s:Body><d:Probe><d:Types>dn:NetworkVideoTransmitter</d:Types></d:Probe></s:Body></s:Envelope>`
		_, e := conn.WriteToUDP([]byte(msg), group)
		return e
	}
	if e = probe(); e != nil {
		return nil, e
	}
	start := time.Now()
	deadline := start.Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	resend := []time.Time{start.Add(wait * 3 / 8), start.Add(wait * 3 / 4)}
	replies := []wsReply{}
	buf := make([]byte, 65535)
	for {
		if len(resend) > 0 && !time.Now().Before(resend[0]) {
			_ = probe()
			resend = resend[1:]
		}
		until := deadline
		if len(resend) > 0 && resend[0].Before(until) {
			until = resend[0]
		}
		_ = conn.SetReadDeadline(until)
		n, from, e := conn.ReadFromUDP(buf)
		if e != nil {
			if errors.Is(e, os.ErrDeadlineExceeded) && time.Now().Before(deadline) {
				continue // Time for the next Probe.
			}
			break
		}
		tree, e := parseXML(buf[:n])
		if e != nil {
			continue
		}
		r := wsReply{from: from.IP}
		matched := false
		for _, p := range tree.find("ProbeMatch") {
			matched = true
			for _, ep := range strings.Fields(p.text("XAddrs")) {
				r.matches = append(r.matches, Discovery{ep, p.text("Scopes")})
			}
		}
		if matched {
			replies = append(replies, r)
		}
	}
	return replies, nil
}

func Discover(ctx context.Context) ([]Discovery, error) {
	replies, e := wsProbe(ctx, wsGroup, nil, 4*time.Second)
	if e != nil {
		return nil, e
	}
	results := []Discovery{}
	seen := map[string]bool{}
	for _, r := range replies {
		for _, m := range r.matches {
			u, e := url.Parse(m.Endpoint)
			if e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && !seen[m.Endpoint] {
				seen[m.Endpoint] = true
				results = append(results, m)
			}
		}
	}
	return results, nil
}

type Profile struct {
	Token string `json:"token"`
	Name  string `json:"name"`
	URL   string `json:"url"`
}

func services(ctx context.Context, c Camera) (media, events string, err error) {
	n, e := soap(ctx, c, c.ONVIF, "http://www.onvif.org/ver10/device/wsdl/GetCapabilities", `<GetCapabilities xmlns="http://www.onvif.org/ver10/device/wsdl"><Category>All</Category></GetCapabilities>`)
	if e != nil {
		err = e
		return
	}
	if v := n.find("Media"); len(v) > 0 {
		media = v[0].text("XAddr")
	}
	if v := n.find("Events"); len(v) > 0 {
		events = v[0].text("XAddr")
	}
	return
}
func Profiles(ctx context.Context, c Camera) ([]Profile, error) {
	m, _, e := services(ctx, c)
	if e != nil {
		return nil, e
	}
	if m == "" {
		return nil, errors.New("камера не объявляет ONVIF Media")
	}
	n, e := soap(ctx, c, m, "http://www.onvif.org/ver10/media/wsdl/GetProfiles", `<GetProfiles xmlns="http://www.onvif.org/ver10/media/wsdl"/>`)
	if e != nil {
		return nil, e
	}
	v := []Profile{}
	for _, p := range n.find("Profiles") {
		token := p.attr("token")
		r, e := soap(ctx, c, m, "http://www.onvif.org/ver10/media/wsdl/GetStreamUri", `<GetStreamUri xmlns="http://www.onvif.org/ver10/media/wsdl"><StreamSetup><Stream xmlns="http://www.onvif.org/ver10/schema">RTP-Unicast</Stream><Transport xmlns="http://www.onvif.org/ver10/schema"><Protocol>RTSP</Protocol></Transport></StreamSetup><ProfileToken>`+esc(token)+`</ProfileToken></GetStreamUri>`)
		if e != nil {
			continue
		}
		u, e := url.Parse(r.text("Uri"))
		if e != nil || u.Scheme != "rtsp" {
			continue
		}
		u.User = nil
		v = append(v, Profile{token, p.text("Name"), u.String()})
	}
	return v, nil
}

type ONVIFMotion struct{}

func (ONVIFMotion) Run(ctx context.Context, c Camera, emit func(MotionSignal)) error {
	_, ep, e := services(ctx, c)
	if e != nil {
		return e
	}
	if ep == "" {
		return errors.New("no event service")
	}
	n, e := soap(ctx, c, ep, "http://www.onvif.org/ver10/events/wsdl/EventPortType/CreatePullPointSubscriptionRequest", `<CreatePullPointSubscription xmlns="http://www.onvif.org/ver10/events/wsdl"><InitialTerminationTime>PT60S</InitialTerminationTime></CreatePullPointSubscription>`)
	if e != nil {
		return e
	}
	sub := n.text("Address")
	if sub == "" {
		return errors.New("no pull point")
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = soap(cleanup, c, sub, "http://docs.oasis-open.org/wsn/bw-2/SubscriptionManager/UnsubscribeRequest", `<Unsubscribe xmlns="http://docs.oasis-open.org/wsn/b-2"/>`)
	}()
	_, _ = soap(ctx, c, sub, "http://www.onvif.org/ver10/events/wsdl/PullPointSubscription/SetSynchronizationPointRequest", `<SetSynchronizationPoint xmlns="http://www.onvif.org/ver10/events/wsdl"/>`)
	renewed := time.Now()
	started := time.Now()
	verified := false
	active := false
	for ctx.Err() == nil {
		if time.Since(renewed) > 35*time.Second {
			_, e = soap(ctx, c, sub, "http://docs.oasis-open.org/wsn/bw-2/SubscriptionManager/RenewRequest", `<Renew xmlns="http://docs.oasis-open.org/wsn/b-2"><TerminationTime>PT60S</TerminationTime></Renew>`)
			if e != nil {
				return e
			}
			renewed = time.Now()
		}
		n, e = soap(ctx, c, sub, "http://www.onvif.org/ver10/events/wsdl/PullPointSubscription/PullMessagesRequest", `<PullMessages xmlns="http://www.onvif.org/ver10/events/wsdl"><Timeout>PT5S</Timeout><MessageLimit>100</MessageLimit></PullMessages>`)
		if e != nil {
			return e
		}
		for _, msg := range n.find("NotificationMessage") {
			if !strings.Contains(strings.ToLower(msg.text("Topic")), "motion") {
				continue
			}
			for _, item := range msg.find("SimpleItem") {
				name := strings.ToLower(item.attr("Name"))
				if name != "ismotion" && name != "state" && name != "motion" {
					continue
				}
				val := strings.ToLower(item.attr("Value"))
				if val != "true" && val != "false" && val != "0" && val != "1" {
					continue
				}
				active = val == "true" || val == "1"
				verified = true
			}
		}
		if verified {
			emit(MotionSignal{c.ID, active, true, "onvif", time.Now()})
		} else if time.Since(started) > 30*time.Second {
			return errors.New("motion notifications not verified")
		}
		if !pause(ctx, 100*time.Millisecond) {
			return ctx.Err()
		}
	}
	return ctx.Err()
}
