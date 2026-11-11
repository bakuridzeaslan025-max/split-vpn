package tunnel

import (
	"io"
	"net"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
)

var (
	serverHello = []byte{0x16, 3, 3, 0, 2, 2, 0}
	alert       = []byte{0x15, 3, 3, 0, 2, 2, 40}
)

// directServer stands for a YouTube node: it reads the hello, then after
// `after` sends answer; nil answer is silence. closed hears of every
// socket the client closed on it.
func directServer(t *testing.T, helloLen int, after time.Duration, answer []byte) (ip net.IP, port uint16, closed <-chan struct{}) {
	t.Helper()
	ip = hostIPv4(t)
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan struct{}, 8)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				if _, err := io.ReadFull(c, make([]byte, helloLen)); err != nil {
					return
				}
				if answer != nil {
					time.Sleep(after)
					c.Write(answer)
				}
				_, err := io.Copy(io.Discard, c)
				if ne, ok := err.(net.Error); !ok || !ne.Timeout() {
					ch <- struct{}{}
				}
			}()
		}
	}()
	return ip, uint16(ln.Addr().(*net.TCPAddr).Port), ch
}

// raceStack: handleTCP over a multiRelay, direct on with the strategy
// none, the leg after 100 ms, the direct try's wait as given.
func raceStack(t *testing.T, wait time.Duration) (*pickState, *channel.Endpoint, *multiRelay, *fakeProtector) {
	t.Helper()
	lg := &lockedLog{}
	setLogger(lg)
	t.Cleanup(func() { setLogger(&sliceLogger{}) })
	r := newMultiRelay(t)
	withConns(t)
	setDomains("googlevideo.com\nyoutube.com")
	t.Cleanup(func() { setDomains("") })
	oldWait, oldRace, oldDead := desyncWait, raceAfter, deadWait
	t.Cleanup(func() { desyncWait, raceAfter, deadWait = oldWait, oldRace, oldDead })
	desyncWait, raceAfter = wait, 100*time.Millisecond
	fp := &fakeProtector{ok: true}
	protector = fp
	t.Cleanup(func() { protector = nil })
	p := directWith(t, `[{"id":"none","spec":""}]`)
	_, ep := testStack(t, r.ln.Addr().String())
	t.Cleanup(func() { endRace(t) })
	return p, ep, r, fp
}

// endRace closes what is still open and waits for the sessions to end
// and every goroutine of the races: they report to the picker.
func endRace(t *testing.T) {
	t.Helper()
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
	for {
		connsMu.Lock()
		n := len(activeConns)
		connsMu.Unlock()
		if n == 0 && racing.Load() == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Errorf("%d sessions, %d race goroutines left", n, racing.Load())
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// stopConns is Stop's part about connections: all closed, none tracked.
func stopConns() {
	connsMu.Lock()
	conns := make([]net.Conn, 0, len(activeConns))
	for c := range activeConns {
		conns = append(conns, c)
	}
	activeConns = nil
	connsMu.Unlock()
	for _, c := range conns {
		c.Close()
	}
}

func waitRaces(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); racing.Load() > 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d race goroutines left", racing.Load())
		}
	}
}

func waitClosed(t *testing.T, closed <-chan struct{}) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the direct socket was not closed")
	}
}

// appHello opens a session from the app to ip:port and sends hello.
func appHello(t *testing.T, ep *channel.Endpoint, ip net.IP, port uint16, hello []byte) *tcpPeer {
	t.Helper()
	c := handshakeFrom(t, ep, tcpip.AddrFrom4([4]byte(ip.To4())), port, 50001)
	for b := hello; len(b) > 0; {
		n := min(len(b), 1000)
		c.send(header.TCPFlagAck|header.TCPFlagPsh, b[:n])
		b = b[n:]
	}
	return c
}

func healthFails() int {
	health.mu.Lock()
	defer health.mu.Unlock()
	return health.fails
}

// The node is silent: the leg takes the session long before desyncWait,
// one VDS session; the address turns silent only with the verdict.
func TestRace_DirectSilentRelayServes(t *testing.T) {
	p, ep, r, _ := raceStack(t, 1500*time.Millisecond)
	hello := clientHello(t, "www.youtube.com")
	ip, port, _ := directServer(t, len(hello), 0, nil)
	t0 := time.Now()
	if got := appHello(t, ep, ip, port, hello).receive(t, len(hello)); string(got) != string(hello) {
		t.Fatalf("app got %d bytes, want the relay's echo", len(got))
	}
	if d := time.Since(t0); d >= desyncWait {
		t.Fatalf("relay answered after %s", d)
	}
	if _, silent := p.silentNow(ip.String()); silent {
		t.Fatal("silent before the verdict")
	}
	if r.count(ip.String()) != 1 || r.upgrades.Load() != 1 {
		t.Fatalf("%d sessions, %d upgrades", r.count(ip.String()), r.upgrades.Load())
	}
	time.Sleep(desyncWait - time.Since(t0) + 300*time.Millisecond)
	if _, silent := p.silentNow(ip.String()); !silent {
		t.Fatal("not silent after the verdict")
	}
	if got := p.tallyNow(); got.raced != 1 || got.relayWon != 1 || got.late != 0 || got.failed != 1 {
		t.Fatalf("tally %+v", got)
	}
}

// The relay took a cache node's session before the verdict: the dead check
// still runs, and asks about silence only when due.
func TestRace_DeadCheckOnRelayedSession(t *testing.T) {
	p, ep, r, _ := raceStack(t, 500*time.Millisecond)
	deadWait = time.Second
	hello := clientHello(t, videoName)
	ip, port, _ := directServer(t, len(hello), 0, nil)
	r.mu.Lock()
	r.silent[ip.String()] = true
	r.mu.Unlock()
	appHello(t, ep, ip, port, hello)
	if closed := closedWithin(t, ep, 3*time.Second); !closed[50001] {
		t.Fatal("not closed")
	}
	if !p.deadNow(ip.String()) {
		t.Fatal("not dead")
	}
	if r.count(ip.String()) != 1 {
		t.Fatalf("%d sessions", r.count(ip.String()))
	}
}

// The node answers past raceAfter while the leg is still in its TLS
// handshake: direct wins, the leg is cancelled and leaves no trace in the
// relay's health, the dial events or the quota.
func TestRace_DirectWinsLegCancelled(t *testing.T) {
	p, ep, r, fp := raceStack(t, 1500*time.Millisecond)
	r.delay.Store(int64(1500 * time.Millisecond))
	addr := r.ln.Addr().String()
	relayEP.Store(&relayTarget{addr, "relay.test", "/app/x", "nl1"})
	SetDialEvents(10000)
	t.Cleanup(func() { SetDialEvents(0) })
	SetQuota(0, 0)
	t.Cleanup(func() { SetQuota(0, 0) })
	hello := clientHello(t, "www.youtube.com")
	ip, port, _ := directServer(t, len(hello), 300*time.Millisecond, serverHello)
	if got := appHello(t, ep, ip, port, hello).receive(t, len(serverHello)); string(got) != string(serverHello) {
		t.Fatalf("app got % x", got)
	}
	waitRaces(t)
	time.Sleep(100 * time.Millisecond) // DialEvent goes in a goroutine
	if n := healthFails(); n != 0 {
		t.Fatalf("health: %d fails", n)
	}
	fp.mu.Lock()
	dials := fp.dials
	fp.mu.Unlock()
	if len(dials) != 0 {
		t.Fatalf("dial events %v", dials)
	}
	if u := Usage(); u > 7 {
		t.Fatalf("quota %d", u)
	}
	if r.upgrades.Load() != 0 {
		t.Fatalf("%d upgrades", r.upgrades.Load())
	}
	if got := p.tallyNow(); got.raced != 1 || got.relayWon != 0 || got.ok != 1 {
		t.Fatalf("tally %+v", got)
	}
}

// The leg was ready first, the node answered later, within desyncWait:
// the address is fine, the socket is closed, the tally says late.
func TestRace_LateDirectAnswerCounts(t *testing.T) {
	p, ep, _, _ := raceStack(t, 1500*time.Millisecond)
	hello := clientHello(t, "www.youtube.com")
	ip, port, closed := directServer(t, len(hello), 500*time.Millisecond, serverHello)
	if got := appHello(t, ep, ip, port, hello).receive(t, len(hello)); string(got) != string(hello) {
		t.Fatalf("app got %d bytes, want the relay's echo", len(got))
	}
	waitClosed(t, closed)
	if st, _, _ := p.pick(ip.String(), "www.youtube.com"); st == nil {
		t.Fatal("relay after an answer")
	}
	if got := p.tallyNow(); got.raced != 1 || got.relayWon != 1 || got.late != 1 || got.ok != 1 {
		t.Fatalf("tally %+v", got)
	}
}

// A late alert is a failure all the same.
func TestRace_LateAlertIsFailure(t *testing.T) {
	p, ep, _, _ := raceStack(t, 1500*time.Millisecond)
	hello := clientHello(t, "www.youtube.com")
	ip, port, closed := directServer(t, len(hello), 500*time.Millisecond, alert)
	appHello(t, ep, ip, port, hello).receive(t, len(hello))
	waitClosed(t, closed)
	waitRaces(t)
	if _, silent := p.silentNow(ip.String()); !silent {
		t.Fatal("not silent after an alert")
	}
	if got := p.tallyNow(); got.late != 0 || got.failed != 1 {
		t.Fatalf("tally %+v", got)
	}
}

// The leg fails and so does the node: the leg's error ends the session,
// no second dial, one failure in the relay's health.
func TestRace_LegFailsNoSecondDial(t *testing.T) {
	p, ep, r, _ := raceStack(t, 600*time.Millisecond)
	r.ln.Close()
	hello := clientHello(t, "www.youtube.com")
	ip, port, _ := directServer(t, len(hello), 0, nil)
	appHello(t, ep, ip, port, hello)
	if closed := closedWithin(t, ep, 3*time.Second); !closed[50001] {
		t.Fatal("not closed")
	}
	if n := healthFails(); n != 1 {
		t.Fatalf("health: %d fails", n)
	}
	if got := p.tallyNow(); got.raced != 1 || got.relayWon != 0 {
		t.Fatalf("tally %+v", got)
	}
}

// The node fails while the leg is in its handshake: the leg serves, once.
func TestRace_DirectFailsLegInHandshake(t *testing.T) {
	p, ep, r, _ := raceStack(t, 1500*time.Millisecond)
	r.delay.Store(int64(600 * time.Millisecond))
	hello := clientHello(t, "www.youtube.com")
	ip, port, _ := directServer(t, len(hello), 250*time.Millisecond, alert)
	if got := appHello(t, ep, ip, port, hello).receive(t, len(hello)); string(got) != string(hello) {
		t.Fatalf("app got %d bytes, want the relay's echo", len(got))
	}
	if r.count(ip.String()) != 1 || r.upgrades.Load() != 1 {
		t.Fatalf("%d sessions, %d upgrades", r.count(ip.String()), r.upgrades.Load())
	}
	if got := p.tallyNow(); got.raced != 1 || got.relayWon != 1 || got.failed != 1 {
		t.Fatalf("tally %+v", got)
	}
}

// A fast failure before raceAfter: no leg, the relay dialed as always.
func TestRace_DirectFailsBeforeRace(t *testing.T) {
	p, ep, r, _ := raceStack(t, 1500*time.Millisecond)
	raceAfter = 500 * time.Millisecond
	hello := clientHello(t, "www.youtube.com")
	ip, port, _ := directServer(t, len(hello), 0, alert)
	if got := appHello(t, ep, ip, port, hello).receive(t, len(hello)); string(got) != string(hello) {
		t.Fatalf("app got %d bytes, want the relay's echo", len(got))
	}
	if r.count(ip.String()) != 1 {
		t.Fatalf("%d sessions", r.count(ip.String()))
	}
	if got := p.tallyNow(); got.raced != 0 || got.failed != 1 {
		t.Fatalf("tally %+v", got)
	}
}

// Another network before the late verdict: it does not count.
func TestRace_NetworkSwitchDropsLateVerdict(t *testing.T) {
	p, ep, _, _ := raceStack(t, 1500*time.Millisecond)
	hello := clientHello(t, "www.youtube.com")
	ip, port, closed := directServer(t, len(hello), 500*time.Millisecond, serverHello)
	appHello(t, ep, ip, port, hello).receive(t, len(hello))
	p.setNetKey("cell:25098")
	waitClosed(t, closed)
	waitRaces(t)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.tally != (tally{}) || p.attempts != 0 || p.okCountLocked() != 0 {
		t.Fatalf("counted on the new network: %+v, %d attempts", p.tally, p.attempts)
	}
}

// Stop while the leg is in its handshake: both sides end, nothing counted.
func TestRace_StopDuringRace(t *testing.T) {
	p, ep, r, _ := raceStack(t, 2*time.Second)
	r.delay.Store(int64(1500 * time.Millisecond))
	hello := clientHello(t, "www.youtube.com")
	ip, port, closed := directServer(t, len(hello), 0, nil)
	appHello(t, ep, ip, port, hello)
	time.Sleep(300 * time.Millisecond)
	stopConns()
	if closed := closedWithin(t, ep, time.Second); !closed[50001] {
		t.Fatal("app side not closed")
	}
	waitClosed(t, closed)
	waitRaces(t)
	if n := healthFails(); n != 0 {
		t.Fatalf("health: %d fails", n)
	}
	if r.upgrades.Load() != 0 {
		t.Fatalf("%d upgrades", r.upgrades.Load())
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attempts != 0 || p.tally.failed != 0 || p.tally.relayWon != 0 {
		t.Fatalf("counted: %+v, %d attempts", p.tally, p.attempts)
	}
}

// yt_race_ms 0: no leg at all; the relay after the full wait, once.
func TestRace_OffWaitsAsBefore(t *testing.T) {
	p, ep, r, _ := raceStack(t, 600*time.Millisecond)
	SetRaceMs(0)
	t.Cleanup(func() { SetRaceMs(-1) })
	hello := clientHello(t, "www.youtube.com")
	ip, port, _ := directServer(t, len(hello), 0, nil)
	t0 := time.Now()
	appHello(t, ep, ip, port, hello).receive(t, len(hello))
	if d := time.Since(t0); d < desyncWait {
		t.Fatalf("relay after %s", d)
	}
	if r.count(ip.String()) != 1 || r.upgrades.Load() != 1 {
		t.Fatalf("%d sessions, %d upgrades", r.count(ip.String()), r.upgrades.Load())
	}
	if got := p.tallyNow(); got.raced != 0 {
		t.Fatalf("tally %+v", got)
	}
}

// The breaker refuses: no leg, the dial after the failure as before, one
// refusal.
func TestRace_NoLegWhileRefusing(t *testing.T) {
	p, ep, r, _ := raceStack(t, 600*time.Millisecond)
	health.mu.Lock()
	health.fails, health.open, health.nextProbe = breakerAfter, true, time.Now().Add(time.Hour)
	health.mu.Unlock()
	hello := clientHello(t, "www.youtube.com")
	ip, port, _ := directServer(t, len(hello), 0, nil)
	appHello(t, ep, ip, port, hello)
	if closed := closedWithin(t, ep, 2*time.Second); !closed[50001] {
		t.Fatal("not closed")
	}
	health.mu.Lock()
	refused := health.refused
	health.mu.Unlock()
	if refused != 1 || r.upgrades.Load() != 0 {
		t.Fatalf("%d refused, %d upgrades", refused, r.upgrades.Load())
	}
	if got := p.tallyNow(); got.raced != 0 {
		t.Fatalf("tally %+v", got)
	}
}

// A silent cluster's working node: the leg starts at once, the node
// answers, and its ok lifts the mark whichever side served.
func TestRace_SilentClusterWorkingNode(t *testing.T) {
	p, ep, _, _ := raceStack(t, 1500*time.Millisecond)
	p.mu.Lock()
	p.clusterSilent.put("sn-oj5hn5", time.Now().Add(silentFor))
	p.mu.Unlock()
	hello := clientHello(t, node3)
	// Late enough for the leg to start for sure.
	ip, port, _ := directServer(t, len(hello), 200*time.Millisecond, serverHello)
	appHello(t, ep, ip, port, hello).receive(t, len(serverHello))
	waitRaces(t)
	p.mu.Lock()
	marked, ok := p.clusterSilentLocked("sn-oj5hn5", time.Now()), p.okClusters.has("sn-oj5hn5")
	p.mu.Unlock()
	if marked || !ok {
		t.Fatalf("still marked %v, ok %v", marked, ok)
	}
	if got := p.tallyNow(); got.raced != 1 || got.ok != 1 {
		t.Fatalf("tally %+v", got)
	}
}

// The leg won by the VDS's 101, the node answered direct later, and the
// relay does not reach it: the session ends at deadWait, the node is
// direct only, not dead.
func TestRace_LateDirectRelaySilentIsDirectOnly(t *testing.T) {
	p, ep, r, _ := raceStack(t, 1500*time.Millisecond)
	deadWait = time.Second
	hello := clientHello(t, videoName)
	ip, port, _ := directServer(t, len(hello), 500*time.Millisecond, serverHello)
	r.mu.Lock()
	r.silent[ip.String()] = true
	r.mu.Unlock()
	appHello(t, ep, ip, port, hello)
	if closed := closedWithin(t, ep, 3*time.Second); !closed[50001] {
		t.Fatal("not closed")
	}
	if p.deadNow(ip.String()) || !p.mutedNow(ip.String()) {
		t.Fatalf("dead %v, muted %v", p.deadNow(ip.String()), p.mutedNow(ip.String()))
	}
	if got := p.tallyNow(); got.relayWon != 1 || got.late != 1 {
		t.Fatalf("tally %+v", got)
	}
}

// Direct only, and the direct try gets no ServerHello: no leg, no relay
// after it, the node dead.
func TestRace_DirectOnlyFailsNoRelay(t *testing.T) {
	p, ep, r, _ := raceStack(t, 500*time.Millisecond)
	hello := clientHello(t, videoName)
	ip, port, _ := directServer(t, len(hello), 0, nil)
	p.mu.Lock()
	a := p.putLocked(ip.String())
	a.hello, a.muteUntil = true, time.Now().Add(silentFor)
	p.mu.Unlock()
	appHello(t, ep, ip, port, hello)
	if closed := closedWithin(t, ep, 2*time.Second); !closed[50001] {
		t.Fatal("not closed")
	}
	if !p.deadNow(ip.String()) || r.upgrades.Load() != 0 {
		t.Fatalf("dead %v, %d upgrades", p.deadNow(ip.String()), r.upgrades.Load())
	}
	if got := p.tallyNow(); got.raced != 0 || got.failed != 1 {
		t.Fatalf("tally %+v", got)
	}
}
