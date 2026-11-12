package tunnel

import (
	"bufio"
	"context"
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
	closes   map[string]bool // sessions to these end at once, nothing back
	sessions map[string]int
	upgrades atomic.Int32
	delay    atomic.Int64 // before the TLS handshake: a relay slow to answer
}

func newMultiRelay(t *testing.T, silent ...string) *multiRelay {
	t.Helper()
	r := &multiRelay{ln: fakeTLSListener(t), silent: map[string]bool{}, closes: map[string]bool{}, sessions: map[string]int{}}
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
	time.Sleep(time.Duration(r.delay.Load()))
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
	silent, closes := r.silent[ip], r.closes[ip]
	r.mu.Unlock()
	if closes {
		return
	}
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
	p.putLocked(ip).noHello = true
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

// A mark from 0.13.0's map (a day) is cut to this build's deadFor.
func TestDeadMap_LongMarkCut(t *testing.T) {
	q := freshPicker(t)
	file := filepath.Join(t.TempDir(), "desync.json")
	writeDeadMap(t, file, q.now().Add(24*time.Hour).Unix())
	q.started(file)
	q.setNetKey("cell:25001")
	if !q.deadNow(deadIP) {
		t.Fatal("not dead from the map")
	}
	if got, want := readMap(t, file)["cell:25001"].Dead[deadIP], q.now().Add(deadFor).Unix(); got != want {
		t.Fatalf("map keeps %d, want the cut %d", got, want)
	}
	// Another apply later (a network back, a restart) grants no more time.
	advance(q, deadFor/2)
	q.setNetKey("cell:25002")
	q.setNetKey("cell:25001")
	if !q.deadNow(deadIP) {
		t.Fatal("not dead after the re-apply")
	}
	advance(q, deadFor/2)
	if q.deadNow(deadIP) {
		t.Fatalf("dead past %s", deadFor)
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
	gen, due := p.recheckDue("1.1.1.1", true)
	if !due {
		t.Fatal("not due")
	}
	p.setNetKey("cell:25002")
	p.setNetKey("cell:25001")
	p.recheckDone(gen, "1.1.1.1", false, true)
	if !p.deadNow("1.1.1.1") {
		t.Fatal("lifted by a stale recheck")
	}
	if _, due := p.recheckDue("1.1.1.1", true); !due {
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

// A direct try in flight when its address turns dead: without a
// ServerHello the app side is closed after the FAIL, no relay session; a
// ServerHello lifts the mark, also from the map.
func TestHandleTCP_InFlightTryTurnedDead(t *testing.T) {
	for _, answer := range [][]byte{nil, serverHello} {
		t.Run(fmt.Sprint(answer != nil), func(t *testing.T) {
			lg := &lockedLog{}
			setLogger(lg)
			t.Cleanup(func() { setLogger(&sliceLogger{}) })
			r := newMultiRelay(t)
			withConns(t)
			setDomains("googlevideo.com")
			t.Cleanup(func() { setDomains("") })
			defer func(w time.Duration) { desyncWait = w }(desyncWait)
			desyncWait = time.Second
			fp := &fakeProtector{ok: true}
			protector = fp
			t.Cleanup(func() { protector = nil })
			SetRaceMs(0)
			t.Cleanup(func() { SetRaceMs(-1) })
			p := directWith(t, `[{"id":"none","spec":""}]`)
			file := filepath.Join(t.TempDir(), "desync.json")
			p.started(file)
			_, ep := testStack(t, r.ln.Addr().String())
			t.Cleanup(func() { endRace(t) })

			hello := clientHello(t, videoName)
			host, port, _ := directServer(t, len(hello), 300*time.Millisecond, answer)
			pe := &tcpPeer{ep: ep, dst: addr4(host.String()), dport: port, sport: 58001, seq: 1000}
			pe.send(header.TCPFlagSyn, nil)
			pe.seq++
			sa := recvFor(t, ep, 58001)
			pe.ack = sa.SequenceNumber() + 1
			pe.send(header.TCPFlagAck, nil)
			pe.send(header.TCPFlagAck|header.TCPFlagPsh, hello)
			time.Sleep(50 * time.Millisecond)
			p.markDead(p.gen, host.String())
			if answer != nil {
				if got := pe.receive(t, len(answer)); string(got) != string(answer) {
					t.Fatalf("app got % x", got)
				}
				if p.deadNow(host.String()) || readMap(t, file)["cell:25099"].Dead[host.String()] != 0 {
					t.Fatalf("still dead; log %q", lg.lines)
				}
				return
			}
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
		})
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
					_, gen, _ := p.pick(ip, "")
					p.markDead(gen, ip)
				case 1:
					if gen, ok := p.recheckDue(ip, true); ok {
						p.recheckDone(gen, ip, i%3 == 0, i%2 == 0)
					}
				case 2:
					name := fmt.Sprintf("rr%d---sn-ab%d-x.googlevideo.com", i%3, g%2)
					if st, gen, _ := p.pick(ip, name); st != nil {
						p.result(gen, st.id, ip, name, time.Now(), i%3 == 0, "x")
						p.countRace(gen, func(t *tally) { t.raced++ })
					}
				case 3:
					p.deadNow(ip)
					if gen, watched := p.silentNow(ip); watched {
						p.relayNoAnswer(gen, ip, i%2 == 0)
					}
					p.directProbe()
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
// hello, or up to deadWait before it: the node is alive, the silence is the
// path's.
func TestConnsByAddr_Answered(t *testing.T) {
	var c connsByAddr
	c.m = map[string]map[net.Conn]net.Conn{}
	since := time.Now()
	early := &watchConn{start: since.Add(-deadWait - 2*time.Second)}
	early.first.Store(int64(time.Second)) // deadWait+1s before since
	defer c.add(deadIP, &net.TCPConn{}, early)()
	if c.answered(deadIP, since) {
		t.Fatal("an answer from long before the hello counted")
	}
	just := &watchConn{start: since.Add(-time.Second)}
	just.first.Store(int64(500 * time.Millisecond)) // just before since
	c.add(deadIP, &net.TCPConn{}, just)()
	if !c.answered(deadIP, since) {
		t.Fatal("an answer just before the hello did not count")
	}
	c.last = nil
	late := &watchConn{start: since}
	late.first.Store(int64(time.Second))
	defer c.add(deadIP, &net.UDPConn{}, late)()
	if !c.answered(deadIP, since) || c.answered(liveIP, since) {
		t.Fatal("answered")
	}
}

// 11.11, Tele2: of a burst of three, one session answered through the relay
// and closed; the other two were silent for deadWait. The closed one's
// answer still counts.
func TestConnsByAddr_AnsweredThenClosed(t *testing.T) {
	var c connsByAddr
	c.m = map[string]map[net.Conn]net.Conn{}
	since := time.Now().Add(-deadWait)
	silent := &watchConn{start: since}
	defer c.add(deadIP, &net.TCPConn{}, silent)()
	quick := &watchConn{start: since}
	quick.first.Store(int64(224 * time.Millisecond))
	c.add(deadIP, &net.UDPConn{}, quick)()
	if !c.answered(deadIP, since) {
		t.Fatal("a closed session's answer did not count")
	}
	if c.answered(deadIP, since.Add(deadWait+time.Second)) || c.answered(liveIP, since) {
		t.Fatal("answered")
	}
	unanswered := &watchConn{start: since}
	c.add(liveIP, &net.UDPConn{}, unanswered)()
	if c.answered(liveIP, since) {
		t.Fatal("a closed session with no byte back counted")
	}
}

// An answer older than answerKept is dropped when another one is kept.
func TestConnsByAddr_OldAnswerDropped(t *testing.T) {
	var c connsByAddr
	c.m = map[string]map[net.Conn]net.Conn{}
	long := time.Now().Add(-answerKept - time.Second)
	old := &watchConn{start: long}
	old.first.Store(int64(time.Millisecond))
	c.add(deadIP, &net.TCPConn{}, old)()
	fresh := &watchConn{start: time.Now()}
	fresh.first.Store(int64(time.Millisecond))
	c.add(liveIP, &net.TCPConn{}, fresh)()
	if _, kept := c.last[deadIP]; kept || len(c.last) != 1 {
		t.Fatalf("kept %v", c.last)
	}
}

// The relay ends a cache node's session at once with nothing back: one
// that answered direct is direct only at once, without waiting deadWait;
// one that never did is not dead, and the VDS is not probed for it.
func TestCheckDead_FastEOF(t *testing.T) {
	for _, hello := range []bool{true, false} {
		t.Run(fmt.Sprint(hello), func(t *testing.T) {
			p, ep, r, lg, _ := deadStack(t)
			deadWait = 3 * time.Second
			r.mu.Lock()
			r.closes[deadIP] = true
			r.mu.Unlock()
			p.mu.Lock()
			a := p.putLocked(deadIP)
			a.hello, a.noHello = hello, !hello
			p.mu.Unlock()
			pe := openHello(t, ep, deadIP, videoName, 59001)
			for {
				tc := recvFor(t, ep, 59001)
				if tc.Flags()&header.TCPFlagFin != 0 {
					pe.ack = tc.SequenceNumber() + uint32(len(tc.Payload())) + 1
					pe.send(header.TCPFlagAck|header.TCPFlagFin, nil)
					break
				}
			}
			deadline := time.Now().Add(time.Second)
			for hello && !p.mutedNow(deadIP) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
			}
			if !hello {
				time.Sleep(time.Second)
			}
			if p.mutedNow(deadIP) != hello || p.deadNow(deadIP) {
				t.Fatalf("muted %v, dead %v; log %q", p.mutedNow(deadIP), p.deadNow(deadIP), lg.lines)
			}
			if want := map[bool]int32{true: 2, false: 1}[hello]; r.upgrades.Load() != want {
				t.Fatalf("%d upgrades", r.upgrades.Load())
			}
		})
	}
}

// probeServer stands for a node a recheck probes directly: it reads one
// TLS record, then sends answer (nil: silence). hits counts the records.
func probeServer(t *testing.T, answer []byte) (net.IP, uint16, *atomic.Int32) {
	t.Helper()
	ip := hostIPv4(t)
	ln, err := net.Listen("tcp", net.JoinHostPort(ip.String(), "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	var hits atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(5 * time.Second))
				hdr := make([]byte, 5)
				if _, err := io.ReadFull(c, hdr); err != nil {
					return
				}
				if _, err := io.ReadFull(c, make([]byte, int(hdr[3])<<8|int(hdr[4]))); err != nil {
					return
				}
				hits.Add(1)
				if answer != nil {
					c.Write(answer)
				}
				io.Copy(io.Discard, c)
			}()
		}
	}()
	return ip, uint16(ln.Addr().(*net.TCPAddr).Port), &hits
}

// recheckStack: a node dead in cell:25001's map for deadFor/2 more, direct
// on with the strategy none, the node answering its direct probe with
// answer, the relay silent to it or echoing.
func recheckStack(t *testing.T, answer []byte, relaySilent bool) (p *pickState, r *multiRelay, ip string, port uint16, hits *atomic.Int32, file string) {
	t.Helper()
	lg := &lockedLog{}
	setLogger(lg)
	t.Cleanup(func() { setLogger(&sliceLogger{}) })
	health.stopped()
	t.Cleanup(health.stopped)
	r = newMultiRelay(t)
	withConns(t)
	oldDead, oldWait := deadWait, desyncWait
	deadWait, desyncWait = 500*time.Millisecond, 500*time.Millisecond
	t.Cleanup(func() { deadWait, desyncWait = oldDead, oldWait; quotaExceeded.Store(false) })
	p = directWith(t, `[{"id":"none","spec":""}]`)
	host, port, hits := probeServer(t, answer)
	ip = host.String()
	if relaySilent {
		r.silent[ip] = true
	}
	file = filepath.Join(t.TempDir(), "desync.json")
	b, _ := json.Marshal(netMapFile{V: 1, Nets: map[string]netEntry{
		"cell:25001": {Win: "none", At: 1, Dead: map[string]int64{ip: time.Now().Add(deadFor / 2).Unix()}},
	}})
	if err := os.WriteFile(file, b, 0o600); err != nil {
		t.Fatal(err)
	}
	p.started(file)
	p.setNetKey("cell:25001")
	testStack(t, r.ln.Addr().String())
	return p, r, ip, port, hits, file
}

func recheckNow(t *testing.T, p *pickState, ip string, port uint16) {
	t.Helper()
	gen, due := p.recheckDue(ip, true)
	if !due {
		t.Fatal("not due")
	}
	recheckDead(gen, health.generation(), ip, port, testCred)
}

// The relay does not reach it, the direct probe gets a ServerHello: no
// longer dead, nothing counted, and no hello: only real traffic sets it.
func TestRecheckDead_DirectAnswers(t *testing.T) {
	p, r, ip, port, hits, file := recheckStack(t, serverHello, true)
	attempts, fails, tl, v := p.attempts, p.fails, p.tallyNow(), p.verdict()
	recheckNow(t, p, ip, port)
	if p.deadNow(ip) || readMap(t, file)["cell:25001"].Dead[ip] != 0 {
		t.Fatal("still dead")
	}
	if hits.Load() != 1 || r.count(ip) != 1 {
		t.Fatalf("%d direct, %d relay probes", hits.Load(), r.count(ip))
	}
	if p.addr(ip).hello || p.attempts != attempts || p.fails != fails || p.tallyNow() != tl || p.verdict() != v {
		t.Fatalf("counted: %+v, %d attempts, %d fails, %+v, %s", p.addr(ip), p.attempts, p.fails, p.tallyNow(), p.verdict())
	}
}

// The direct probe silent, the relay's answers: no longer dead.
func TestRecheckDead_RelayAnswers(t *testing.T) {
	p, _, ip, port, hits, file := recheckStack(t, nil, false)
	recheckNow(t, p, ip, port)
	if p.deadNow(ip) || readMap(t, file)["cell:25001"].Dead[ip] != 0 || hits.Load() != 1 {
		t.Fatalf("still dead, %d direct probes", hits.Load())
	}
}

// Silence both ways, the VDS answering another session meanwhile: dead
// for deadFor from now. With the relay's probe alone (no direct one under
// "nothing works") the same silence leaves the mark as it was.
func TestRecheckDead_SilenceBothWaysExtends(t *testing.T) {
	for _, direct := range []bool{true, false} {
		t.Run(fmt.Sprint(direct), func(t *testing.T) {
			p, _, ip, port, hits, file := recheckStack(t, nil, true)
			if !direct {
				p.mu.Lock()
				p.noneUntil = time.Now().Add(time.Hour)
				p.mu.Unlock()
			}
			until := readMap(t, file)["cell:25001"].Dead[ip]
			go func() {
				time.Sleep(100 * time.Millisecond)
				if c, err := dialRelay(context.Background(), testCred, net.ParseIP(liveIP).To4(), 443); err == nil {
					c.Close()
				}
			}()
			recheckNow(t, p, ip, port)
			got := readMap(t, file)["cell:25001"].Dead[ip]
			if got > until != direct || !p.deadNow(ip) || hits.Load() != map[bool]int32{true: 1}[direct] {
				t.Fatalf("until %d, was %d; %d direct probes", got, until, hits.Load())
			}
		})
	}
}

// The relay down or over the quota: the direct probe alone lifts the mark,
// its silence alone does not extend it.
func TestRecheckDead_DirectAlone(t *testing.T) {
	for _, c := range []struct {
		name string
		do   func(r *multiRelay)
	}{
		{"down", func(r *multiRelay) { r.ln.Close() }},
		{"quota", func(*multiRelay) { quotaExceeded.Store(true) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, r, ip, port, _, _ := recheckStack(t, serverHello, true)
			c.do(r)
			recheckNow(t, p, ip, port)
			if p.deadNow(ip) {
				t.Fatal("still dead")
			}
			p, r, ip, port, _, file := recheckStack(t, nil, true)
			until := readMap(t, file)["cell:25001"].Dead[ip]
			c.do(r)
			recheckNow(t, p, ip, port)
			if got := readMap(t, file)["cell:25001"].Dead[ip]; got != until || !p.deadNow(ip) {
				t.Fatalf("until %d, was %d", got, until)
			}
		})
	}
}

// The relay's half lost to a new relay generation: the turn stays open for
// the next SYN. A relay that heard the hello and stayed silent uses it up.
func TestRecheckDead_RelayLostKeepsTurn(t *testing.T) {
	t.Run("lost", func(t *testing.T) {
		p, _, ip, port, _, _ := recheckStack(t, nil, true)
		gen, _ := p.recheckDue(ip, true)
		recheckDead(gen, health.generation()+1, ip, port, testCred)
		if _, due := p.recheckDue(ip, true); !due {
			t.Fatal("turn used up")
		}
	})
	t.Run("silent", func(t *testing.T) {
		p, _, ip, port, _, _ := recheckStack(t, nil, true)
		recheckNow(t, p, ip, port)
		if _, due := p.recheckDue(ip, true); due {
			t.Fatal("due again")
		}
	})
}

// No direct probe on another visit to the network, under "nothing works"
// or without strategies: the relay alone. An alert is no answer.
func TestRecheckDead_DirectProbeWhen(t *testing.T) {
	t.Run("stale", func(t *testing.T) {
		p, _, ip, port, hits, _ := recheckStack(t, serverHello, false)
		gen, _ := p.recheckDue(ip, true)
		p.setNetKey("cell:25002")
		p.setNetKey("cell:25001")
		recheckDead(gen, health.generation(), ip, port, testCred)
		if !p.deadNow(ip) || hits.Load() != 0 {
			t.Fatalf("dead %v, %d direct probes", p.deadNow(ip), hits.Load())
		}
	})
	for _, c := range []struct {
		name string
		do   func(p *pickState)
	}{
		{"none", func(p *pickState) { p.mu.Lock(); p.noneUntil = time.Now().Add(time.Hour); p.mu.Unlock() }},
		{"no strategies", func(*pickState) { strategies.Store(nil) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			p, r, ip, port, hits, _ := recheckStack(t, serverHello, false)
			c.do(p)
			recheckNow(t, p, ip, port)
			if p.deadNow(ip) || hits.Load() != 0 || r.count(ip) != 1 {
				t.Fatalf("dead %v, %d direct, %d relay probes", p.deadNow(ip), hits.Load(), r.count(ip))
			}
		})
	}
	t.Run("alert", func(t *testing.T) {
		p, _, ip, port, hits, _ := recheckStack(t, alert, true)
		recheckNow(t, p, ip, port)
		if !p.deadNow(ip) || hits.Load() != 1 {
			t.Fatalf("dead %v, %d direct probes", p.deadNow(ip), hits.Load())
		}
	})
}
