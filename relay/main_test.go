package main

import (
	"bufio"
	"crypto/ed25519"
	"encoding/binary"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

// testCred is a valid credential for the key pair installed in TestMain.
var testCred []byte

func TestMain(m *testing.M) {
	pub, priv, _ := ed25519.GenerateKey(nil)
	credPub, issuerPriv = pub, priv
	testCred = issueCred(kindIntegrity, time.Now().Add(time.Hour))
	testLoopback = true
	m.Run()
}

func TestIsAllowedDest(t *testing.T) {
	cases := map[string]bool{
		"149.154.167.99":  true, // Telegram
		"1.1.1.1":         true, // DoH
		"162.159.140.229": true, // Cloudflare, any of it
		"8.8.8.8":         true,
		"10.0.0.1":        false, // private
		"172.18.0.5":      false, // the compose network
		"192.168.1.1":     false,
		"169.254.1.1":     false,
		"100.64.0.1":      false, // CGNAT
		"224.0.0.1":       false,
	}
	for ip, want := range cases {
		if got := isAllowedDest(net.ParseIP(ip), 443); got != want {
			t.Errorf("%s: got %v want %v", ip, got, want)
		}
	}
	if isAllowedDest(net.ParseIP("8.8.8.8"), 25) {
		t.Error("smtp allowed")
	}
	testLoopback = false
	defer func() { testLoopback = true }()
	if isAllowedDest(net.ParseIP("127.0.0.1"), 443) {
		t.Error("loopback allowed outside tests")
	}
}

func startRelay(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go serve(ln)
	return ln.Addr().String()
}

// withLimits sets the limits for one test and restores them after.
func withLimits(t *testing.T, global, perIP int, proxy bool) {
	t.Helper()
	// Sessions from earlier tests are still tearing down.
	for d := time.Now().Add(2 * time.Second); activeConns.Load() != 0 && time.Now().Before(d); {
		time.Sleep(10 * time.Millisecond)
	}
	g, p, pp := maxConns, maxConnsPerIP, proxyProto
	maxConns, maxConnsPerIP, proxyProto = global, perIP, proxy
	t.Cleanup(func() { maxConns, maxConnsPerIP, proxyProto = g, p, pp })
}

// holdConn opens a relayed echo session and keeps it open; returns the conn
// after the echo proved the session is established.
func holdConn(t *testing.T, relayAddr string, pre []byte) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", relayAddr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write(pre)
	c.Write([]byte("ping"))
	br := bufio.NewReader(c)
	if res, err := http.ReadResponse(br, nil); err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("session not established: %v %v", res, err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(br, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("session not established: %q %v", buf, err)
	}
	return c
}

func proxyLine(src string) []byte {
	return []byte("PROXY TCP4 " + src + " 10.9.9.9 40000 4460\r\n")
}

func startEcho(t *testing.T) (net.IP, uint16) {
	t.Helper()
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
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	a := ln.Addr().(*net.TCPAddr)
	return a.IP.To4(), uint16(a.Port)
}

// preamble is the upgrade request for cred followed by the 7-byte header:
// the relay reads the header from the same buffer, so a client may send
// both at once (nginx would hold the header until the 101 anyway).
func preamble(cred []byte, typ byte, ip net.IP, port uint16) []byte {
	hdr := [headerSize]byte{typ}
	copy(hdr[1:5], ip.To4())
	binary.BigEndian.PutUint16(hdr[5:], port)
	return append([]byte(upgradeReq(cred)), hdr[:]...)
}

// dialAndSend returns what the relay echoed after a 101; nil when the
// relay answered anything else or closed.
func dialAndSend(t *testing.T, relayAddr string, pre, payload []byte) []byte {
	t.Helper()
	c, err := net.Dial("tcp", relayAddr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write(pre)
	if payload != nil {
		c.Write(payload)
	}
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil || res.StatusCode != http.StatusSwitchingProtocols {
		return nil
	}
	buf := make([]byte, 64)
	n, _ := io.ReadAtLeast(br, buf, len(payload))
	return buf[:n]
}

func TestRelay_EchoThroughRelay(t *testing.T) {
	relay := startRelay(t)
	ip, port := startEcho(t)
	got := dialAndSend(t, relay, preamble(testCred, typeTCP, ip, port), []byte("hello"))
	if string(got) != "hello" {
		t.Fatalf("got %q", got)
	}
}

func TestRelay_RejectsBadCredential(t *testing.T) {
	relay := startRelay(t)
	ip, port := startEcho(t)
	bad := append([]byte{}, testCred...)
	bad[20] ^= 1
	got := dialAndSend(t, relay, preamble(bad, typeTCP, ip, port), []byte("hello"))
	if len(got) != 0 {
		t.Fatalf("relayed data with bad credential: %q", got)
	}
}

func TestRelay_RejectsBlockedDest(t *testing.T) {
	relay := startRelay(t)
	got := dialAndSend(t, relay, preamble(testCred, typeTCP, net.IPv4(10, 1, 2, 3), 443), []byte("hello"))
	if len(got) != 0 {
		t.Fatalf("relayed to blocked dest: %q", got)
	}
}

func TestRelay_RejectsUDPAndUnknownType(t *testing.T) {
	relay := startRelay(t)
	ip, port := startEcho(t)
	for _, typ := range []byte{typeUDP, 0x7F} {
		if got := dialAndSend(t, relay, preamble(testCred, typ, ip, port), []byte("x")); len(got) != 0 {
			t.Fatalf("type %#x relayed: %q", typ, got)
		}
	}
}

func TestRelay_GlobalLimitRejectsWithCounter(t *testing.T) {
	withLimits(t, 1, 0, false)
	relay := startRelay(t)
	ip, port := startEcho(t)
	pre := preamble(testCred, typeTCP, ip, port)

	first := holdConn(t, relay, pre)
	before := rejected.Load()
	if got := dialAndSend(t, relay, pre, []byte("x")); len(got) != 0 {
		t.Fatalf("second conn relayed over the limit: %q", got)
	}
	if rejected.Load() != before+1 {
		t.Fatalf("rejected counter %d, want %d", rejected.Load(), before+1)
	}

	first.Close()
	// Slot freed: the next one goes through.
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := dialAndSend(t, relay, pre, []byte("y")); string(got) == "y" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("slot not released after close")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRelay_ProxyProtocolPerIPLimit(t *testing.T) {
	withLimits(t, 100, 1, true)
	relay := startRelay(t)
	ip, port := startEcho(t)
	pre := preamble(testCred, typeTCP, ip, port)

	a := holdConn(t, relay, append(proxyLine("10.0.0.1"), pre...))
	if got := dialAndSend(t, relay, append(proxyLine("10.0.0.1"), pre...), []byte("x")); len(got) != 0 {
		t.Fatalf("second conn from same IP relayed: %q", got)
	}
	// Another client is not affected.
	holdConn(t, relay, append(proxyLine("10.0.0.2"), pre...))

	a.Close()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := dialAndSend(t, relay, append(proxyLine("10.0.0.1"), pre...), []byte("y")); string(got) == "y" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("per-IP slot not released after close")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRelay_ProxyProtocolRequiredWhenEnabled(t *testing.T) {
	withLimits(t, 100, 10, true)
	relay := startRelay(t)
	ip, port := startEcho(t)
	// No PROXY line: the preamble must not be taken as one, and nothing relayed.
	if got := dialAndSend(t, relay, preamble(testCred, typeTCP, ip, port), []byte("x")); len(got) != 0 {
		t.Fatalf("relayed without PROXY line: %q", got)
	}
}

func TestRelay_PerIPIgnoredWithoutProxyProtocol(t *testing.T) {
	// Without PROXY protocol every client looks like nginx: a per-IP cap
	// would be a global cap in disguise.
	withLimits(t, 100, 1, false)
	relay := startRelay(t)
	ip, port := startEcho(t)
	pre := preamble(testCred, typeTCP, ip, port)
	holdConn(t, relay, pre)
	holdConn(t, relay, pre)
}
