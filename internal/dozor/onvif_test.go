package dozor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestONVIFProfilesAndCredentialIsolation(t *testing.T) {
	var endpoint string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		n, err := parseXML(b)
		if err != nil || len(n.find("UsernameToken")) != 1 || n.text("Username") != "admin<&" {
			t.Error("invalid escaped SOAP/WSSE envelope")
			w.WriteHeader(400)
			return
		}
		var response string
		switch {
		case len(n.find("GetCapabilities")) > 0:
			response = `<GetCapabilitiesResponse><Capabilities><Media><XAddr>` + endpoint + `/media</XAddr></Media></Capabilities></GetCapabilitiesResponse>`
		case len(n.find("GetProfiles")) > 0:
			response = `<GetProfilesResponse><Profiles token="main"><Name>Main stream</Name></Profiles></GetProfilesResponse>`
		case len(n.find("GetStreamUri")) > 0:
			response = `<GetStreamUriResponse><MediaUri><Uri>rtsp://admin:secret@192.168.1.50/main</Uri></MediaUri></GetStreamUriResponse>`
		default:
			t.Error("unexpected SOAP action")
		}
		w.Header().Set("Content-Type", "application/soap+xml")
		io.WriteString(w, `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope"><s:Body>`+response+`</s:Body></s:Envelope>`)
	}))
	defer server.Close()
	endpoint = server.URL
	c := Camera{ONVIF: endpoint + "/device", Username: "admin<&", Password: "fixture"}
	v, err := Profiles(context.Background(), c)
	must(t, err)
	if len(v) != 1 || v[0].URL != "rtsp://192.168.1.50/main" || v[0].Name != "Main stream" {
		t.Fatal(v)
	}
	if _, err = soap(context.Background(), c, "http://different-camera.invalid/device", "action", ""); err == nil {
		t.Fatal("credentials could be sent to a different service host")
	}
}

func TestHTTPDigestReferenceVector(t *testing.T) {
	value, err := digestAuth(`Digest realm="testrealm@host.com", nonce="dcd98b7102dd2f0e8b11d0f600bfb0c093"`, "GET", "/dir/index.html", "Mufasa", "Circle Of Life")
	must(t, err)
	if !strings.Contains(value, `response="670fd8c2df070c60b045671b8b24ff02"`) {
		t.Fatal("incorrect legacy HTTP Digest response")
	}
	if _, err = digestAuth(`Digest realm="r", nonce="n", qop="auth-int"`, "GET", "/", "u", "p"); err == nil {
		t.Fatal("unsupported integrity mode accepted")
	}
}
