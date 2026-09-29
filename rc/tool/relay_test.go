package main

import (
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Pinned in android/tunnel/v3_test.go (TestRequestsForRCTool): the check
// talks to the relay as the app does. Change both together.
const (
	upgradeForTunnel = "GET /app/test-path HTTP/1.1\r\nHost: relay.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
		"User-Agent: Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36\r\n" +
		"Origin: https://relay.test\r\nAccept-Encoding: gzip, deflate, br\r\nAccept-Language: ru-RU,ru;q=0.9,en-US;q=0.8\r\n" +
		"Cache-Control: no-cache\r\nPragma: no-cache\r\nCookie: *\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: *\r\nSec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\n" +
		"Sec-WebSocket-Protocol: 0505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505\r\n\r\n" +
		"\x01\x01\x01\x01\x01\x01\xbb"
	registerForTunnel = "POST /app/test-path HTTP/1.1\r\nHost: relay.test\r\n" +
		"User-Agent: Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36\r\n" +
		"Origin: https://relay.test\r\nAccept-Encoding: gzip, deflate, br\r\nAccept-Language: ru-RU,ru;q=0.9,en-US;q=0.8\r\n" +
		"Cache-Control: no-cache\r\nPragma: no-cache\r\nCookie: *\r\n" +
		"Content-Type: application/octet-stream\r\nContent-Length: 12\r\n\r\n\x02invite-code"
)

func TestRequestsLikeTheTunnel(t *testing.T) {
	mask := regexp.MustCompile(`(Sec-WebSocket-Key|Cookie): [^\r]*`)
	ep := endpoint{Host: "relay.test", IP: "203.0.113.10", Port: 443, Path: "/app/test-path"}
	cred := make([]byte, credSize)
	for i := range cred {
		cred[i] = 5
	}
	up := mask.ReplaceAllString(upgradeRequest(ep, cred), "$1: *") + string(relayHeader(&net.TCPAddr{IP: net.IPv4(1, 1, 1, 1), Port: 443}))
	if up != upgradeForTunnel {
		t.Errorf("upgrade %q", up)
	}
	if reg := mask.ReplaceAllString(registerRequest(ep, kindInvite, []byte("invite-code")), "$1: *"); reg != registerForTunnel {
		t.Errorf("register %q", reg)
	}
	// As the tunnel's TestCredExpires.
	cred[9], cred[10], cred[11], cred[12] = 0x6B, 0x49, 0xD2, 0x00
	if got := credExpires(cred).Unix(); got != 1800000000 {
		t.Errorf("expires %d", got)
	}
}

const testPath = "/app/test-path"

func newCred(expires time.Time) []byte {
	c := make([]byte, credSize)
	c[0] = kindInvite
	binary.BigEndian.PutUint32(c[9:13], uint32(expires.Unix()))
	return c
}

// fakeRelay is nginx + relay in one: POST registers with the one invite,
// the upgrade takes the one credential and then carries raw TCP to where
// the 7-byte header says.
type fakeRelay struct {
	*httptest.Server
	cred   []byte
	invite string
	hits   atomic.Int32
}

func newFakeRelay(t *testing.T, cred []byte, invite string) *fakeRelay {
	f := &fakeRelay{cred: cred, invite: invite}
	f.Server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if r.URL.Path != testPath {
			http.NotFound(w, r)
			return
		}
		switch r.Method {
		case http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			if string(body) != "\x02"+f.invite {
				http.NotFound(w, r)
				return
			}
			w.Write(append([]byte{0, credSize}, f.cred...))
		case http.MethodGet:
			if r.Header.Get("Upgrade") != "websocket" || r.Header.Get("Sec-WebSocket-Protocol") != hex.EncodeToString(f.cred) {
				http.NotFound(w, r)
				return
			}
			c, brw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				return
			}
			defer c.Close()
			brw.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
			brw.Flush()
			hdr := make([]byte, 7)
			if _, err := io.ReadFull(brw, hdr); err != nil || hdr[0] != 1 {
				return
			}
			d, err := net.Dial("tcp", (&net.TCPAddr{IP: net.IP(hdr[1:5]), Port: int(binary.BigEndian.Uint16(hdr[5:]))}).String())
			if err != nil {
				return
			}
			defer d.Close()
			go io.Copy(d, brw)
			io.Copy(c, d)
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeRelay) endpoint() endpoint {
	a := f.Listener.Addr().(*net.TCPAddr)
	// httptest's certificate is for example.com and 127.0.0.1.
	return endpoint{Host: "example.com", IP: a.IP.String(), Port: a.Port, Path: testPath}
}

type stand struct {
	p        prober
	credPath string
	now      time.Time
}

func newStand(t *testing.T) stand {
	page := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(page.Close)
	roots := x509.NewCertPool()
	roots.AddCert(page.Certificate())
	return stand{
		p:        prober{roots: roots, timeout: 5 * time.Second, target: target{addr: page.Listener.Addr().String(), sni: "example.com", roots: roots}},
		credPath: filepath.Join(t.TempDir(), ".check-cred"),
		now:      time.Now(),
	}
}

func debugTemplate(t *testing.T, eps ...endpoint) string {
	blob, err := encryptEndpoints(eps, testKey, strings.NewReader(strings.Repeat("n", gcmNonceSize)))
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{
  "conditions": [{"name": "build_debug", "expression": "x"}],
  "parameters": {
    "endpoints": {"defaultValue": {"value": "OLD"}, "conditionalValues": {"build_debug": {"value": %q}}, "valueType": "STRING"},
    "min_version": {"defaultValue": {"value": "100"}, "conditionalValues": {"build_debug": {"value": "114"}}, "valueType": "NUMBER"},
    "latest_version": {"defaultValue": {"useInAppDefault": true}, "conditionalValues": {"build_debug": {"value": "115"}}, "valueType": "NUMBER"},
    "update_url": {"defaultValue": {"value": "old"}, "conditionalValues": {"build_debug": {"value": "https://github.com/o/r/releases/download/v0.5.7/app.apk"}}, "valueType": "STRING"}
  }
}`, blob)
}

func noAddresses(t *testing.T, out string, eps ...endpoint) {
	t.Helper()
	for _, ep := range eps {
		for _, s := range []string{ep.IP, ep.Host, ep.Path} {
			if strings.Contains(out, s) {
				t.Errorf("output names %q:\n%s", s, out)
			}
		}
	}
}

func TestPromoteWhenAllWork(t *testing.T) {
	st := newStand(t)
	cred := newCred(st.now.Add(7 * 24 * time.Hour))
	a, b := newFakeRelay(t, cred, "code-1"), newFakeRelay(t, cred, "code-1")
	f, rc := newFake(t, debugTemplate(t, a.endpoint(), b.endpoint()))
	tmpl, etag, _ := rc.get()
	var out strings.Builder
	if err := checkAndPromote(rc, tmpl, etag, testKey, st.p, st.credPath, "code-1", st.now, true, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if want := []string{"GET /rc", "PUT /rc?validate_only=true", "PUT /rc"}; !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls %v, want %v", f.calls, want)
	}
	got := template(f.put)
	for _, k := range []string{"endpoints", "min_version", "latest_version", "update_url"} {
		d, _ := got.value(k, "")
		dbg, ok := got.value(k, debugCondition)
		if !ok || d != dbg {
			t.Errorf("%s: default %q, debug %q", k, d, dbg)
		}
	}
	if _, ok := got.condition(debugCondition); !ok {
		t.Error("condition dropped")
	}
	saved, _ := os.ReadFile(st.credPath)
	if strings.TrimSpace(string(saved)) != hex.EncodeToString(cred) {
		t.Errorf("credential not cached: %q", saved)
	}
	o := out.String()
	if !strings.Contains(o, "endpoint 1/2: ok") || !strings.Contains(o, "endpoint 2/2: ok") || !strings.Contains(o, "published version 8") {
		t.Errorf("output:\n%s", o)
	}
	noAddresses(t, o, a.endpoint(), b.endpoint())

	// The cached credential does, the code is left alone; defaults already match.
	f.calls = nil
	tmpl, etag, _ = rc.get()
	out.Reset()
	if err := checkAndPromote(rc, tmpl, etag, testKey, st.p, st.credPath, "code-2", st.now, true, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if want := []string{"GET /rc"}; !reflect.DeepEqual(f.calls, want) || !strings.Contains(out.String(), "nothing to publish") {
		t.Fatalf("calls %v\n%s", f.calls, out.String())
	}
}

func TestNoPromoteWhenOneIsDown(t *testing.T) {
	st := newStand(t)
	cred := newCred(st.now.Add(3 * 24 * time.Hour))
	os.WriteFile(st.credPath, []byte(hex.EncodeToString(cred)), 0o600)
	a, b := newFakeRelay(t, cred, ""), newFakeRelay(t, cred, "")
	dead := b.endpoint()
	b.Close()
	f, rc := newFake(t, debugTemplate(t, a.endpoint(), dead))
	tmpl, etag, _ := rc.get()
	var out strings.Builder
	err := checkAndPromote(rc, tmpl, etag, testKey, st.p, st.credPath, "", st.now, true, &out)
	if err == nil || !strings.Contains(out.String(), "endpoint 1/2: ok") || !strings.Contains(out.String(), "endpoint 2/2: ") || strings.Contains(out.String(), "2/2: ok") {
		t.Fatalf("err %v\n%s", err, out.String())
	}
	if want := []string{"GET /rc"}; !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls %v", f.calls)
	}
	noAddresses(t, out.String()+err.Error(), a.endpoint(), dead)
}

// nginx with a relay that refuses the credential, and one with no relay behind it.
func TestRefusedCredentialAndBadGatewayAreDown(t *testing.T) {
	st := newStand(t)
	cred := newCred(st.now.Add(3 * 24 * time.Hour))
	os.WriteFile(st.credPath, []byte(hex.EncodeToString(cred)), 0o600)
	refusing := newFakeRelay(t, newCred(st.now), "")
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusBadGateway) }))
	t.Cleanup(gateway.Close)
	ga := gateway.Listener.Addr().(*net.TCPAddr)
	eps := []endpoint{refusing.endpoint(), {Host: "example.com", IP: ga.IP.String(), Port: ga.Port, Path: testPath}}
	var out strings.Builder
	if checkAll(st.p, eps, cred, &out) {
		t.Fatalf("all ok:\n%s", out.String())
	}
	o := out.String()
	if !strings.Contains(o, "endpoint 1/2: credential refused") || !strings.Contains(o, "endpoint 2/2: upgrade: http 502") || !strings.Contains(o, "INVITE=") {
		t.Errorf("output:\n%s", o)
	}
}

func TestExpiredCredentialAsksForInvite(t *testing.T) {
	st := newStand(t)
	os.WriteFile(st.credPath, []byte(hex.EncodeToString(newCred(st.now.Add(-time.Hour)))), 0o600)
	a := newFakeRelay(t, newCred(st.now.Add(7*24*time.Hour)), "code-1")
	f, rc := newFake(t, debugTemplate(t, a.endpoint()))
	tmpl, etag, _ := rc.get()
	var out strings.Builder
	err := checkAndPromote(rc, tmpl, etag, testKey, st.p, st.credPath, "", st.now, true, &out)
	if !errors.Is(err, errNeedInvite) || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("err %v", err)
	}
	if a.hits.Load() != 0 || len(f.calls) != 1 {
		t.Fatalf("relay hits %d, rc calls %v", a.hits.Load(), f.calls)
	}
	// Nearly expired counts as expired.
	os.WriteFile(st.credPath, []byte(hex.EncodeToString(newCred(st.now.Add(credMargin/2)))), 0o600)
	if _, err := credential(st.p, []endpoint{a.endpoint()}, st.credPath, "", st.now, io.Discard); !errors.Is(err, errNeedInvite) {
		t.Fatalf("err %v", err)
	}
	os.Remove(st.credPath)
	if _, err := credential(st.p, []endpoint{a.endpoint()}, st.credPath, "", st.now, io.Discard); !errors.Is(err, errNeedInvite) {
		t.Fatalf("no file: err %v", err)
	}
}

func TestRegisterTriesEveryEndpoint(t *testing.T) {
	st := newStand(t)
	cred := newCred(st.now.Add(7 * 24 * time.Hour))
	notIssuer, issuer := newFakeRelay(t, cred, "other"), newFakeRelay(t, cred, "code-1")
	var out strings.Builder
	got, err := credential(st.p, []endpoint{notIssuer.endpoint(), issuer.endpoint()}, st.credPath, "code-1", st.now, &out)
	if err != nil || hex.EncodeToString(got) != hex.EncodeToString(cred) || !strings.Contains(out.String(), "via endpoint 2") {
		t.Fatalf("err %v\n%s", err, out.String())
	}
	os.Remove(st.credPath)
	_, err = credential(st.p, []endpoint{notIssuer.endpoint()}, st.credPath, "code-1", st.now, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "endpoint 1: code refused") {
		t.Fatalf("err %v", err)
	}
	noAddresses(t, err.Error(), notIssuer.endpoint())

	var out2 strings.Builder
	got, err = credential(st.p, []endpoint{issuer.endpoint()}, filepath.Join(st.credPath, "no-dir"), "code-1", st.now, &out2)
	if err != nil || len(got) != credSize || !strings.Contains(out2.String(), "valid until") || !strings.Contains(out2.String(), "not cached") {
		t.Fatalf("unwritable cache: err %v\n%s", err, out2.String())
	}
}

func TestPromoteConflict(t *testing.T) {
	st := newStand(t)
	cred := newCred(st.now.Add(7 * 24 * time.Hour))
	a := newFakeRelay(t, cred, "code-1")
	f, rc := newFake(t, debugTemplate(t, a.endpoint()))
	f.bumpAfter = true
	tmpl, etag, _ := rc.get()
	err := checkAndPromote(rc, tmpl, etag, testKey, st.p, st.credPath, "code-1", st.now, true, io.Discard)
	if !errors.Is(err, errConflict) {
		t.Fatalf("err %v", err)
	}
	if f.put != nil {
		t.Fatal("published over a newer version")
	}
}

func TestPromoteNeedsDebugValues(t *testing.T) {
	_, rc := newFake(t, consoleTemplate)
	tmpl, etag, _ := rc.get()
	if err := promote(rc, tmpl, etag, io.Discard); err == nil || !strings.Contains(err.Error(), "rc-push") {
		t.Fatalf("err %v", err)
	}
}

// A version-only promote: the defaults already hold the same list under
// another nonce, so no relay is asked and no invite needed. rc-check still checks.
func TestPromoteSameEndpointsSkipsCheck(t *testing.T) {
	st := newStand(t)
	a := newFakeRelay(t, newCred(st.now.Add(7*24*time.Hour)), "code-1")
	ep := a.endpoint()
	def, err := encryptEndpoints([]endpoint{ep}, testKey, strings.NewReader(strings.Repeat("d", gcmNonceSize)))
	if err != nil {
		t.Fatal(err)
	}
	tmpl := strings.Replace(debugTemplate(t, ep), `"value": "OLD"`, fmt.Sprintf(`"value": %q`, def), 1)

	f, rc := newFake(t, tmpl)
	tm, etag, _ := rc.get()
	if err := checkAndPromote(rc, tm, etag, testKey, st.p, st.credPath, "", st.now, false, io.Discard); !errors.Is(err, errNeedInvite) {
		t.Fatalf("rc-check: err %v", err)
	}

	f.calls = nil
	tm, etag, _ = rc.get()
	var out strings.Builder
	if err := checkAndPromote(rc, tm, etag, testKey, st.p, st.credPath, "", st.now, true, &out); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	if a.hits.Load() != 0 || !strings.Contains(out.String(), "check skipped") {
		t.Fatalf("relay hits %d\n%s", a.hits.Load(), out.String())
	}
	if want := []string{"GET /rc", "PUT /rc?validate_only=true", "PUT /rc"}; !reflect.DeepEqual(f.calls, want) {
		t.Fatalf("calls %v", f.calls)
	}
	got := template(f.put)
	for _, k := range []string{"min_version", "latest_version", "update_url"} {
		if d, _ := got.value(k, ""); d != map[string]string{"min_version": "114", "latest_version": "115", "update_url": "https://github.com/o/r/releases/download/v0.5.7/app.apk"}[k] {
			t.Errorf("%s: default %q", k, d)
		}
	}
}
