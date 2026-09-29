package tunnel

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net"
	"os"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

var (
	tunIP    = tcpip.AddrFrom4([4]byte{10, 255, 0, 1})
	fakeDNSA = tcpip.AddrFrom4([4]byte(fakeDNS))
	tgIP     = tcpip.AddrFrom4([4]byte{149, 154, 167, 99})
)

// testStack wires newStack to a channel endpoint standing in for the TUN fd.
func testStack(t *testing.T, vpnAddr string) (*stack.Stack, *channel.Endpoint) {
	t.Helper()
	ep := channel.New(64, 1500, "")
	withEndpoint(t, vpnAddr, "relay.test", "/app/x")
	ns, err := newStack(ep, testCred)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ns.Close(); ep.Close() })
	return ns, ep
}

func ipv4Pkt(proto tcpip.TransportProtocolNumber, src, dst tcpip.Address, transport []byte) []byte {
	b := make([]byte, header.IPv4MinimumSize+len(transport))
	ip := header.IPv4(b)
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(len(b)),
		TTL:         64,
		Protocol:    uint8(proto),
		SrcAddr:     src,
		DstAddr:     dst,
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	copy(b[header.IPv4MinimumSize:], transport)
	return b
}

func udpPkt(src, dst tcpip.Address, sport, dport uint16, payload []byte) []byte {
	b := make([]byte, header.UDPMinimumSize+len(payload))
	u := header.UDP(b)
	u.Encode(&header.UDPFields{SrcPort: sport, DstPort: dport, Length: uint16(len(b))})
	copy(b[header.UDPMinimumSize:], payload)
	xsum := header.PseudoHeaderChecksum(udp.ProtocolNumber, src, dst, uint16(len(b)))
	u.SetChecksum(^u.CalculateChecksum(checksum.Checksum(payload, xsum)))
	return ipv4Pkt(udp.ProtocolNumber, src, dst, b)
}

func tcpPkt(src, dst tcpip.Address, sport, dport uint16, seq, ack uint32, flags header.TCPFlags, payload []byte) []byte {
	b := make([]byte, header.TCPMinimumSize+len(payload))
	tc := header.TCP(b)
	tc.Encode(&header.TCPFields{
		SrcPort: sport, DstPort: dport,
		SeqNum: seq, AckNum: ack,
		DataOffset: header.TCPMinimumSize,
		Flags:      flags,
		WindowSize: 65535,
	})
	copy(b[header.TCPMinimumSize:], payload)
	xsum := header.PseudoHeaderChecksum(tcp.ProtocolNumber, src, dst, uint16(len(b)))
	tc.SetChecksum(^tc.CalculateChecksum(checksum.Checksum(payload, xsum)))
	return ipv4Pkt(tcp.ProtocolNumber, src, dst, b)
}

func inject(ep *channel.Endpoint, raw []byte) {
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(raw)})
	ep.InjectInbound(ipv4.ProtocolNumber, pkt)
	pkt.DecRef()
}

// readOut returns the next outbound packet split into IP header and
// transport bytes, or nil on timeout.
func readOut(ep *channel.Endpoint, timeout time.Duration) (header.IPv4, []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	pkt := ep.ReadContext(ctx)
	if pkt == nil {
		return nil, nil
	}
	raw := append([]byte(nil), pkt.ToView().AsSlice()...)
	pkt.DecRef()
	ip := header.IPv4(raw)
	return ip, raw[ip.HeaderLength():]
}

// readProto skips outbound packets until one with the given protocol shows up.
func readProto(t *testing.T, ep *channel.Endpoint, proto tcpip.TransportProtocolNumber) (header.IPv4, []byte) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ip, tr := readOut(ep, time.Until(deadline))
		if ip == nil {
			break
		}
		if tcpip.TransportProtocolNumber(ip.Protocol()) == proto {
			return ip, tr
		}
	}
	t.Fatalf("no outbound packet with protocol %d", proto)
	return nil, nil
}

func TestNetstack_DNSOverUDP(t *testing.T) {
	resetDNS()
	t.Cleanup(resetDNS)
	var calls atomic.Int32
	doh = fakeDoH(t, &calls, func(q []byte) ([]byte, error) {
		return mustAnswer(t, binary.BigEndian.Uint16(q[:2]), "stack.test.", 60, [4]byte{5, 6, 7, 8}), nil
	})
	_, ep := testStack(t, "127.0.0.1:1")

	q := mustQuery(t, 0xABCD, "stack.test.", dnsmessage.TypeA)
	inject(ep, udpPkt(tunIP, fakeDNSA, 40000, 53, q))

	ip, tr := readProto(t, ep, udp.ProtocolNumber)
	if ip.SourceAddress() != fakeDNSA || ip.DestinationAddress() != tunIP {
		t.Fatalf("bad addrs: %s -> %s", ip.SourceAddress(), ip.DestinationAddress())
	}
	u := header.UDP(tr)
	if u.SourcePort() != 53 || u.DestinationPort() != 40000 {
		t.Fatalf("bad ports: %d -> %d", u.SourcePort(), u.DestinationPort())
	}
	resp := u.Payload()
	if binary.BigEndian.Uint16(resp[:2]) != 0xABCD || rcode(resp) != dnsmessage.RCodeSuccess {
		t.Fatalf("bad dns response: % x", resp[:4])
	}
	var p dnsmessage.Parser
	if _, err := p.Start(resp); err != nil {
		t.Fatal(err)
	}
	p.SkipAllQuestions()
	a, err := p.AllAnswers()
	if err != nil || len(a) != 1 {
		t.Fatalf("answers: %v %v", a, err)
	}
	if got := a[0].Body.(*dnsmessage.AResource).A; got != [4]byte{5, 6, 7, 8} {
		t.Fatalf("A = %v", got)
	}
	if calls.Load() != 1 {
		t.Fatalf("doh calls = %d", calls.Load())
	}
}

func TestNetstack_UDPElsewhereIgnored(t *testing.T) {
	resetDNS()
	t.Cleanup(resetDNS)
	var calls atomic.Int32
	doh = fakeDoH(t, &calls, func(q []byte) ([]byte, error) { t.Error("doh must not be called"); return nil, nil })
	_, ep := testStack(t, "127.0.0.1:1")

	q := mustQuery(t, 1, "other.test.", dnsmessage.TypeA)
	inject(ep, udpPkt(tunIP, fakeDNSA, 40001, 5353, q))
	inject(ep, udpPkt(tunIP, tcpip.AddrFrom4([4]byte{10, 255, 0, 3}), 40002, 53, q))

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		ip, _ := readOut(ep, time.Until(deadline))
		if ip == nil {
			break
		}
		if ip.Protocol() != uint8(header.ICMPv4ProtocolNumber) {
			t.Fatalf("unexpected outbound protocol %d", ip.Protocol())
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("doh called %d times", calls.Load())
	}
}

// tcpPeer is a hand-driven TCP client behind the channel endpoint.
type tcpPeer struct {
	ep       *channel.Endpoint
	dst      tcpip.Address
	dport    uint16
	seq, ack uint32
}

func (p *tcpPeer) send(flags header.TCPFlags, payload []byte) {
	inject(p.ep, tcpPkt(tunIP, p.dst, 50000, p.dport, p.seq, p.ack, flags, payload))
	p.seq += uint32(len(payload))
}

func (p *tcpPeer) recv(t *testing.T) header.TCP {
	t.Helper()
	for {
		ip, tr := readProto(t, p.ep, tcp.ProtocolNumber)
		tc := header.TCP(tr)
		if ip.SourceAddress() != p.dst || tc.SourcePort() != p.dport || tc.DestinationPort() != 50000 {
			t.Fatalf("stray segment %s:%d -> :%d", ip.SourceAddress(), tc.SourcePort(), tc.DestinationPort())
		}
		return tc
	}
}

func handshake(t *testing.T, ep *channel.Endpoint, dst tcpip.Address, dport uint16) *tcpPeer {
	t.Helper()
	p := &tcpPeer{ep: ep, dst: dst, dport: dport, seq: 1000}
	p.send(header.TCPFlagSyn, nil)
	p.seq++
	sa := p.recv(t)
	if sa.Flags() != header.TCPFlagSyn|header.TCPFlagAck || sa.AckNumber() != p.seq {
		t.Fatalf("expected SYN-ACK acking %d, got flags=%v ack=%d", p.seq, sa.Flags(), sa.AckNumber())
	}
	p.ack = sa.SequenceNumber() + 1
	p.send(header.TCPFlagAck, nil)
	return p
}

func TestNetstack_TCPRelayedThroughFakeRelay(t *testing.T) {
	addr, got := fakeRelay(t)
	withConns(t)
	_, ep := testStack(t, addr)

	p := handshake(t, ep, tgIP, 443)
	p.send(header.TCPFlagAck|header.TCPFlagPsh, []byte("hello"))

	select {
	case hdr := <-got:
		if hdr[0] != 0x01 || !bytes.Equal(hdr[1:5], tgIP.AsSlice()) || binary.BigEndian.Uint16(hdr[5:7]) != 443 {
			t.Fatalf("bad header: % x", hdr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("relay never got the header")
	}

	// fakeRelay echoes: the stack must deliver "hello" back to the client.
	var echoed []byte
	for len(echoed) < 5 {
		tc := p.recv(t)
		if tc.Flags()&header.TCPFlagRst != 0 {
			t.Fatal("got RST")
		}
		if pl := tc.Payload(); len(pl) > 0 && tc.SequenceNumber() == p.ack {
			echoed = append(echoed, pl...)
			p.ack += uint32(len(pl))
			p.send(header.TCPFlagAck, nil)
		}
	}
	if string(echoed) != "hello" {
		t.Fatalf("echo = %q", echoed)
	}
	p.send(header.TCPFlagAck|header.TCPFlagFin, nil)
}

func TestStop_ClosesEverything(t *testing.T) {
	_, got := fakeRelay(t)
	// Start()-like init minus the TLS probe and fdbased.
	ep := channel.New(64, 1500, "")
	ns, err := newStack(ep, testCred)
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	mu.Lock()
	setConns(make(map[net.Conn]struct{}))
	doh = fakeDoH(t, &calls, func([]byte) ([]byte, error) { return nil, nil })
	s = ns
	mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tlsConn, err := dialRelay(ctx, testCred, net.IPv4(1, 1, 1, 1), 443)
	if err != nil {
		t.Fatal(err)
	}
	<-got

	// Stand-in for the gonet side of relay(): a tracked loopback conn.
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	defer ln.Close()
	appConn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	peer, _ := ln.Accept()
	defer peer.Close()
	trackConn(appConn)

	done := make(chan struct{})
	go func() { relay(appConn, tlsConn); close(done) }()
	select {
	case <-done:
		t.Fatal("relay returned before Stop")
	case <-time.After(100 * time.Millisecond):
	}

	Stop()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("relay() still blocked after Stop")
	}
	mu.Lock()
	stopped := s == nil && doh == nil && tunFile == nil
	mu.Unlock()
	if !stopped {
		t.Fatal("globals not cleared")
	}
	connsMu.Lock()
	ac := activeConns
	connsMu.Unlock()
	if ac != nil {
		t.Fatal("activeConns not nil")
	}
	if trackConn(peer) {
		t.Fatal("new conns must be refused after Stop")
	}
	// Stack is closed: injections are dropped without panicking.
	inject(ep, udpPkt(tunIP, fakeDNSA, 40000, 53, mustQuery(t, 1, "x.test.", dnsmessage.TypeA)))
	if calls.Load() != 0 {
		t.Fatal("doh used after Stop")
	}
	Stop()
	ep.Close()
}

// Android's opportunistic Private DNS probes DoT (853) on the DNS server it
// was given, i.e. our fake resolver. That must be refused locally with a
// RST, not forwarded to the relay as a TCP session to 10.255.0.2.
func TestNetstack_FakeDNSOtherPortsRefusedLocally(t *testing.T) {
	addr, got := fakeRelay(t)
	withConns(t)
	_, ep := testStack(t, addr)

	p := &tcpPeer{ep: ep, dst: fakeDNSA, dport: 853, seq: 1000}
	p.send(header.TCPFlagSyn, nil)
	tc := p.recv(t)
	if tc.Flags()&header.TCPFlagRst == 0 {
		t.Fatalf("expected RST, got flags=%v", tc.Flags())
	}
	select {
	case pre := <-got:
		t.Fatalf("relay got a preamble for the fake DNS: % x", pre[32:])
	case <-time.After(500 * time.Millisecond):
	}
}

// A ClientHello for a site that is not on the list is dialed directly
// (here: a local echo), the relay never hears about it, and the hello
// bytes that were peeked arrive at the destination intact.
func TestNetstack_UnlistedNameGoesDirect(t *testing.T) {
	addr, got := fakeRelay(t)
	withConns(t)
	setDomains("telegram.org")
	t.Cleanup(func() { setDomains("") })
	fp := &fakeProtector{ok: true}
	protector = fp
	t.Cleanup(func() { protector = nil })
	_, ep := testStack(t, addr)

	// gVisor drops packets to loopback ("martian"): use a real interface.
	var hostIP net.IP
	if addrs, _ := net.InterfaceAddrs(); true {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLoopback() && ipn.IP.To4() != nil {
				hostIP = ipn.IP.To4()
				break
			}
		}
	}
	if hostIP == nil {
		t.Skip("no non-loopback IPv4 interface")
	}
	echo, err := net.Listen("tcp", hostIP.String()+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer echo.Close()
	go func() {
		c, err := echo.Accept()
		if err != nil {
			return
		}
		io.Copy(c, c)
		c.Close()
	}()
	ea := echo.Addr().(*net.TCPAddr)
	hello := clientHello(t, "example.com")

	p := handshake(t, ep, tcpip.AddrFrom4([4]byte(ea.IP.To4())), uint16(ea.Port))
	p.send(header.TCPFlagAck|header.TCPFlagPsh, hello)

	var echoed []byte
	deadline := time.Now().Add(5 * time.Second)
	for len(echoed) < len(hello) && time.Now().Before(deadline) {
		tc := p.recv(t)
		if tc.Flags()&header.TCPFlagRst != 0 {
			t.Fatal("got RST")
		}
		if pl := tc.Payload(); len(pl) > 0 && tc.SequenceNumber() == p.ack {
			echoed = append(echoed, pl...)
			p.ack += uint32(len(pl))
			p.send(header.TCPFlagAck, nil)
		}
	}
	if !bytes.Equal(echoed, hello) {
		t.Fatalf("echoed %d of %d bytes", len(echoed), len(hello))
	}
	if n := len(fp.protected()); n != 1 {
		t.Fatalf("protect called %d times", n)
	}
	select {
	case <-got:
		t.Fatal("relay was dialed for a direct session")
	default:
	}
	p.send(header.TCPFlagAck|header.TCPFlagFin, nil)
}

// The fdbased reader sleeps in ppoll on the TUN fd, and a sleeping ppoll pins
// the file past close(): Android keeps the interface (and the VPN network) up
// until some packet happens to wake it.
func TestStop_StopsTheTunReader(t *testing.T) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	// The kernel side of the TUN stays open and silent, like an idle phone.
	defer syscall.Close(fds[1])
	ep, err := fdbased.New(&fdbased.Options{FDs: []int{fds[0]}, MTU: 1500})
	if err != nil {
		t.Fatal(err)
	}
	ns, err := newStack(ep, testCred)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	setConns(make(map[net.Conn]struct{}))
	tunFile = os.NewFile(uintptr(fds[0]), "tun")
	s = ns
	mu.Unlock()
	// Let the reader park in ppoll: a close() before that fails it with EBADF.
	time.Sleep(200 * time.Millisecond)

	Stop()

	done := make(chan struct{})
	go func() { ep.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("TUN reader still running after Stop")
	}
}
