package tunnel

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
	"gvisor.dev/gvisor/pkg/waiter"
)

// Fake resolver address inside the TUN. Must match TunnelVpnService.
var fakeDNS = net.IPv4(10, 255, 0, 2).To4()

var (
	mu          sync.Mutex
	rc          *routeCache
	s           *stack.Stack
	linkEP      stack.LinkEndpoint
	tunFile     *os.File
	connsMu     sync.Mutex
	activeConns map[net.Conn]struct{}
	doh         *http.Client
	dohCred     []byte // for SetEndpoint's new doh
	proberStop  chan struct{}
)

const (
	KindIntegrity = 1 // proof = 32-byte nonce + Play Integrity token
	KindInvite    = 2 // proof = one-shot invite code

	credSize = 77
)

var (
	ErrRejected  = errors.New("registration rejected")
	ErrNotIssuer = errors.New("relay is not an issuer")
	// The relay answered 404 to our credential: expired or revoked.
	ErrCredRejected = errors.New("credential rejected")
)

// The relay sits behind the cover site's nginx at a secret path. Sessions
// are a WebSocket upgrade (credential in the subprotocol header, raw bytes
// after 101, no WS framing); registration is a POST. See relay/http.go.
type relayTarget struct{ addr, sni, path string }

// relayEP is read by every new relay session, so SetEndpoint moves them
// without touching sessions already open.
var relayEP atomic.Pointer[relayTarget]

func wsKey() string {
	var k [16]byte
	rand.Read(k[:])
	return base64.StdEncoding.EncodeToString(k[:])
}

const chromeUA = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36"

// browserHeaders makes the request look like Chrome's and pads it: the
// cookie's random length (padMin..padMax) hides the otherwise constant size
// of our first TLS record. Only the length is visible from outside.
func browserHeaders(sni string) string {
	var n [1]byte
	rand.Read(n[:])
	pad := make([]byte, padMin+int(n[0])*(padMax-padMin)/255)
	rand.Read(pad)
	return "User-Agent: " + chromeUA + "\r\nOrigin: https://" + sni + "\r\n" +
		"Accept-Encoding: gzip, deflate, br\r\nAccept-Language: ru-RU,ru;q=0.9,en-US;q=0.8\r\n" +
		"Cache-Control: no-cache\r\nPragma: no-cache\r\n" +
		"Cookie: sid=" + base64.RawURLEncoding.EncodeToString(pad) + "\r\n"
}

const (
	padMin = 16
	padMax = 256
)

// httpRoundTrip sends one request over c and parses the response. The
// returned reader holds bytes the relay sent right after the headers.
func httpRoundTrip(c net.Conn, req string) (*http.Response, *bufio.Reader, error) {
	if _, err := io.WriteString(c, req); err != nil {
		return nil, nil, err
	}
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("relay reply: %v", err)
	}
	return res, br, nil
}

// upgrade does the v3 handshake on c: 101 means the credential is accepted.
func upgrade(c net.Conn, ep *relayTarget, cred []byte) (*bufio.Reader, error) {
	req := "GET " + ep.path + " HTTP/1.1\r\nHost: " + ep.sni + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
		browserHeaders(ep.sni) +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + wsKey() + "\r\n" +
		"Sec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\n" +
		"Sec-WebSocket-Protocol: " + hex.EncodeToString(cred) + "\r\n\r\n"
	res, br, err := httpRoundTrip(c, req)
	if err != nil {
		return nil, err
	}
	// 404 is the relay's own answer to a bad credential (nginx-like, see
	// relay/http.go). Anything else (502/504) is nginx with no relay behind
	// it: an outage, or the prober would call a dead relay "back".
	switch res.StatusCode {
	case http.StatusSwitchingProtocols:
		return br, nil
	case http.StatusNotFound:
		return nil, ErrCredRejected
	default:
		return nil, fmt.Errorf("relay reply: http %d", res.StatusCode)
	}
}

// bufConn reads through the bufio.Reader that parsed the 101. It is the
// relay session past the handshake, so it counts the quota both ways.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	countQuota(n)
	return n, err
}

func (c *bufConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	countQuota(n)
	return n, err
}

// CredExpires returns the credential's expiry as unix seconds, 0 if malformed.
func CredExpires(cred []byte) int64 {
	if len(cred) != credSize {
		return 0
	}
	return int64(binary.BigEndian.Uint32(cred[9:13]))
}

// Register asks the issuer relay for a credential. kind is KindIntegrity or
// KindInvite, proof its payload. Does not need the stack: call before Start.
func Register(vpnAddr, sni, path string, kind int, proof []byte) ([]byte, error) {
	if len(proof) == 0 || len(proof) > 16*1024 {
		return nil, fmt.Errorf("bad proof length %d", len(proof))
	}
	if path == "" {
		return nil, errors.New("no relay path")
	}
	// Any endpoint may be asked (anyEndpoint), so its failures say nothing
	// about the one the tunnel uses.
	c, err := dialTLS(context.Background(), notCounted, vpnAddr, sni)
	if err != nil {
		return nil, fmt.Errorf("relay unreachable: %v", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(20 * time.Second))
	body := append([]byte{byte(kind)}, proof...)
	res, r, err := httpRoundTrip(c, fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\n%sContent-Type: application/octet-stream\r\nContent-Length: %d\r\n\r\n%s",
		path, sni, browserHeaders(sni), len(body), body))
	if err != nil {
		return nil, err
	}
	// Refusals, including not-an-issuer, are the site's 404.
	if res.StatusCode == http.StatusNotFound {
		return nil, ErrRejected
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("register: http %d", res.StatusCode)
	}
	var st [2]byte
	if _, err := io.ReadFull(r, st[:]); err != nil {
		return nil, fmt.Errorf("register reply: %v", err)
	}
	switch st[0] {
	case 0:
	case 1:
		return nil, ErrRejected
	case 2:
		return nil, ErrNotIssuer
	default:
		return nil, fmt.Errorf("register status %d", st[0])
	}
	cred := make([]byte, st[1])
	if _, err := io.ReadFull(r, cred); err != nil {
		return nil, fmt.Errorf("register reply: %v", err)
	}
	if len(cred) != credSize {
		return nil, fmt.Errorf("register: credential size %d", len(cred))
	}
	return cred, nil
}

// vpnAddr is "ip:port" — the caller resolves the host before the TUN takes
// over DNS, otherwise DoH would loop through our own resolver. sni is the
// hostname for TLS verification, path the relay's location on the cover
// site, cred a credential from Register.
// domains is a newline-separated list of site suffixes that go through the
// relay; any other name inside the routed subnets is dialed directly via
// host. Sessions without a name (MTProto, plain TCP) go through the relay.
// routes are the TUN's subnets ("a.b.c.d/n" per line) and cacheFile the
// route cache (see routes.go); host hears when a listed site resolves
// outside routes.
func Start(tunFd int, vpnAddr string, sni string, path string, cred []byte, domains string, routes string, cacheFile string, host Host, logger Logger) error {
	setLogger(logger)
	setDomains(domains)
	protector = host
	// SetQuota before the first Start had no one to tell.
	if overQuota() && host != nil {
		go host.QuotaExceeded()
	}
	routeCacheSet(newRouteCache(cacheFile, routes, host))
	if len(cred) != credSize {
		return fmt.Errorf("no credential")
	}
	if path == "" {
		return errors.New("no relay path")
	}
	// gomobile lends the Java array only for this call; every later
	// connection reads cred, so keep our own copy.
	cred = append([]byte(nil), cred...)
	relayEP.Store(&relayTarget{vpnAddr, sni, path})

	// Probe the relay before touching TUN. The probe is a full handshake:
	// ErrCredRejected tells the caller to register again instead of showing
	// "connected" to a 404. An unreachable relay is not an error: no network
	// yet, or one that blocks the VDS, heals like any outage, and the host
	// hears RelayDown meanwhile.
	firstHello.Store(false)
	health.newNetwork()
	wake := health.setHost(host)
	gen := health.generation()
	if err := probeRelay(cred); errors.Is(err, ErrCredRejected) {
		return err
	} else if err != nil {
		health.unreachableAtStart()
	} else {
		health.accepted()
		health.ok(gen)
	}

	mu.Lock()
	if s != nil {
		mu.Unlock()
		return fmt.Errorf("already running")
	}

	// handleTCP of a stopped tunnel may still be untracking.
	connsMu.Lock()
	activeConns = make(map[net.Conn]struct{})
	connsMu.Unlock()

	ep, err := fdbased.New(&fdbased.Options{
		FDs: []int{tunFd},
		MTU: 1500,
	})
	if err != nil {
		mu.Unlock()
		return fmt.Errorf("fdbased: %v", err)
	}

	ns, err := newStack(ep, cred)
	if err != nil {
		mu.Unlock()
		return err
	}

	// Only now take ownership of the fd: on any error above the caller still
	// owns and closes it, and an *os.File left behind would close the same
	// number again from its finalizer once Android reuses it.
	tunFile = os.NewFile(uintptr(tunFd), "tun")
	doh, dohCred = newDoHClient(cred), cred
	s = ns
	linkEP = ep
	proberStop = make(chan struct{})
	go health.prober(proberStop, wake, func() error { return probeRelay(cred) })
	mu.Unlock()
	return nil
}

// probeRelay is a full handshake and nothing more. Its failures count as
// dials: dialTLS records its own, the upgrade is recorded here as in
// dialRelay, so a pocketed phone's outage still has its error kinds.
func probeRelay(cred []byte) error {
	gen := health.generation()
	ep := relayEP.Load()
	probe, err := dialTLS(context.Background(), gen, ep.addr, ep.sni)
	if err != nil {
		return err
	}
	defer probe.Close()
	probe.SetDeadline(time.Now().Add(handshakeTimeout))
	if _, err = upgrade(probe, ep, cred); err != nil && !errors.Is(err, ErrCredRejected) {
		health.failed(gen, "upgrade", fmt.Sprintf("relay: upgrade failed: %v", err), err)
	}
	return err
}

func routeCacheSet(c *routeCache) {
	mu.Lock()
	rc = c
	mu.Unlock()
}

const nicID tcpip.NICID = 1

// newStack builds the netstack over ep and installs the TCP/UDP forwarders.
// Split from Start so tests can drive it with a channel endpoint.
func newStack(ep stack.LinkEndpoint, cred []byte) (*stack.Stack, error) {
	ns := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})

	if tcpErr := ns.CreateNIC(nicID, ep); tcpErr != nil {
		ns.Close()
		return nil, fmt.Errorf("create NIC: %v", tcpErr)
	}

	ns.SetRouteTable([]tcpip.Route{{
		Destination: header.IPv4EmptySubnet,
		NIC:         nicID,
	}})

	ns.SetPromiscuousMode(nicID, true)
	ns.SetSpoofing(nicID, true)

	tcpForwarder := tcp.NewForwarder(ns, 0, 256, func(r *tcp.ForwarderRequest) {
		id := r.ID()
		go handleTCP(r, id, cred)
	})
	ns.SetTransportProtocolHandler(tcp.ProtocolNumber, tcpForwarder.HandlePacket)

	udpForwarder := udp.NewForwarder(ns, func(r *udp.ForwarderRequest) bool {
		id := r.ID()
		if id.LocalPort != 53 || id.LocalAddress.As4() != [4]byte(fakeDNS) {
			return false
		}
		var wq waiter.Queue
		ep, err := r.CreateEndpoint(&wq)
		if err != nil {
			log.Printf("udp dns endpoint: %v", err)
			return true
		}
		go serveDNSUDP(gonet.NewUDPConn(&wq, ep))
		return true
	})
	ns.SetTransportProtocolHandler(udp.ProtocolNumber, udpForwarder.HandlePacket)

	return ns, nil
}

// NetworkChanged drops idle DoH connections: after a wifi<->LTE switch they
// are dead but would only fail after the client timeout.
func NetworkChanged() {
	health.newNetwork()
	mu.Lock()
	d := doh
	mu.Unlock()
	if d != nil {
		d.CloseIdleConnections()
	}
	DnsChanged()
}

// DnsChanged drops the DNS cache: answers of the old network's resolver may
// be local to it. The host calls it alone when only the default network or
// its resolvers changed, which says nothing about the relay's health.
func DnsChanged() {
	dnsCache.Clear()
	flushDirectFails()
}

// SetEndpoint points new relay sessions (apps, DoH, the prober) at another
// VDS; sessions already open stay where they are. The relay's health starts
// afresh: the prober tries the new one at once and the host hears a new
// RelayDown verdict. Callable any time: outside a running tunnel it only
// stores the endpoint, and the next Start replaces it with its own.
// suspect: the host moves on because the last one was down, so this one may
// be too; apps are refused until a probe gets through, instead of each
// waiting out a dial timeout.
func SetEndpoint(vpnAddr, sni, path string, suspect bool) error {
	if path == "" {
		return errors.New("no relay path")
	}
	// Before the health reset: a dial that reads the new generation must
	// also read the new endpoint.
	relayEP.Store(&relayTarget{vpnAddr, sni, path})
	health.newEndpoint(suspect)
	firstHello.Store(false)
	// A fresh client: lookups in flight would return their conns to the old
	// pool, and steady DNS traffic would keep reusing the old relay forever.
	mu.Lock()
	d := doh
	if d != nil && dohCred != nil {
		doh = newDoHClient(dohCred)
	}
	mu.Unlock()
	if d != nil {
		d.CloseIdleConnections()
	}
	return nil
}

// NetworkLost cuts every relay connection: the network they were opened on
// is gone, so they would only fail after minutes of TCP retransmits while
// the app waits. Cutting them makes apps reconnect at once via the new
// network. The tunnel itself stays up. In-flight DoH lookups are among the
// cut connections; net/http retries them (Idempotency-Key).
func NetworkLost() {
	connsMu.Lock()
	conns := make([]net.Conn, 0, len(activeConns))
	for c := range activeConns {
		conns = append(conns, c)
	}
	connsMu.Unlock()
	for _, c := range conns {
		c.Close()
	}
	NetworkChanged()
}

func Stop() {
	health.stopped()
	flushDirectFails()
	mu.Lock()
	if s == nil {
		mu.Unlock()
		return
	}
	ns := s
	s = nil
	if proberStop != nil {
		close(proberStop)
		proberStop = nil
	}
	tf := tunFile
	tunFile = nil
	le := linkEP
	linkEP = nil
	// Snapshot active connections.
	connsMu.Lock()
	conns := make([]net.Conn, 0, len(activeConns))
	for c := range activeConns {
		conns = append(conns, c)
	}
	activeConns = nil
	connsMu.Unlock()
	d := doh
	doh, dohCred = nil, nil
	mu.Unlock()

	if d != nil {
		d.CloseIdleConnections()
	}

	// Force-close every active connection. relay() will unblock immediately.
	for _, c := range conns {
		c.Close()
	}

	// Stack.Close leaves the NIC attached, and its reader asleep in ppoll
	// pins the TUN past close(): Android keeps the interface until a packet
	// wakes it. RemoveNIC detaches the endpoint and waits for the reader,
	// but under the stack's lock; on one CPU the reader delivers packets
	// itself and may wait for that lock (a route for an ICMP reply). So
	// detach first, outside it: RemoveNIC's own Attach(nil) is then a no-op.
	if le != nil {
		le.Attach(nil)
	}
	ns.RemoveNIC(nicID)
	ns.Close()

	if tf != nil {
		tf.Close()
	}
}

func trackConn(c net.Conn) bool {
	connsMu.Lock()
	defer connsMu.Unlock()
	if activeConns == nil {
		return false
	}
	activeConns[c] = struct{}{}
	return true
}

func untrackConn(c net.Conn) {
	connsMu.Lock()
	defer connsMu.Unlock()
	if activeConns == nil {
		return
	}
	delete(activeConns, c)
}

func handleTCP(r *tcp.ForwarderRequest, id stack.TransportEndpointID, cred []byte) {
	defer guard("handleTCP")
	// Only port 53 is served on the fake resolver. Android's opportunistic
	// Private DNS probes DoT (853) on it; answer RST here instead of hauling
	// a doomed session to the relay, and let the system fall back at once.
	if id.LocalAddress.As4() == [4]byte(fakeDNS) && id.LocalPort != 53 {
		r.Complete(true)
		return
	}

	var wq waiter.Queue
	ep, tcpErr := r.CreateEndpoint(&wq)
	if tcpErr != nil {
		r.Complete(true)
		return
	}
	r.Complete(false)

	conn := gonet.NewTCPConn(&wq, ep)
	defer conn.Close()

	addrBytes := id.LocalAddress.As4()
	dstIP := net.IP(addrBytes[:])
	dstPort := id.LocalPort

	if !trackConn(conn) {
		return
	}
	defer untrackConn(conn)

	if dstPort == 53 && dstIP.Equal(fakeDNS) {
		serveDNSTCP(conn)
		return
	}

	// Routing by name: the site from the ClientHello decides. Ours (or no
	// name at all) → relay; anyone else on a shared CDN address → direct.
	// The peeked bytes are forwarded as they were.
	first := peekClientHello(conn)
	name := parseSNI(first)
	direct := name != "" && !relayByName(name)
	if name == "" {
		name = "-"
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	var out net.Conn
	var err error
	if direct {
		out, err = dialDirect(ctx, "tcp", fmt.Sprintf("%s:%d", dstIP, dstPort))
		if err == nil {
			out = &trackedConn{Conn: out}
			if !trackConn(out) {
				out.Close()
				err = errors.New("stopped")
			}
		}
	} else {
		out, err = dialRelay(ctx, cred, dstIP, dstPort)
	}
	cancel()
	if err != nil {
		// During an outage or over the quota, the relay's own lines tell the story.
		if direct || !health.down() && !errors.Is(err, errQuota) {
			log.Printf("sni %s → %s:%d %s: %v", name, dstIP, dstPort, way(direct), err)
		}
		return
	}
	defer out.Close()
	// Relayed sessions are the norm and too many to log; direct ones say
	// which foreign sites share our subnets.
	if direct {
		log.Printf("sni %s → %s:%d direct", name, dstIP, dstPort)
	}

	if len(first) > 0 {
		out.SetWriteDeadline(time.Now().Add(handshakeTimeout))
		if _, err := out.Write(first); err != nil {
			return
		}
		out.SetWriteDeadline(time.Time{})
	}
	relay(conn, out)
}

func way(direct bool) string {
	if direct {
		return "direct"
	}
	return "relay"
}

// dialRelay opens a TLS session to the VDS, does the upgrade handshake and
// sends the 7-byte header. The returned conn is tracked so Stop() can
// force-close it.
func dialRelay(ctx context.Context, cred []byte, dstIP net.IP, dstPort uint16) (net.Conn, error) {
	if overQuota() {
		return nil, errQuota
	}
	if err := health.allow(); err != nil {
		return nil, err
	}
	gen := health.generation()
	ep := relayEP.Load()
	rawConn, err := dialTLS(ctx, gen, ep.addr, ep.sni)
	if err != nil {
		return nil, err
	}
	rawConn.SetDeadline(time.Now().Add(handshakeTimeout))
	br, err := upgrade(rawConn, ep, cred)
	if err != nil {
		rawConn.Close()
		// TLS passing and the upgrade dying is an outage too (DPI after the
		// hello, relay down behind nginx). A refused credential is not.
		if !errors.Is(err, ErrCredRejected) {
			health.failed(gen, "upgrade", fmt.Sprintf("relay: upgrade failed: %v", err), err)
		}
		return nil, err
	}
	health.accepted()
	health.ok(gen)
	rawConn.SetDeadline(time.Time{})
	c := &trackedConn{Conn: &bufConn{rawConn, br}}
	if !trackConn(c) {
		rawConn.Close()
		return nil, fmt.Errorf("stopped")
	}

	var hdr [7]byte
	hdr[0] = 0x01
	copy(hdr[1:5], dstIP.To4())
	binary.BigEndian.PutUint16(hdr[5:7], dstPort)

	c.SetWriteDeadline(time.Now().Add(handshakeTimeout))
	if _, err := c.Write(hdr[:]); err != nil {
		c.Close()
		return nil, err
	}
	c.SetWriteDeadline(time.Time{})
	return c, nil
}

// relayRootCAs is nil in production (system roots); tests inject their own.
var relayRootCAs *x509.CertPool

// TrustCA makes the relay's TLS verify against this PEM CA alone, for a
// test relay on the host: Go on Android reads the system store only, never
// a user-installed CA. Empty restores the system roots.
func TrustCA(pemCA string) error {
	if pemCA == "" {
		relayRootCAs = nil
		return nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM([]byte(pemCA)) {
		return errors.New("trust ca: no certificate in pem")
	}
	relayRootCAs = pool
	return nil
}

// Chrome 120, not HelloChrome_Auto: the current hello carries an ML-KEM key
// share (~1.8 KB, two TCP segments) and on the user's phone most of those
// never got an answer, on Wi-Fi and LTE alike, while the one-segment hello
// passed 60/60. Still a Chrome fingerprint, just an older one.
var chromeHello = utls.HelloChrome_120

// notCounted as dialTLS's gen: health ignores the dial.
const notCounted = ^uint64(0)

var (
	handshakeTimeout = 10 * time.Second
	// Reset by Start; the first handshake after it logs its details once.
	firstHello atomic.Bool
)

// dialTLS connects to the VDS with a Chrome ClientHello (uTLS): Go's own
// hello is a fingerprint of its own. The tunnelled DoH TLS is not affected.
// Chrome offers h2; our HTTP is 1.1 only, so an h2 pick is an error.
// Every failure is logged with its timing: there is no fallback, the log
// is how a broken hello gets noticed. gen is the caller's, read before it
// picked addr: a failure on the endpoint SetEndpoint just replaced must not
// count against the new one.
func dialTLS(ctx context.Context, gen uint64, addr, sni string) (net.Conn, error) {
	t0 := time.Now()
	// Idle above the relay's 2 min idle close: a live session ends before
	// the first probe, a dead path (no onLost) still breaks after ~195 s.
	raw, err := (&net.Dialer{Timeout: handshakeTimeout, KeepAliveConfig: net.KeepAliveConfig{
		Enable: true, Idle: 150 * time.Second, Interval: 15 * time.Second, Count: 3,
	}}).DialContext(ctx, "tcp", addr)
	if err != nil {
		health.failed(gen, "connect", fmt.Sprintf("tls: connect %s failed after %s: %v", addr, time.Since(t0).Round(time.Millisecond), err), err)
		return nil, err
	}
	uc := utls.UClient(raw, &utls.Config{ServerName: sni, RootCAs: relayRootCAs}, chromeHello)
	// The dialer timeout ends at connect; without this a server that never
	// answers the hello hangs us until the kernel gives up (minutes).
	raw.SetDeadline(time.Now().Add(handshakeTimeout))
	if err := uc.HandshakeContext(ctx); err != nil {
		raw.Close()
		health.failed(gen, "hello", fmt.Sprintf("tls: chrome hello to %s failed after %s: %v", sni, time.Since(t0).Round(time.Millisecond), err), err)
		return nil, err
	}
	raw.SetDeadline(time.Time{})
	st := uc.ConnectionState()
	if st.NegotiatedProtocol == "h2" {
		uc.Close()
		log.Printf("tls: %s picked h2, cannot speak it", sni)
		return nil, errors.New("server picked h2")
	}
	if firstHello.CompareAndSwap(false, true) {
		log.Printf("tls: chrome hello to %s ok in %s (tls 0x%04x, alpn %q)", sni, time.Since(t0).Round(time.Millisecond), st.Version, st.NegotiatedProtocol)
	}
	return uc, nil
}

type trackedConn struct {
	net.Conn
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { untrackConn(c) })
	return c.Conn.Close()
}

func (c *trackedConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return nil
}

func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	cp := func(dst, src net.Conn) {
		defer wg.Done()
		defer guard("relay")
		io.Copy(dst, src)
		if hc, ok := dst.(interface{ CloseWrite() error }); ok {
			hc.CloseWrite()
		}
	}

	go cp(a, b)
	go cp(b, a)
	wg.Wait()
}
