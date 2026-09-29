package main

import (
	"bufio"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func httpDial(t *testing.T, relayAddr, raw string) (net.Conn, *bufio.Reader, *http.Response) {
	t.Helper()
	c, err := net.Dial("tcp", relayAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte(raw))
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		t.Fatalf("response: %v", err)
	}
	return c, br, res
}

var dateHeader = regexp.MustCompile(`(?m)^Date: .*\r$`)

// rawReply returns the relay's whole reply to raw, Date blanked so that two
// replies compare byte for byte.
func rawReply(t *testing.T, relayAddr, raw string) string {
	t.Helper()
	c, err := net.Dial("tcp", relayAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte(raw))
	b, _ := io.ReadAll(c)
	return dateHeader.ReplaceAllString(string(b), "Date: -\r")
}

func upgradeReq(cred []byte) string {
	return "GET /s/x HTTP/1.1\r\nHost: site\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\nSec-WebSocket-Version: 13\r\n" +
		"Sec-WebSocket-Protocol: " + hex.EncodeToString(cred) + "\r\n\r\n"
}

func TestHTTP_UpgradeThenEcho(t *testing.T) {
	installIssuer(t)
	relay := startRelay(t)
	ip, port := startEcho(t)
	cred := issueCred(kindIntegrity, time.Now().Add(time.Hour))
	c, br, res := httpDial(t, relay, upgradeReq(cred))
	if res.StatusCode != 101 || res.Header.Get("Sec-WebSocket-Accept") != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("status %d accept %q", res.StatusCode, res.Header.Get("Sec-WebSocket-Accept"))
	}
	c.Write(preamble(nil, typeTCP, ip, port)[len(upgradeReq(nil)):])
	c.Write([]byte("hello"))
	buf := make([]byte, 5)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("got %q %v", buf, err)
	}
}

func TestHTTP_BadCredentialAndProbesGet404(t *testing.T) {
	installIssuer(t)
	relay := startRelay(t)
	expired := issueCred(kindIntegrity, time.Now().Add(-time.Minute))
	for name, raw := range map[string]string{
		"expired":   upgradeReq(expired),
		"nocred":    upgradeReq(nil),
		"plain get": "GET / HTTP/1.1\r\nHost: site\r\n\r\n",
		"post junk": "POST /s/x HTTP/1.1\r\nHost: site\r\nContent-Length: 1\r\n\r\nx",
	} {
		_, _, res := httpDial(t, relay, raw)
		body, _ := io.ReadAll(res.Body)
		if res.StatusCode != 404 || !strings.Contains(string(body), "nginx") || res.Header.Get("Server") != "nginx" {
			t.Fatalf("%s: status %d body %q", name, res.StatusCode, body)
		}
	}
}

// nginx proxies any method to the location; a dropped connection would make
// it answer 502.
func TestHTTP_OtherMethodsGet404(t *testing.T) {
	relay := startRelay(t)
	get := rawReply(t, relay, "GET /s/x HTTP/1.1\r\nHost: site\r\n\r\n")
	for _, m := range []string{"PUT", "OPTIONS", "DELETE", "PATCH", "M-SEARCH", "AB", "X_Y"} {
		if got := rawReply(t, relay, m+" /s/x HTTP/1.1\r\nHost: site\r\nContent-Length: 0\r\n\r\n"); got != get {
			t.Fatalf("%s: %q", m, got)
		}
	}
	if got := rawReply(t, relay, "\x01\x02junk\r\n\r\n"); got != get {
		t.Fatalf("junk: %q", got)
	}
	head := rawReply(t, relay, "HEAD /s/x HTTP/1.1\r\nHost: site\r\n\r\n")
	if want := strings.TrimSuffix(get, notFound); head != want {
		t.Fatalf("HEAD: %q, want %q", head, want)
	}
}

// Every refusal, whatever its reason, is the probe's 404 to the byte.
func TestHTTP_RefusedRegisterIsProbe404(t *testing.T) {
	installIssuer(t)
	relay := startRelay(t)
	probe := rawReply(t, relay, "GET / HTTP/1.1\r\nHost: site\r\n\r\n")
	if !strings.HasPrefix(probe, "HTTP/1.1 404") {
		t.Fatalf("probe: %q", probe)
	}
	post := func(body string) string {
		return "POST /s/x HTTP/1.1\r\nHost: site\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	}
	for name, raw := range map[string]string{
		"unknown kind": post("\x7fjunk"),
		"bad invite":   post("\x02nope"),
		"no verifier":  post("\x01" + strings.Repeat("n", nonceSize) + "token"),
		"short body":   post("\x02"),
	} {
		if got := rawReply(t, relay, raw); got != probe {
			t.Fatalf("%s:\n%q\nwant\n%q", name, got, probe)
		}
	}
	issuerPriv = nil
	if got := rawReply(t, relay, post("\x02code")); got != probe {
		t.Fatalf("not issuer:\n%q\nwant\n%q", got, probe)
	}
}

// A body cut short (client half-closed) is a refusal like any other, not a dropped connection.
func TestHTTP_TruncatedRegisterBodyIs404(t *testing.T) {
	installIssuer(t)
	relay := startRelay(t)
	probe := rawReply(t, relay, "GET / HTTP/1.1\r\nHost: site\r\n\r\n")
	c, err := net.Dial("tcp", relay)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte("POST /s/x HTTP/1.1\r\nHost: site\r\nContent-Length: 10\r\n\r\n\x02abc"))
	c.(*net.TCPConn).CloseWrite()
	b, _ := io.ReadAll(c)
	if got := dateHeader.ReplaceAllString(string(b), "Date: -\r"); got != probe {
		t.Fatalf("got\n%q\nwant\n%q", got, probe)
	}
}

func TestHTTP_RegisterThenUpgrade(t *testing.T) {
	installIssuer(t)
	f := filepath.Join(t.TempDir(), "invites")
	os.WriteFile(f, []byte("code1\n"), 0o600)
	old := invitesFile
	invitesFile = f
	t.Cleanup(func() { invitesFile = old })
	relay := startRelay(t)
	ip, port := startEcho(t)

	body := append([]byte{kindInvite}, "code1"...)
	post := func() (int, []byte) {
		_, _, res := httpDial(t, relay, "POST /s/x HTTP/1.1\r\nHost: site\r\nContent-Length: "+
			strconv.Itoa(len(body))+"\r\n\r\n"+string(body))
		b, _ := io.ReadAll(res.Body)
		return res.StatusCode, b
	}
	st, b := post()
	if st != 200 || len(b) != 2+credSize || b[0] != statusOK || int(b[1]) != credSize {
		t.Fatalf("register: status %d body %x", st, b)
	}
	cred := b[2:]
	if st, b := post(); st != 404 || !strings.Contains(string(b), "nginx") {
		t.Fatalf("invite reused: status %d body %q", st, b)
	}

	c, br, res := httpDial(t, relay, upgradeReq(cred))
	if res.StatusCode != 101 {
		t.Fatalf("status %d", res.StatusCode)
	}
	c.Write(preamble(nil, typeTCP, ip, port)[len(upgradeReq(nil)):])
	c.Write([]byte("ok"))
	buf := make([]byte, 2)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ok" {
		t.Fatalf("got %q %v", buf, err)
	}
}

func TestHTTP_RegisterAfterUpgradeRefused(t *testing.T) {
	installIssuer(t)
	relay := startRelay(t)
	cred := issueCred(kindIntegrity, time.Now().Add(time.Hour))
	c, br, res := httpDial(t, relay, upgradeReq(cred))
	if res.StatusCode != 101 {
		t.Fatalf("status %d", res.StatusCode)
	}
	c.Write(append([]byte{0x03, kindInvite, 0, 4, 0, 0, 0}, "code"...))
	if b, err := br.ReadByte(); err == nil {
		t.Fatalf("got reply %#x to register over an upgraded session", b)
	}
}
