package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	utls "github.com/refraction-networking/utls"
)

// fakeHTTPRelay parses one request and records it. With accept it answers
// 101 and echoes what follows the 7-byte header; otherwise 404.
func fakeHTTPRelay(t *testing.T, accept bool) (addr string, got <-chan *http.Request) {
	t.Helper()
	ln := fakeTLSListener(t)
	ch := make(chan *http.Request, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		if req.Method == http.MethodPost {
			body, _ := io.ReadAll(req.Body)
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		ch <- req
		if !accept {
			io.WriteString(c, "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
			return
		}
		if req.Method == http.MethodPost {
			cred := bytes.Repeat([]byte{7}, credSize)
			io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 79\r\n\r\n")
			c.Write(append([]byte{0, credSize}, cred...))
			return
		}
		io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		hdr := make([]byte, 7)
		if _, err := io.ReadFull(br, hdr); err != nil {
			return
		}
		io.Copy(c, br)
	}()
	return ln.Addr().String(), ch
}

func withEndpoint(t *testing.T, addr, sni, path string) {
	t.Helper()
	old := relayEP.Swap(&relayTarget{addr, sni, path})
	t.Cleanup(func() { relayEP.Store(old) })
}

func TestDialRelay_V3UpgradeThenRaw(t *testing.T) {
	addr, got := fakeHTTPRelay(t, true)
	withEndpoint(t, addr, "relay.test", "/app/x")
	withConns(t)
	cred := bytes.Repeat([]byte{5}, credSize)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialRelay(ctx, cred, net.IPv4(1, 1, 1, 1), 443)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	req := <-got
	if req.URL.Path != "/app/x" || req.Host != "relay.test" || req.Header.Get("Upgrade") != "websocket" ||
		req.Header.Get("Sec-WebSocket-Protocol") != hex.EncodeToString(cred) || req.Header.Get("Sec-WebSocket-Key") == "" {
		t.Fatalf("request %v %v", req.URL, req.Header)
	}
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
}

func TestBrowserHeaders_PaddedAndVaried(t *testing.T) {
	lens := map[int]bool{}
	for i := 0; i < 32; i++ {
		h := browserHeaders("relay.test")
		if !strings.Contains(h, "User-Agent: Mozilla/5.0") || !strings.Contains(h, "Origin: https://relay.test\r\n") {
			t.Fatalf("headers %q", h)
		}
		cookie := h[strings.Index(h, "Cookie: sid=")+len("Cookie: sid="):]
		cookie = cookie[:strings.Index(cookie, "\r\n")]
		raw, err := base64.RawURLEncoding.DecodeString(cookie)
		if err != nil || len(raw) < padMin || len(raw) > padMax {
			t.Fatalf("cookie len %d err %v", len(raw), err)
		}
		lens[len(h)] = true
	}
	if len(lens) < 8 {
		t.Fatalf("only %d distinct lengths in 32 requests", len(lens))
	}
}

func TestDialRelay_V3Rejected(t *testing.T) {
	addr, _ := fakeHTTPRelay(t, false)
	withEndpoint(t, addr, "relay.test", "/app/x")
	withConns(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := dialRelay(ctx, testCred, net.IPv4(1, 1, 1, 1), 443)
	if !errors.Is(err, ErrCredRejected) {
		t.Fatalf("err=%v", err)
	}
	connsMu.Lock()
	n := len(activeConns)
	connsMu.Unlock()
	if n != 0 {
		t.Fatal("rejected conn left tracked")
	}
	if health.down() {
		t.Fatal("a refused credential counted as the relay being down")
	}
	if LastRelayOK() != 0 {
		t.Fatal("a refused credential set LastRelayOK")
	}
}

// nginx answers for a dead relay with 502: an outage, not a credential.
func TestDialRelay_V3BadGatewayIsAnOutage(t *testing.T) {
	freshHealth(t)
	ln := fakeTLSListener(t)
	withEndpoint(t, ln.Addr().String(), "relay.test", "/app/x")
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
			return
		}
		io.WriteString(c, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	}()
	withConns(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := dialRelay(ctx, testCred, net.IPv4(1, 1, 1, 1), 443)
	if err == nil || errors.Is(err, ErrCredRejected) {
		t.Fatalf("err=%v, want an outage", err)
	}
	if !health.down() {
		t.Fatal("a 502 did not count as the relay being down")
	}
}

func TestRegister_V3Post(t *testing.T) {
	addr, got := fakeHTTPRelay(t, true)
	out, err := Register(addr, "relay.test", "/app/x", KindInvite, []byte("code123"))
	if err != nil || !bytes.Equal(out, bytes.Repeat([]byte{7}, credSize)) {
		t.Fatalf("Register: %v cred=% x", err, out)
	}
	req := <-got
	body, _ := io.ReadAll(req.Body)
	if req.Method != "POST" || req.URL.Path != "/app/x" || string(body) != "\x02code123" {
		t.Fatalf("request %s %s body %q", req.Method, req.URL, body)
	}
}

func TestStart_V3ProbeRejectedBeforeTUN(t *testing.T) {
	addr, _ := fakeHTTPRelay(t, false)
	err := Start(9999, addr, "relay.test", "/app/x", bytes.Repeat([]byte{5}, credSize), "", "", "", nil, nil)
	if !errors.Is(err, ErrCredRejected) {
		t.Fatalf("err=%v", err)
	}
	if s != nil || tunFile != nil {
		t.Fatal("stack started after rejected probe")
	}
	if LastRelayOK() != 0 {
		t.Fatal("a refused credential set LastRelayOK")
	}
}

func TestStart_RefusesMissingCredOrPath(t *testing.T) {
	if err := Start(9999, "127.0.0.1:1", "relay.test", "/app/x", nil, "", "", "", nil, nil); err == nil {
		t.Fatal("started without a credential")
	}
	if err := Start(9999, "127.0.0.1:1", "relay.test", "", testCred, "", "", "", nil, nil); err == nil {
		t.Fatal("started without a path")
	}
}

func drainHellos() {
	for len(lastHello) > 0 {
		<-lastHello
	}
}

// The server side sees Chrome's hello: ALPN h2 first, GREASE-sized cipher list.
func TestDialTLS_ChromeHello(t *testing.T) {
	ln := fakeTLSListener(t)
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.(*tls.Conn).Handshake()
			c.Close()
		}
	}()
	drainHellos()
	c, err := dialTLS(context.Background(), health.generation(), ln.Addr().String(), "relay.test")
	if err != nil {
		t.Fatal(err)
	}
	raw := c.(*utls.UConn).NetConn()
	if ka, idle := sockopt(t, raw, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE), sockopt(t, raw, syscall.IPPROTO_TCP, syscall.TCP_KEEPIDLE); ka != 1 || idle != 150 {
		t.Fatalf("SO_KEEPALIVE %d TCP_KEEPIDLE %d, want 1 and 150", ka, idle)
	}
	c.Close()
	h := <-lastHello
	if len(h.SupportedProtos) != 2 || h.SupportedProtos[0] != "h2" || len(h.CipherSuites) < 15 {
		t.Fatalf("protos %v ciphers %d", h.SupportedProtos, len(h.CipherSuites))
	}
}

func TestDialTLS_SilentServerTimesOut(t *testing.T) {
	freshHealth(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() }) // accept, never answer
		}
	}()
	old := handshakeTimeout
	handshakeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })
	start := time.Now()
	if _, err := dialTLS(context.Background(), health.generation(), ln.Addr().String(), "relay.test"); err == nil {
		t.Fatal("handshake with a silent server succeeded")
	}
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("handshake hung for %s", d)
	}
}

// Pinned in rc/tool/relay_test.go (TestRequestsLikeTheTunnel): rc-promote's
// check talks to the relay as the app does. Random parts (WebSocket key,
// padding cookie) are masked. Change both together.
const (
	upgradeForRCTool = "GET /app/test-path HTTP/1.1\r\nHost: relay.test\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
		"User-Agent: Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36\r\n" +
		"Origin: https://relay.test\r\nAccept-Encoding: gzip, deflate, br\r\nAccept-Language: ru-RU,ru;q=0.9,en-US;q=0.8\r\n" +
		"Cache-Control: no-cache\r\nPragma: no-cache\r\nCookie: *\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: *\r\nSec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\n" +
		"Sec-WebSocket-Protocol: 0505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505050505\r\n\r\n" +
		"\x01\x01\x01\x01\x01\x01\xbb"
	registerForRCTool = "POST /app/test-path HTTP/1.1\r\nHost: relay.test\r\n" +
		"User-Agent: Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36\r\n" +
		"Origin: https://relay.test\r\nAccept-Encoding: gzip, deflate, br\r\nAccept-Language: ru-RU,ru;q=0.9,en-US;q=0.8\r\n" +
		"Cache-Control: no-cache\r\nPragma: no-cache\r\nCookie: *\r\n" +
		"Content-Type: application/octet-stream\r\nContent-Length: 12\r\n\r\n\x02invite-code"
)

func maskRequest(raw string) string {
	return regexp.MustCompile(`(Sec-WebSocket-Key|Cookie): [^\r]*`).ReplaceAllString(raw, "$1: *")
}

func TestRequestsForRCTool(t *testing.T) {
	ln := fakeTLSListener(t)
	got := make(chan string, 2)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			var raw bytes.Buffer
			br := bufio.NewReader(io.TeeReader(c, &raw))
			req, err := http.ReadRequest(br)
			if err != nil {
				c.Close()
				return
			}
			if req.Method == http.MethodPost {
				io.ReadAll(req.Body)
				io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Length: 79\r\n\r\n")
				c.Write(append([]byte{0, credSize}, testCred...))
			} else {
				io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
				io.ReadFull(br, make([]byte, 7))
			}
			got <- raw.String()
			c.Close()
		}
	}()
	withEndpoint(t, ln.Addr().String(), "relay.test", "/app/test-path")
	withConns(t)
	c, err := dialRelay(context.Background(), testCred, net.IPv4(1, 1, 1, 1), 443)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if g := maskRequest(<-got); g != upgradeForRCTool {
		t.Errorf("upgrade %q", g)
	}
	if _, err := Register(ln.Addr().String(), "relay.test", "/app/test-path", KindInvite, []byte("invite-code")); err != nil {
		t.Fatal(err)
	}
	if g := maskRequest(<-got); g != registerForRCTool {
		t.Errorf("register %q", g)
	}
}
