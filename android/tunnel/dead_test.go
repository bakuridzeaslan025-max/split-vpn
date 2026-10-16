package tunnel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
)

type lockedLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *lockedLog) Log(s string) { l.mu.Lock(); l.lines = append(l.lines, s); l.mu.Unlock() }
func (l *lockedLog) has(sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// multiRelay: every upgrade gets a 101; sessions to a silent dst swallow
// what they get, the rest echo. sessions counts 7-byte headers per dst.
type multiRelay struct {
	ln       net.Listener
	mu       sync.Mutex
	silent   map[string]bool
	sessions map[string]int
	upgrades atomic.Int32
}

func newMultiRelay(t *testing.T, silent ...string) *multiRelay {
	t.Helper()
	r := &multiRelay{ln: fakeTLSListener(t), silent: map[string]bool{}, sessions: map[string]int{}}
	for _, ip := range silent {
		r.silent[ip] = true
	}
	go func() {
		for {
			c, err := r.ln.Accept()
			if err != nil {
				return
			}
			go r.serve(c)
		}
	}()
	return r
}

func (r *multiRelay) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	if _, err := http.ReadRequest(br); err != nil {
		return
	}
	io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	r.upgrades.Add(1)
	hdr := make([]byte, 7)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return // a probe
	}
	ip := net.IP(hdr[1:5]).String()
	r.mu.Lock()
	r.sessions[ip]++
	silent := r.silent[ip]
	r.mu.Unlock()
	if silent {
		io.Copy(io.Discard, br)
		return
	}
	io.Copy(c, br)
}

func (r *multiRelay) count(ip string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sessions[ip]
}

// deadStack: direct on, no strategies (no direct tries: sessions go
// straight to the relay), googlevideo relayed, a named network.
func deadStack(t *testing.T, silent ...string) (*pickState, *channel.Endpoint, *multiRelay, *lockedLog, string) {
	t.Helper()
	lg := &lockedLog{}
	setLogger(lg)
	t.Cleanup(func() { setLogger(&sliceLogger{}) })
	r := newMultiRelay(t, silent...)
	withConns(t)
	setDomains("googlevideo.com\nyoutube.com")
	t.Cleanup(func() { setDomains("") })
	old := deadWait
	deadWait = 300 * time.Millisecond
	t.Cleanup(func() { deadWait = old })
	p := freshPicker(t)
	strategies.Store(nil)
	directOn.Store(true)
	t.Cleanup(func() { directOn.Store(false); quotaExceeded.Store(false) })
	file := filepath.Join(t.TempDir(), "desync.json")
	p.started(file)
	p.setNetKey("cell:25001")
	_, ep := testStack(t, r.ln.Addr().String())
	return p, ep, r, lg, file
}

func watch(p *pickState, ip string) {
	p.mu.Lock()
	p.noHello.put(ip, time.Time{})
	p.mu.Unlock()
}

func addr4(s string) tcpip.Address { return tcpip.AddrFrom4([4]byte(net.ParseIP(s).To4())) }

// open a session to ip:443 with the hello for name; returns the peer.
func openHello(t *testing.T, ep *channel.Endpoint, ip, name string, sport uint16) *tcpPeer {
	t.Helper()
	p := &tcpPeer{ep: ep, dst: addr4(ip), dport: 443, sport: sport, seq: 1000}
	p.send(header.TCPFlagSyn, nil)
	p.seq++
	sa := recvFor(t, ep, sport)
	if sa.Flags() != header.TCPFlagSyn|header.TCPFlagAck {
		t.Fatalf("flags %v", sa.Flags())
	}
	p.ack = sa.SequenceNumber() + 1
	p.send(header.TCPFlagAck, nil)
	p.send(header.TCPFlagAck|header.TCPFlagPsh, clientHello(t, name))
	return p
}

// recvFor skips segments of other sessions.
func recvFor(t *testing.T, ep *channel.Endpoint, sport uint16) header.TCP {
	t.Helper()
	for {
		_, tr := readProto(t, ep, 6)
		if tc := header.TCP(tr); tc.DestinationPort() == sport {
			return tc
		}
	}
}

// closedWithin: the app side gets a FIN or RST from the stack.
func closedWithin(t *testing.T, ep *channel.Endpoint, d time.Duration) map[uint16]bool {
	t.Helper()
	got := map[uint16]bool{}
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		ip, tr := readOut(ep, time.Until(deadline))
		if ip == nil {
			break
		}
		tc := header.TCP(tr)
		if tc.Flags()&(header.TCPFlagFin|header.TCPFlagRst) != 0 {
			got[tc.DestinationPort()] = true
		}
	}
	return got
}

func synAnswer(t *testing.T, ep *channel.Endpoint, ip string, port, sport uint16) header.TCPFlags {
	t.Helper()
	p := &tcpPeer{ep: ep, dst: addr4(ip), dport: port, sport: sport, seq: 1000}
	p.send(header.TCPFlagSyn, nil)
	return recvFor(t, ep, sport).Flags()
}

const deadIP, liveIP = "203.0.113.7", "203.0.113.8"
const videoName = "rr3---sn-gvnuxaxjvh-axqz.googlevideo.com"

// A dead address: SYN to :443 gets a RST without a relay dial; other
// ports, and everything with direct off, are served.
func TestHandleTCP_DeadRefusedOn443Only(t *testing.T) {
	p, ep, r, _, _ := deadStack(t)
	p.markDead(p.gen, deadIP)
	if f := synAnswer(t, ep, deadIP, 443, 50001); f&header.TCPFlagRst == 0 {
		t.Fatalf(":443 flags %v", f)
	}
	time.Sleep(300 * time.Millisecond)
	if n := r.upgrades.Load(); n != 0 {
		t.Fatalf("%d relay dials for a refused SYN (fresh mark = no recheck)", n)
	}
	if f := synAnswer(t, ep, deadIP, 80, 50002); f != header.TCPFlagSyn|header.TCPFlagAck {
		t.Fatalf(":80 flags %v", f)
	}
	directOn.Store(false)
	if f := synAnswer(t, ep, deadIP, 443, 50003); f != header.TCPFlagSyn|header.TCPFlagAck {
		t.Fatalf("direct off: flags %v", f)
	}
}

// A silent node's relayed sessions (a burst of two) say nothing for
// deadWait, the VDS answers a probe: the node is dead, kept in the map,
// both sessions closed, next SYN refused.
func TestCheckDead_MarksAndClosesBurst(t *testing.T) {
	p, ep, r, lg, file := deadStack(t, deadIP)
	watch(p, deadIP)
	openHello(t, ep, deadIP, videoName, 51001)
	openHello(t, ep, deadIP, videoName, 51002)
	closed := closedWithin(t, ep, deadWait+time.Second)
	if !p.deadNow(deadIP) {
		t.Fatalf("not dead; log %q", lg.lines)
	}
	if !closed[51001] || !closed[51002] {
		t.Fatalf("closed %v", closed)
	}
	if readMap(t, file)["cell:25001"].Dead[deadIP] == 0 {
		t.Fatal("not in the map")
	}
	if r.count(deadIP) != 2 {
		t.Fatalf("relay sessions %d", r.count(deadIP))
	}
	if f := synAnswer(t, ep, deadIP, 443, 51003); f&header.TCPFlagRst == 0 {
		t.Fatalf("after the mark: %v", f)
	}
}

// Names that must never be refused, and a node that is not watched.
func TestCheckDead_OnlyWatchedCacheNodes(t *testing.T) {
	p, ep, _, _, _ := deadStack(t, deadIP, liveIP)
	watch(p, deadIP)
	openHello(t, ep, deadIP, "redirector.googlevideo.com", 52001)
	openHello(t, ep, deadIP, "www.youtube.com", 52002)
	openHello(t, ep, liveIP, videoName, 52003) // silent but never failed direct
	time.Sleep(deadWait + time.Second)
	if p.deadNow(deadIP) || p.deadNow(liveIP) {
		t.Fatal("marked")
	}
}

// The relay itself unreachable for the probe and no other session
// answered: the address is not blamed.
func TestCheckDead_ProbeFailsNoMark(t *testing.T) {
	p, ep, r, _, _ := deadStack(t, deadIP)
	watch(p, deadIP)
	openHello(t, ep, deadIP, videoName, 53001)
	waitSession(t, r, deadIP)
	r.ln.Close()
	time.Sleep(deadWait + time.Second)
	if p.deadNow(deadIP) {
		t.Fatal("dead while the VDS is unreachable")
	}
}

// Network or endpoint switched within the check, or over the quota: no mark.
func TestCheckDead_SwitchOrQuotaNoMark(t *testing.T) {
	for _, c := range []struct {
		name string
		do   func()
	}{
		{"new network", health.newNetwork},
		{"new endpoint", func() { health.newEndpoint(false) }},
		{"quota", func() { quotaExceeded.Store(true) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, ep, r, _, _ := deadStack(t, deadIP)
			watch(p, deadIP)
			openHello(t, ep, deadIP, videoName, 54001)
			waitSession(t, r, deadIP)
			c.do()
			time.Sleep(deadWait + time.Second)
			if p.deadNow(deadIP) {
				t.Fatal("marked")
			}
		})
	}
}

func writeDeadMap(t *testing.T, file string, until int64) {
	t.Helper()
	b, _ := json.Marshal(netMapFile{V: 1, Nets: map[string]netEntry{
		"cell:25001": {Win: "rec2", At: 1, Dead: map[string]int64{deadIP: until}},
	}})
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// From the map: the first refused SYN probes the node through the relay;
// an answer lifts the mark (memory and map), the next SYN is served.
func TestRecheckDead_AliveLifts(t *testing.T) {
	_, ep, r, _, _ := deadStack(t) // echoes: the node "answers"
	q := freshPicker(t)
	file := filepath.Join(t.TempDir(), "desync.json")
	writeDeadMap(t, file, q.now().Add(time.Hour).Unix())
	q.started(file)
	q.setNetKey("cell:25001")
	if f := synAnswer(t, ep, deadIP, 443, 55001); f&header.TCPFlagRst == 0 {
		t.Fatalf("from the map: %v", f)
	}
	deadline := time.Now().Add(deadWait + time.Second)
	for q.deadNow(deadIP) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if q.deadNow(deadIP) || r.count(deadIP) != 1 {
		t.Fatalf("still dead, %d probes", r.count(deadIP))
	}
	if _, in := readMap(t, file)["cell:25001"].Dead[deadIP]; in {
		t.Fatal("still in the map")
	}
	if f := synAnswer(t, ep, deadIP, 443, 55002); f != header.TCPFlagSyn|header.TCPFlagAck {
		t.Fatalf("after the lift: %v", f)
	}
}

// Silence with nobody else answered: the mark is left as it was.
func TestRecheckDead_SilenceAloneDoesNotExtend(t *testing.T) {
	_, ep, r, _, _ := deadStack(t, deadIP)
	q := freshPicker(t)
	file := filepath.Join(t.TempDir(), "desync.json")
	until := q.now().Add(time.Hour).Unix()
	writeDeadMap(t, file, until)
	q.started(file)
	q.setNetKey("cell:25001")
	synAnswer(t, ep, deadIP, 443, 56001)
	time.Sleep(deadWait + time.Second)
	if r.count(deadIP) != 1 {
		t.Fatalf("%d probes", r.count(deadIP))
	}
	if got := readMap(t, file)["cell:25001"].Dead[deadIP]; got != until || !q.deadNow(deadIP) {
		t.Fatalf("until %d, want %d", got, until)
	}
	// A second refused SYN within deadRecheck: no second probe.
	synAnswer(t, ep, deadIP, 443, 56002)
	time.Sleep(300 * time.Millisecond)
	if r.count(deadIP) != 1 {
		t.Fatalf("%d probes", r.count(deadIP))
	}
}

// A desync.json from before the branch (no "dead"): read, the winner kept,
// a mark added next to it.
func TestMarkDead_OldMapFile(t *testing.T) {
	captureLog(t)
	p := freshPicker(t)
	SetStrategies(planStrategies)
	directOn.Store(true)
	t.Cleanup(func() { strategies.Store(nil); directOn.Store(false) })
	file := filepath.Join(t.TempDir(), "desync.json")
	os.WriteFile(file, []byte(fmt.Sprintf(`{"v":1,"nets":{"cell:25001":{"win":"rec-mid-dis","at":%d}}}`, p.now().Unix())), 0o600)
	p.started(file)
	p.setNetKey("cell:25001")
	if p.curID() != "rec-mid-dis" {
		t.Fatalf("cur %s", p.curID())
	}
	p.markDead(p.gen, "9.9.9.9")
	e := readMap(t, file)["cell:25001"]
	if e.Win != "rec-mid-dis" || e.Dead["9.9.9.9"] == 0 {
		t.Fatalf("%+v", e)
	}
}

// deadMax per network in the map, the earliest-ending dropped.
func TestMarkDead_MapCapped(t *testing.T) {
	p, _, file := picking(t)
	for i := range deadMax + 1 {
		p.markDead(p.gen, fmt.Sprintf("10.0.0.%d", i))
		advance(p, time.Second)
	}
	d := readMap(t, file)["cell:25001"].Dead
	if len(d) != deadMax || d["10.0.0.0"] != 0 {
		t.Fatalf("%d in the map, first %d", len(d), d["10.0.0.0"])
	}
}

// A recheck begun on an earlier visit to this network reports after a
// switch away and back: ignored.
func TestRecheck_StaleVisit(t *testing.T) {
	p, _, _ := picking(t)
	p.markDead(p.gen, "1.1.1.1")
	advance(p, deadRecheck)
	gen, due := p.recheckDue("1.1.1.1")
	if !due {
		t.Fatal("not due")
	}
	p.setNetKey("cell:25002")
	p.setNetKey("cell:25001")
	p.recheckDone(gen, "1.1.1.1", true)
	if !p.deadNow("1.1.1.1") {
		t.Fatal("lifted by a stale recheck")
	}
	if _, due := p.recheckDue("1.1.1.1"); !due {
		t.Fatal("not due on a new visit")
	}
}

// Another session's answer after this one's hello proves the VDS: marked
// without the probe (the relay is gone for it).
func TestCheckDead_OtherSessionSuffices(t *testing.T) {
	p, ep, r, _, _ := deadStack(t, deadIP)
	watch(p, deadIP)
	openHello(t, ep, deadIP, videoName, 57001)
	waitSession(t, r, deadIP)
	time.Sleep(100 * time.Millisecond)
	openHello(t, ep, liveIP, "www.youtube.com", 57002)
	waitSession(t, r, liveIP)
	time.Sleep(200 * time.Millisecond)
	r.ln.Close()
	time.Sleep(deadWait + time.Second)
	if !p.deadNow(deadIP) {
		t.Fatal("not marked")
	}
}

// A direct try in flight when its address turns dead: the app side is
// closed after the FAIL, no relay session.
func TestHandleTCP_InFlightTryTurnedDead(t *testing.T) {
	lg := &lockedLog{}
	setLogger(lg)
	t.Cleanup(func() { setLogger(&sliceLogger{}) })
	r := newMultiRelay(t)
	withConns(t)
	setDomains("googlevideo.com")
	t.Cleanup(func() { setDomains("") })
	defer func(w time.Duration) { desyncWait = w }(desyncWait)
	desyncWait = 300 * time.Millisecond
	fp := &fakeProtector{ok: true}
	protector = fp
	t.Cleanup(func() { protector = nil })
	p := directWith(t, `[{"id":"none","spec":""}]`)
	_, ep := testStack(t, r.ln.Addr().String())

	host := hostIPv4(t)
	ln, err := net.Listen("tcp", host.String()+":0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(io.Discard, c) }()
		}
	}()
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	pe := &tcpPeer{ep: ep, dst: addr4(host.String()), dport: port, sport: 58001, seq: 1000}
	pe.send(header.TCPFlagSyn, nil)
	pe.seq++
	sa := recvFor(t, ep, 58001)
	pe.ack = sa.SequenceNumber() + 1
	pe.send(header.TCPFlagAck, nil)
	pe.send(header.TCPFlagAck|header.TCPFlagPsh, clientHello(t, videoName))
	time.Sleep(50 * time.Millisecond)
	p.markDead(p.gen, host.String())
	closed := closedWithin(t, ep, 2*time.Second)
	if !closed[58001] {
		t.Fatalf("not closed; log %q", lg.lines)
	}
	if r.count(host.String()) != 0 {
		t.Fatal("relayed")
	}
	if p.fails != 0 {
		t.Fatalf("fails %d", p.fails)
	}
}

func waitSession(t *testing.T, r *multiRelay, ip string) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); r.count(ip) == 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("no relay session to %s", ip)
		}
	}
}

// Marks, rechecks, results and network switches at once: -race and no panic.
func TestPicker_Concurrent(t *testing.T) {
	p, _, _ := picking(t)
	p.now = time.Now
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 200 {
				ip := fmt.Sprintf("10.1.%d.%d", g, i%20)
				switch i % 5 {
				case 0:
					_, gen := p.pick(ip)
					p.markDead(gen, ip)
				case 1:
					if gen, ok := p.recheckDue(ip); ok {
						p.recheckDone(gen, ip, i%2 == 0)
					}
				case 2:
					if st, gen := p.pick(ip); st != nil {
						p.result(gen, st.id, ip, time.Now(), false, "x")
					}
				case 3:
					p.deadNow(ip)
					p.silentNow(ip)
				case 4:
					if g == 0 && i%50 == 4 {
						p.setNetKey(fmt.Sprintf("cell:2500%d", i%3))
					}
				}
			}
		}()
	}
	wg.Wait()
}

// Another session to the same node got its first byte after this one's
// hello: the node is alive, the silence is the path's.
func TestConnsByAddr_Answered(t *testing.T) {
	var c connsByAddr
	c.m = map[string]map[net.Conn]net.Conn{}
	since := time.Now()
	early := &watchConn{start: since.Add(-time.Second)}
	early.first.Store(int64(500 * time.Millisecond)) // before since
	defer c.add(deadIP, &net.TCPConn{}, early)()
	if c.answered(deadIP, since) {
		t.Fatal("an answer from before the hello counted")
	}
	late := &watchConn{start: since}
	late.first.Store(int64(time.Second))
	defer c.add(deadIP, &net.UDPConn{}, late)()
	if !c.answered(deadIP, since) || c.answered(liveIP, since) {
		t.Fatal("answered")
	}
}
