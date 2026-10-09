package tunnel

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// echoRelay is fakeRelay for any number of sessions; it counts them.
func echoRelay(t *testing.T) (addr string, sessions *atomic.Int32) {
	t.Helper()
	ln := fakeTLSListener(t)
	withEndpoint(t, ln.Addr().String(), "relay.test", "/app/x")
	var n atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				if _, err := http.ReadRequest(br); err != nil {
					return
				}
				io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
				if _, err := io.ReadFull(br, make([]byte, 7)); err != nil {
					return
				}
				n.Add(1)
				io.Copy(c, br)
			}()
		}
	}()
	return ln.Addr().String(), &n
}

// fakeDPI stands for a provider's box in front of YouTube: silent while
// the name lies whole in one TLS record, else the server answers.
func fakeDPI(t *testing.T, ip net.IP, name string, helloLen int) (port uint16) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
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
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				var b []byte
				buf := make([]byte, 4096)
				for len(unrecord(b)) < helloLen {
					n, err := c.Read(buf)
					b = append(b, buf[:n]...)
					if err != nil {
						return
					}
				}
				for _, r := range records(b) {
					if bytes.Contains(r, []byte(name)) {
						io.Copy(io.Discard, c)
						return
					}
				}
				c.Write([]byte{0x16, 3, 3, 0, 2, 2, 0})
			}()
		}
	}()
	return uint16(ln.Addr().(*net.TCPAddr).Port)
}

// receive reads n payload bytes from the stack, acking them.
func (p *tcpPeer) receive(t *testing.T, n int) []byte {
	t.Helper()
	var got []byte
	deadline := time.Now().Add(5 * time.Second)
	for len(got) < n && time.Now().Before(deadline) {
		tc := p.recv(t)
		if tc.Flags()&header.TCPFlagRst != 0 {
			t.Fatal("got RST")
		}
		if pl := tc.Payload(); len(pl) > 0 && tc.SequenceNumber() == p.ack {
			got = append(got, pl...)
			p.ack += uint32(len(pl))
			p.send(header.TCPFlagAck, nil)
		}
	}
	return got
}

// Through handleTCP: "none" stays silent under the fake DPI and the
// connection goes on by the relay; rec2 cuts the name and gets through.
// The app gets an answer either way.
func TestHandleTCP_FakeDPI(t *testing.T) {
	relayAddr, sessions := echoRelay(t)
	withConns(t)
	captureLog(t)
	setDomains("youtube.com")
	t.Cleanup(func() { setDomains("") })
	defer func(w time.Duration) { desyncWait = w }(desyncWait)
	desyncWait = 300 * time.Millisecond
	p, _, _ := picking(t)
	p.now = time.Now // tryDesync's start times are real ones
	SetStrategies(`[{"id":"none","spec":""},{"id":"rec2","spec":"rec=host+1,midsld;cut=host+1,midsld"}]`)
	p.setNetKey("cell:25002")
	_, ep := testStack(t, relayAddr)

	hostIP := hostIPv4(t)
	const name = "www.youtube.com"
	hello := clientHello(t, name)
	port := fakeDPI(t, hostIP, name, len(unrecord(hello)))
	dst := tcpip.AddrFrom4([4]byte(hostIP))

	var peers []*tcpPeer
	// The sessions end before the test does: they report to the picker.
	defer func() {
		for _, c := range peers {
			c.send(header.TCPFlagRst|header.TCPFlagAck, nil)
		}
		// The relay never hears the app's end (no half-close over TLS).
		connsMu.Lock()
		var open []net.Conn
		for c := range activeConns {
			open = append(open, c)
		}
		connsMu.Unlock()
		for _, c := range open {
			c.Close()
		}
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			connsMu.Lock()
			n := len(activeConns)
			connsMu.Unlock()
			if n == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Error("sessions still open")
	}()
	conn := func(sport uint16, want int) []byte {
		c := handshakeFrom(t, ep, dst, port, sport)
		peers = append(peers, c)
		for b := hello; len(b) > 0; {
			n := min(len(b), 1000)
			c.send(header.TCPFlagAck|header.TCPFlagPsh, b[:n])
			b = b[n:]
		}
		return c.receive(t, want)
	}
	for i, sport := range []uint16{50001, 50002} {
		if got := conn(sport, len(hello)); !bytes.Equal(got, hello) {
			t.Fatalf("none #%d: app got %d bytes, want the relay's echo", i+1, len(got))
		}
	}
	if n := sessions.Load(); n != 2 {
		t.Fatalf("relay sessions %d", n)
	}
	if p.curID() != "rec2" {
		t.Fatalf("after two silences: %s", p.curID())
	}
	if got := conn(50003, 7); !bytes.Equal(got, []byte{0x16, 3, 3, 0, 2, 2, 0}) {
		t.Fatalf("rec2: app got % x", got)
	}
	if n := sessions.Load(); n != 2 {
		t.Fatalf("rec2 went by the relay too: %d sessions", n)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.ok.has(hostIP.String()) {
		t.Fatal("rec2's success not counted")
	}
}

// YouTube's names: DoH through the relay as before; the network's resolver
// only while the relay is down and a strategy gets through here, and its
// answers go once the relay is back.
func TestResolveDNS_YouTubeByTheNetworkOnlyWhileTheRelayIsDown(t *testing.T) {
	p, _, _ := picking(t)
	freshHealth(t)
	fakeResolver(t, "127.0.0.1", nil) // 5.5.5.5
	dohCalls := splitDNS(t, "127.0.0.1")
	setDomains("youtube.com")
	t.Cleanup(func() { dnsCache.Clear() })
	q := mustQuery(t, 7, "www.youtube.com.", dnsmessage.TypeA)
	answer := func() [4]byte {
		t.Helper()
		var m dnsmessage.Message
		if err := m.Unpack(resolveDNS(q)); err != nil || len(m.Answers) != 1 {
			t.Fatalf("answer %+v, %v", m, err)
		}
		return m.Answers[0].Body.(*dnsmessage.AResource).A
	}
	setWon := func(won bool) {
		p.mu.Lock()
		p.won = won
		p.mu.Unlock()
	}
	setWon(true)
	if a := answer(); a != [4]byte{9, 9, 9, 9} || dohCalls.Load() != 1 {
		t.Fatalf("relay up: %v", a)
	}
	dnsCache.Clear()
	health.mu.Lock()
	health.fails = 1
	health.mu.Unlock()
	setWon(false)
	if a := answer(); a != [4]byte{9, 9, 9, 9} {
		t.Fatalf("relay down, nothing works here: %v", a)
	}
	dnsCache.Clear()
	setWon(true)
	if a := answer(); a != [4]byte{5, 5, 5, 5} {
		t.Fatalf("relay down, a winner: %v", a)
	}
	health.ok(health.generation())
	if a := answer(); a != [4]byte{9, 9, 9, 9} {
		t.Fatalf("relay back, the network's answer still cached: %v", a)
	}

	// Back through Stop and Start: the tracker forgot the outage, the
	// cache did not.
	dnsCache.Clear()
	health.mu.Lock()
	health.fails = 1
	health.mu.Unlock()
	if a := answer(); a != [4]byte{5, 5, 5, 5} {
		t.Fatalf("relay down again: %v", a)
	}
	health.stopped()
	health.ok(health.generation())
	if a := answer(); a != [4]byte{9, 9, 9, 9} {
		t.Fatalf("back after a restart, the network's answer still cached: %v", a)
	}
}

// rr*---sn-*.googlevideo.com are hundreds of names: one entry for them all.
func TestRouteCache_YouTubeBySuffix(t *testing.T) {
	setDomains("googlevideo.com\nx.com")
	t.Cleanup(func() { setDomains("") })
	c := newRouteCache("", "10.0.0.0/8", nil)
	c.observe("rr1---sn-abc.googlevideo.com.", []net.IP{net.IPv4(10, 0, 0, 1)})
	c.observe("rr5---sn-xyz.googlevideo.com", []net.IP{net.IPv4(10, 0, 1, 1)})
	c.observe("api.x.com", []net.IP{net.IPv4(10, 0, 2, 1)})
	if len(c.sites) != 2 || len(c.sites["googlevideo.com"]) != 2 || c.sites["api.x.com"] == nil {
		t.Fatalf("sites %v", c.sites)
	}
}
