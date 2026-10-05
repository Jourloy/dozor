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
			return nil, errors.New("ONVIF camera unavailable")
		}
		b, e := io.ReadAll(io.LimitReader(res.Body, 2<<20))
		res.Body.Close()
		if e != nil {
			return nil, e
		}
		if res.StatusCode == 401 && attempt == 0 {
			auth, e = digestAuth(res.Header.Get("WWW-Authenticate"), "POST", req.URL.RequestURI(), c.Username, c.Password)
			if e != nil {
				return nil, e
			}
			continue
		}
		if res.StatusCode != 200 {
			return nil, errors.New("ONVIF request rejected")
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

func Discover(ctx context.Context) ([]Discovery, error) {
	conn, e := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero})
	if e != nil {
		return nil, e
	}
	defer conn.Close()
	msg := `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:a="http://schemas.xmlsoap.org/ws/2004/08/addressing" xmlns:d="http://schemas.xmlsoap.org/ws/2005/04/discovery" xmlns:dn="http://www.onvif.org/ver10/network/wsdl"><s:Header><a:MessageID>` + uuidURN() + `</a:MessageID><a:To s:mustUnderstand="true">urn:schemas-xmlsoap-org:ws:2005:04:discovery</a:To><a:Action s:mustUnderstand="true">http://schemas.xmlsoap.org/ws/2005/04/discovery/Probe</a:Action></s:Header><s:Body><d:Probe><d:Types>dn:NetworkVideoTransmitter</d:Types></d:Probe></s:Body></s:Envelope>`
	_, e = conn.WriteToUDP([]byte(msg), &net.UDPAddr{IP: net.IPv4(239, 255, 255, 250), Port: 3702})
	if e != nil {
		return nil, e
	}
	deadline := time.Now().Add(4 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	_ = conn.SetReadDeadline(deadline)
	results := []Discovery{}
	seen := map[string]bool{}
	buf := make([]byte, 65535)
	for {
		n, _, e := conn.ReadFromUDP(buf)
		if e != nil {
			break
		}
		tree, e := parseXML(buf[:n])
		if e != nil {
			continue
		}
		for _, p := range tree.find("ProbeMatch") {
			for _, ep := range strings.Fields(p.text("XAddrs")) {
				u, e := url.Parse(ep)
				if e == nil && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && !seen[ep] {
					seen[ep] = true
					results = append(results, Discovery{ep, p.text("Scopes")})
				}
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
