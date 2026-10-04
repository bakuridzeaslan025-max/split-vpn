package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// testCred is what tests hand to dialRelay; the fakes do not verify it.
var testCred = bytes.Repeat([]byte{5}, credSize)

// fakeRelay speaks the relay's HTTP: answers the upgrade with 101, hands
// the 7-byte header to got and echoes everything after it. New relay
// sessions go to it.
func fakeRelay(t *testing.T) (addr string, got <-chan []byte) {
	t.Helper()
	ln := fakeTLSListener(t)
	withEndpoint(t, ln.Addr().String(), "relay.test", "/app/x")
	ch := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		if _, err := http.ReadRequest(br); err != nil {
			return
		}
		io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
		hdr := make([]byte, 7)
		if _, err := io.ReadFull(br, hdr); err != nil {
			return
		}
		ch <- hdr
		io.Copy(c, br)
	}()
	return ln.Addr().String(), ch
}

// withConns gives the test a live conn tracker.
func withConns(t *testing.T) {
	setConns(make(map[net.Conn]struct{}))
	t.Cleanup(func() { setConns(nil) })
}

// setConns locks like the tunnel does: handleTCP goroutines of an earlier
// test may still be untracking their conns.
func setConns(m map[net.Conn]struct{}) {
	connsMu.Lock()
	activeConns = m
	connsMu.Unlock()
}

// lastHello receives every ClientHello the fake listeners see (buffered, drop when full).
var lastHello = make(chan *tls.ClientHelloInfo, 64)

// fakeTLSListener is a TLS listener with a self-signed cert the package trusts.
func fakeTLSListener(t *testing.T) net.Listener {
	t.Helper()
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "relay.test"},
		DNSNames:     []string{"relay.test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, _ := x509.ParseCertificate(der)
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	relayRootCAs = pool
	t.Cleanup(func() { relayRootCAs = nil })
	freshHealth(t)

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
		GetConfigForClient: func(h *tls.ClientHelloInfo) (*tls.Config, error) {
			select {
			case lastHello <- h:
			default:
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func TestDialRelay_HeaderAndTracking(t *testing.T) {
	_, got := fakeRelay(t)
	withConns(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialRelay(ctx, testCred, net.IPv4(149, 154, 167, 99), 443)
	if err != nil {
		t.Fatal(err)
	}

	hdr := <-got
	if hdr[0] != 0x01 || !net.IP(hdr[1:5]).Equal(net.IPv4(149, 154, 167, 99)) || binary.BigEndian.Uint16(hdr[5:7]) != 443 {
		t.Fatalf("bad header: % x", hdr)
	}

	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo failed: %q %v", buf, err)
	}

	connsMu.Lock()
	n := len(activeConns)
	connsMu.Unlock()
	if n != 1 {
		t.Fatalf("tracked %d conns, want 1", n)
	}
	c.Close()
	connsMu.Lock()
	n = len(activeConns)
	connsMu.Unlock()
	if n != 0 {
		t.Fatalf("conn not untracked on Close: %d", n)
	}
}

func TestDialRelay_RefusesWhenStopped(t *testing.T) {
	fakeRelay(t)
	setConns(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dialRelay(ctx, testCred, net.IPv4(1, 1, 1, 1), 443); err == nil {
		t.Fatal("dialRelay must fail after Stop()")
	}
}

func TestDialRelay_BadSNI(t *testing.T) {
	addr, _ := fakeRelay(t)
	withEndpoint(t, addr, "evil.test", "/app/x")
	withConns(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dialRelay(ctx, testCred, net.IPv4(1, 1, 1, 1), 443); err == nil {
		t.Fatal("certificate for wrong host must be rejected")
	}
}

// A failed Start must not keep an *os.File over a fd the caller closes:
// its finalizer would later close whatever fd number Android reused.
func TestStart_FailedInitLeavesNoTunFile(t *testing.T) {
	addr, _ := fakeRelay(t)

	err := Start(9999, addr, "relay.test", "/app/x", testCred, "", "", "", nil, nil) // not an open fd: fdbased.New fails
	if err == nil {
		t.Fatal("expected error")
	}
	mu.Lock()
	defer mu.Unlock()
	if tunFile != nil || s != nil {
		t.Fatalf("tunFile=%v s=%v after failed Start", tunFile, s)
	}
}

func wantRelayOKSince(t *testing.T, t0 int64) {
	t.Helper()
	if got := LastRelayOK(); got < t0 || got > time.Now().Unix() {
		t.Fatalf("LastRelayOK = %d, want since %d", got, t0)
	}
}

// Start's probe getting a 101 is a session the relay let in.
func TestStart_ProbeSetsLastRelayOK(t *testing.T) {
	addr, _ := fakeRelay(t)
	health.lastOK.Store(0)
	t0 := time.Now().Unix()
	Start(9999, addr, "relay.test", "/app/x", testCred, "", "", "", nil, nil) // fails later, on the fake fd
	wantRelayOKSince(t, t0)
}

// An app's relay session sets it, and Stop does not take it back: the day
// the relay carried traffic stays that day.
func TestDialRelay_SetsLastRelayOKAndStopKeepsIt(t *testing.T) {
	fakeRelay(t)
	withConns(t)
	health.lastOK.Store(0)
	t0 := time.Now().Unix()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialRelay(ctx, testCred, net.IPv4(1, 1, 1, 1), 443)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	wantRelayOKSince(t, t0)
	Stop()
	wantRelayOKSince(t, t0)
}

// Wifi went away: every relay connection is bound to a dead path and would
// hang for minutes. NetworkLost must cut them so apps reconnect at once,
// while the tunnel itself keeps accepting new connections.
func TestNetworkLost_CutsActiveConnsKeepsTunnel(t *testing.T) {
	_, got := fakeRelay(t)
	mu.Lock()
	setConns(make(map[net.Conn]struct{}))
	doh = newDoHClient(testCred)
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		setConns(nil)
		doh = nil
		mu.Unlock()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := dialRelay(ctx, testCred, net.IPv4(1, 1, 1, 1), 443)
	if err != nil {
		t.Fatal(err)
	}
	<-got

	NetworkLost()

	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("conn still open after NetworkLost")
	}
	connsMu.Lock()
	n := len(activeConns)
	alive := activeConns != nil
	connsMu.Unlock()
	if n != 0 || !alive {
		t.Fatalf("activeConns len=%d nil=%v; want empty but usable", n, !alive)
	}
}

// fakeIssuer answers one POST with status and cred, records the body.
func fakeIssuer(t *testing.T, status byte, cred []byte) (addr string, got <-chan []byte) {
	reply := append([]byte{status, byte(len(cred))}, cred...)
	return fakeHTTP(t, fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(reply), reply))
}

// fakeHTTP answers one request with the raw reply, records the body.
func fakeHTTP(t *testing.T, reply string) (addr string, got <-chan []byte) {
	t.Helper()
	ln := fakeTLSListener(t)
	ch := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		req, err := http.ReadRequest(bufio.NewReader(c))
		if err != nil {
			return
		}
		body, _ := io.ReadAll(req.Body)
		ch <- body
		io.WriteString(c, reply)
	}()
	return ln.Addr().String(), ch
}

func TestRegister_InviteBodyAndCredential(t *testing.T) {
	cred := bytes.Repeat([]byte{9}, 77)
	addr, got := fakeIssuer(t, 0, cred)
	out, err := Register(addr, "relay.test", "/app/x", KindInvite, []byte("code123"))
	if err != nil || !bytes.Equal(out, cred) {
		t.Fatalf("Register: %v cred=% x", err, out)
	}
	if body := <-got; string(body) != "\x02code123" {
		t.Fatalf("body %q", body)
	}
}

func TestRegister_StatusErrors(t *testing.T) {
	for status, want := range map[byte]error{1: ErrRejected, 2: ErrNotIssuer} {
		addr, _ := fakeIssuer(t, status, nil)
		if _, err := Register(addr, "relay.test", "/app/x", KindInvite, []byte("x")); !errors.Is(err, want) {
			t.Fatalf("status %d: err=%v want %v", status, err, want)
		}
	}
	addr, _ := fakeHTTP(t, "HTTP/1.1 404 Not Found\r\nServer: nginx\r\nContent-Type: text/html\r\nContent-Length: 9\r\n\r\nnot found")
	if _, err := Register(addr, "relay.test", "/app/x", KindInvite, []byte("x")); !errors.Is(err, ErrRejected) {
		t.Fatalf("404: err=%v want %v", err, ErrRejected)
	}
	if _, err := Register("127.0.0.1:1", "relay.test", "", KindInvite, []byte("x")); err == nil {
		t.Fatal("Register without a path must fail")
	}
}

func TestCredExpires(t *testing.T) {
	cred := make([]byte, 77)
	cred[9], cred[10], cred[11], cred[12] = 0x6B, 0x49, 0xD2, 0x00 // 1800000000
	if got := CredExpires(cred); got != 1800000000 {
		t.Fatalf("expires %d", got)
	}
	if CredExpires([]byte{1, 2}) != 0 {
		t.Fatal("short cred must give 0")
	}
}

// New sessions go to the new relay, the open one keeps its old relay, and
// the old relay's outage does not carry over.
func TestSetEndpoint_MovesNewSessionsOnly(t *testing.T) {
	freshHealth(t)
	_, gotA := fakeRelay(t)
	withConns(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	old, err := dialRelay(ctx, testCred, net.IPv4(1, 1, 1, 1), 443)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	<-gotA

	addrB, gotB := fakeHTTPRelay(t, true)
	now := time.Now()
	health.now = func() time.Time { return now }
	openBreaker(&health, &now)
	gen := health.generation()
	if err := SetEndpoint(addrB, "relay.test", "/app/y", false); err != nil {
		t.Fatal(err)
	}
	health.failed(gen, "connect", "tls: connect failed: late", errTimeout)
	if health.down() {
		t.Fatal("the old relay's outage carried over")
	}
	c, err := dialRelay(ctx, testCred, net.IPv4(1, 1, 1, 1), 443)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if req := <-gotB; req.URL.Path != "/app/y" {
		t.Fatalf("path %q", req.URL.Path)
	}

	old.SetDeadline(time.Now().Add(2 * time.Second))
	old.Write([]byte("ping"))
	buf := make([]byte, 4)
	if _, err := io.ReadFull(old, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("old session: %q %v", buf, err)
	}
	if SetEndpoint(addrB, "relay.test", "", false) == nil {
		t.Fatal("SetEndpoint without a path must fail")
	}
}

// For go test -race: switches while sessions dial.
func TestSetEndpoint_Concurrent(t *testing.T) {
	freshHealth(t)
	withEndpoint(t, "127.0.0.1:1", "relay.test", "/app/x")
	withConns(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				SetEndpoint(fmt.Sprintf("127.0.0.1:%d", 1+j%2), "relay.test", "/app/x", false)
			}
		}()
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				dialRelay(context.Background(), testCred, net.IPv4(1, 1, 1, 1), 443)
			}
		}()
	}
	wg.Wait()
}

// Lookups in flight return their conns to the DoH client's pool, so only a
// new client takes DNS off the old relay.
func TestSetEndpoint_NewDoHClient(t *testing.T) {
	freshHealth(t)
	withEndpoint(t, "127.0.0.1:1", "relay.test", "/app/x")
	addrB, gotB := fakeHTTPRelay(t, true)
	old := newDoHClient(testCred)
	mu.Lock()
	setConns(make(map[net.Conn]struct{}))
	doh, dohCred = old, testCred
	mu.Unlock()
	t.Cleanup(func() {
		mu.Lock()
		setConns(nil)
		doh, dohCred = nil, nil
		mu.Unlock()
	})

	if err := SetEndpoint(addrB, "relay.test", "/app/y", false); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	d := doh
	mu.Unlock()
	if d == old {
		t.Fatal("DoH kept its old client")
	}
	done := make(chan struct{})
	go func() { dohQuery(d, mustQuery(t, 1, "example.com.", dnsmessage.TypeA)); close(done) }()
	defer func() { <-done }()
	select {
	case req := <-gotB:
		if req.URL.Path != "/app/y" {
			t.Fatalf("path %q", req.URL.Path)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DoH did not reach the new relay")
	}
}

// handleTCP of a stopped tunnel outlives Stop and still untracks its conns
// while the next Start sets up the tracker.
func TestStart_WhileOldConnsUntrack(t *testing.T) {
	freshHealth(t)
	captureLog(t)
	t.Cleanup(func() { setConns(nil) })
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	c, peer := net.Pipe()
	defer c.Close()
	defer peer.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			trackConn(c)
			untrackConn(c)
		}
	}()
	for i := 0; i < 5; i++ {
		Stop()
		Start(9999, closed, "relay.test", "/app/x", testCred, "", "", "", nil, nil) // fails on the fake fd, after the tracker
	}
	<-done
}
