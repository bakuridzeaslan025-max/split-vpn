package tunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// startRelay is a fake relay whose n-th connection runs the n-th script (the
// last one repeats). A script returns when the client has gone, so closed
// counting up to accepted is every attempt having closed its conn.
type startRelay struct {
	addr             string
	accepted, closed atomic.Int32
	mu               sync.Mutex
	conns            []net.Conn
	at               []time.Time
}

func newStartRelay(t *testing.T, scripts ...func(net.Conn)) *startRelay {
	t.Helper()
	ln := fakeTLSListener(t)
	r := &startRelay{addr: ln.Addr().String()}
	old := relayEP.Swap(&relayTarget{r.addr, "relay.test", "/app/x", "nl1"})
	t.Cleanup(func() {
		relayEP.Store(old)
		r.mu.Lock()
		defer r.mu.Unlock()
		for _, c := range r.conns {
			c.Close()
		}
	})
	go func() {
		for n := 0; ; n++ {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			r.mu.Lock()
			r.conns = append(r.conns, c)
			r.at = append(r.at, time.Now())
			r.mu.Unlock()
			r.accepted.Add(1)
			go func() {
				defer r.closed.Add(1)
				defer c.Close()
				scripts[min(n, len(scripts)-1)](c)
			}()
		}
	}()
	return r
}

// settled: exactly want attempts reached the relay and all of them are gone.
func (r *startRelay) settled(t *testing.T, want int32) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for (r.accepted.Load() != want || r.closed.Load() != want) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if a, c := r.accepted.Load(), r.closed.Load(); a != want || c != want {
		t.Fatalf("%d attempts reached the relay, %d closed, want %d", a, c, want)
	}
}

func helloSilent(c net.Conn) { io.Copy(io.Discard, c.(*tls.Conn).NetConn()) }

func upgradeSilent(c net.Conn) {
	if _, err := http.ReadRequest(bufio.NewReader(c)); err == nil {
		io.Copy(io.Discard, c)
	}
}

func answers(s string) func(net.Conn) {
	return func(c net.Conn) {
		if _, err := http.ReadRequest(bufio.NewReader(c)); err == nil {
			io.WriteString(c, s)
			io.Copy(io.Discard, c)
		}
	}
}

func closesAtOnce(net.Conn) {}

const answer404 = "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n"

func startProbeTimings(t *testing.T, deadline time.Duration, at ...time.Duration) {
	t.Helper()
	oldAt, oldT := startProbeAt, handshakeTimeout
	startProbeAt, handshakeTimeout = at, deadline
	t.Cleanup(func() { startProbeAt, handshakeTimeout = oldAt, oldT })
}

// startOnFakeFd is Start up to its failure on the fake fd, which comes after
// the probe and leaves the tracker to look at.
func startOnFakeFd(t *testing.T, r *startRelay, host Host) error {
	t.Helper()
	t.Cleanup(func() { protector = nil })
	return Start(9999, r.addr, "relay.test", "/app/x", "nl1", testCred, "", "", "", "", host, nil)
}

func wantFakeFdFailure(t *testing.T, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "fdbased") {
		t.Fatalf("err = %v, want the fd failure after the probe", err)
	}
}

func noProbeGoroutines(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		stacks := string(buf[:runtime.Stack(buf, true)])
		if !strings.Contains(stacks, "tunnel.startProbe") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a probe goroutine outlived the probe:\n%s", filterStacks(stacks))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func wantDownVerdicts(t *testing.T, host *fakeProtector, want ...bool) {
	t.Helper()
	got := host.relayDowns()
	if len(got) != len(want) {
		t.Fatalf("RelayDown %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("RelayDown %v, want %v", got, want)
		}
	}
}

func logHas(lines []string, re string) bool {
	m := regexp.MustCompile(re)
	for _, l := range lines {
		if m.MatchString(l) {
			return true
		}
	}
	return false
}

func TestStartProbe_HealthyStartIsOneHandshake(t *testing.T) {
	r := newStartRelay(t, answers(answer101))
	out := captureLog(t)
	host := &fakeProtector{}
	wantFakeFdFailure(t, startOnFakeFd(t, r, host))
	r.settled(t, 1)
	wantDownVerdicts(t, host, false)
	if LastRelayOK() == 0 {
		t.Fatal("the 101 did not set LastRelayOK")
	}
	if logHas(out.lines, "start probe") {
		t.Fatalf("a healthy start logged: %q", out.lines)
	}
	noProbeGoroutines(t)
}

// startProbe alone: Start's health.ok would wipe a failure counted by a loser.
func TestStartProbe_SilentLoserLeavesNoTrace(t *testing.T) {
	for name, loser := range map[string]func(net.Conn){
		"at the hello":   helloSilent,
		"at the upgrade": upgradeSilent,
	} {
		t.Run(name, func(t *testing.T) {
			startProbeTimings(t, 5*time.Second, 0, 300*time.Millisecond, 4*time.Second)
			r := newStartRelay(t, loser, answers(answer101))
			out := captureLog(t)
			h := withDialEvents(t, 10000)
			t0 := time.Now()
			if err := startProbe(health.generation(), relayEP.Load(), testCred); err != nil {
				t.Fatal(err)
			}
			if d := time.Since(t0); d < 300*time.Millisecond {
				t.Fatalf("won in %s, before the second attempt was due", d)
			}
			if health.down() {
				t.Fatal("the cancelled attempt counted as a failure")
			}
			if logHas(out.lines, "fail") {
				t.Fatalf("the cancelled attempt logged: %q", out.lines)
			}
			if !logHas(out.lines, `^relay: start probe ok: attempt 2 of 2 won in \d`) {
				t.Fatalf("no result line: %q", out.lines)
			}
			r.settled(t, 2)
			if got := h.next(t); got != "nl1 ok" {
				t.Fatalf("event %q", got)
			}
			select {
			case got := <-h.ch:
				t.Fatalf("the cancelled attempt reported %q", got)
			case <-time.After(100 * time.Millisecond):
			}
			noProbeGoroutines(t)
		})
	}
}

// The first attempt is slow, not lost: it may still win over the second.
func TestStartProbe_FirstAttemptCanStillWin(t *testing.T) {
	startProbeTimings(t, 5*time.Second, 0, 100*time.Millisecond, 4*time.Second)
	var arrived sync.WaitGroup
	arrived.Add(2)
	r := newStartRelay(t,
		func(c net.Conn) {
			if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
				return
			}
			arrived.Done()
			arrived.Wait()
			io.WriteString(c, answer101)
			io.Copy(io.Discard, c)
		},
		func(c net.Conn) {
			if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
				return
			}
			arrived.Done()
			io.Copy(io.Discard, c)
		})
	out := captureLog(t)
	if err := startProbe(health.generation(), relayEP.Load(), testCred); err != nil {
		t.Fatal(err)
	}
	if !logHas(out.lines, `^relay: start probe ok: attempt 1 of 2 won in \d`) {
		t.Fatalf("%q", out.lines)
	}
	r.settled(t, 2)
}

// Two 101s at once: the one that lost the race holds a session and closes it.
func TestStartProbe_SecondSuccessIsClosed(t *testing.T) {
	startProbeTimings(t, 5*time.Second, 0, 20*time.Millisecond, 4*time.Second)
	for i := 0; i < 10; i++ {
		var arrived sync.WaitGroup
		arrived.Add(2)
		both := func(c net.Conn) {
			if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
				return
			}
			arrived.Done()
			arrived.Wait()
			io.WriteString(c, answer101)
			io.Copy(io.Discard, c)
		}
		r := newStartRelay(t, both)
		if err := startProbe(health.generation(), relayEP.Load(), testCred); err != nil {
			t.Fatal(err)
		}
		r.settled(t, 2)
	}
	noProbeGoroutines(t)
}

// Through a real fd: Start returns nil, with the second attempt's answer.
func TestStart_TunnelUpOnTheSecondAttempt(t *testing.T) {
	startProbeTimings(t, 5*time.Second, 0, 300*time.Millisecond, 4*time.Second)
	r := newStartRelay(t, helloSilent, answers(answer101))
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Close(fds[1]) })
	resetDNS()
	t.Cleanup(resetDNS)
	host := &fakeProtector{}
	t.Cleanup(func() { protector = nil })
	t0 := time.Now()
	err = Start(fds[0], r.addr, "relay.test", "/app/x", "nl1", testCred, "", "", "", "", host, nil)
	if err != nil {
		syscall.Close(fds[0])
		t.Fatal(err)
	}
	t.Cleanup(Stop)
	if d := time.Since(t0); d < 300*time.Millisecond {
		t.Fatalf("up in %s, before the second attempt was due", d)
	}
	wantDownVerdicts(t, host, false)
	if health.refusing() || health.down() {
		t.Fatal("relay answered, but the tracker says down")
	}
	r.settled(t, 2)
}

func TestStart_AllSilentIsOneVerdictAtTheDeadline(t *testing.T) {
	for _, c := range []struct {
		name   string
		script func(net.Conn)
		result string
	}{
		{"at the hello", helloSilent, "hello_timeout"},
		{"at the upgrade", upgradeSilent, "upgrade_timeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			const deadline = time.Second
			startProbeTimings(t, deadline, 0, 150*time.Millisecond, 600*time.Millisecond)
			r := newStartRelay(t, c.script)
			out := captureLog(t)
			host := &fakeProtector{}
			t0 := time.Now()
			wantFakeFdFailure(t, startOnFakeFd(t, r, host))
			// The upper bound is below 600ms past it: each handshake's own
			// timers alone would run the third attempt to 1.6 s.
			if d := time.Since(t0); d < deadline || d > deadline+400*time.Millisecond {
				t.Fatalf("verdict after %s, want it at %s", d, deadline)
			}
			r.settled(t, 3)
			wantDownVerdicts(t, host, true)
			if !health.refusing() {
				t.Fatal("no refusals after the verdict")
			}
			want := `^relay: start probe failed in \S+, 3 attempts: ` + c.result + `, ` + c.result + `, ` + c.result + `$`
			if !logHas(out.lines, want) {
				t.Fatalf("no %q in %q", want, out.lines)
			}
			r.mu.Lock()
			spread := r.at[2].Sub(r.at[0])
			r.mu.Unlock()
			if spread < 200*time.Millisecond {
				t.Fatalf("all attempts within %s, want them spread up to 600ms", spread)
			}
			noProbeGoroutines(t)
		})
	}
}

// With the real timings: nothing here waits for the 2 s or the 5 s.
func TestStart_AllFailFastIsAQuickVerdict(t *testing.T) {
	r := newStartRelay(t, closesAtOnce)
	out := captureLog(t)
	host := &fakeProtector{}
	t0 := time.Now()
	wantFakeFdFailure(t, startOnFakeFd(t, r, host))
	if d := time.Since(t0); d >= startProbeAt[1] {
		t.Fatalf("verdict after %s: a failure must start the next attempt at once", d)
	}
	r.settled(t, 3)
	wantDownVerdicts(t, host, true)
	if !logHas(out.lines, `^relay: start probe failed in \S+, 3 attempts: `) {
		t.Fatalf("%q", out.lines)
	}
}

func TestStart_FastFailureStartsTheNextAtOnce(t *testing.T) {
	r := newStartRelay(t, closesAtOnce, answers(answer101))
	out := captureLog(t)
	host := &fakeProtector{}
	t0 := time.Now()
	wantFakeFdFailure(t, startOnFakeFd(t, r, host))
	if d := time.Since(t0); d >= startProbeAt[1] {
		t.Fatalf("up after %s: the second attempt waited for its timer", d)
	}
	r.settled(t, 2)
	wantDownVerdicts(t, host, false)
	if !logHas(out.lines, `^relay: start probe ok: attempt 2 of 2 won in \d`) {
		t.Fatalf("%q", out.lines)
	}
}

// A fast failure moves up the next attempt only: the one after it still
// keeps its own time, or one quick error would cost all three handshakes.
func TestStartProbe_FastFailureKeepsTheLastAttemptsTime(t *testing.T) {
	const third = 500 * time.Millisecond
	startProbeTimings(t, 5*time.Second, 0, 200*time.Millisecond, third)
	r := newStartRelay(t, closesAtOnce, helloSilent, answers(answer101))
	out := captureLog(t)
	t0 := time.Now()
	if err := startProbe(health.generation(), relayEP.Load(), testCred); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(t0); d < third {
		t.Fatalf("won in %s, before the third attempt was due", d)
	}
	if !logHas(out.lines, `^relay: start probe ok: attempt 3 of 3 won in \d`) {
		t.Fatalf("%q", out.lines)
	}
	r.settled(t, 3)
}

func TestStart_RejectionFromAnyAttemptIsAtOnce(t *testing.T) {
	for _, c := range []struct {
		name    string
		scripts []func(net.Conn)
	}{
		{"first", []func(net.Conn){answers(answer404)}},
		{"second, first silent", []func(net.Conn){helloSilent, answers(answer404)}},
		{"third, others silent", []func(net.Conn){upgradeSilent, helloSilent, answers(answer404)}},
		{"after a fast failure", []func(net.Conn){closesAtOnce, answers(answer404)}},
	} {
		t.Run(c.name, func(t *testing.T) {
			startProbeTimings(t, 5*time.Second, 0, 100*time.Millisecond, 200*time.Millisecond)
			r := newStartRelay(t, c.scripts...)
			host := &fakeProtector{}
			t0 := time.Now()
			if err := startOnFakeFd(t, r, host); !errors.Is(err, ErrCredRejected) {
				t.Fatalf("err = %v", err)
			}
			if d := time.Since(t0); d > 3*time.Second {
				t.Fatalf("rejected after %s, waited for the deadline", d)
			}
			r.settled(t, int32(len(c.scripts)))
			wantDownVerdicts(t, host)
			if LastRelayOK() != 0 {
				t.Fatal("a refused credential set LastRelayOK")
			}
			noProbeGoroutines(t)
		})
	}
}

// A network change during the probe: what it found is the old network's.
func TestStart_GenerationChangeDropsTheVerdict(t *testing.T) {
	changing := func(next func(net.Conn)) func(net.Conn) {
		return func(c net.Conn) {
			health.newNetwork()
			next(c)
		}
	}
	for _, c := range []struct {
		name   string
		script func(net.Conn)
	}{
		{"silence", changing(helloSilent)},
		{"success", changing(answers(answer101))},
	} {
		t.Run(c.name, func(t *testing.T) {
			startProbeTimings(t, 400*time.Millisecond, 0, 100*time.Millisecond, 200*time.Millisecond)
			r := newStartRelay(t, c.script)
			out := captureLog(t)
			host := &fakeProtector{}
			wantFakeFdFailure(t, startOnFakeFd(t, r, host))
			wantDownVerdicts(t, host)
			if health.refusing() || health.down() {
				t.Fatal("the old network's result stayed on the tracker")
			}
			if logHas(out.lines, "unreachable") {
				t.Fatalf("%q", out.lines)
			}
		})
	}
}

// The verdict dropped, the host still gets one: from the prober, on the new
// network, which Start runs next.
func TestHealth_DroppedStartVerdictLeavesItToTheProber(t *testing.T) {
	h, _, _ := testHealth(t)
	fastVerdict(t)
	host := &chanHost{down: make(chan bool, 4)}
	wake := h.setHost(host)
	gen := h.generation()
	h.newNetwork()
	h.unreachableAtStart(gen)
	if h.known || h.allow() != nil {
		t.Fatalf("known=%v, refusing=%v after a stale verdict", h.known, h.allow() != nil)
	}
	var probes atomic.Int32
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go h.prober(stop, wake, func() error { probes.Add(1); return errTimeout })
	if !verdict(t, host.down) || probes.Load() != verdictAfter {
		t.Fatalf("after %d probes: want RelayDown(true) after %d", probes.Load(), verdictAfter)
	}
}

// upgradePeer: peer is the far end of c's pipe; got closes once it has read
// the request.
func upgradePeer(t *testing.T) (c, peer net.Conn, got chan struct{}) {
	t.Helper()
	c, peer = net.Pipe()
	t.Cleanup(func() { c.Close(); peer.Close() })
	got = make(chan struct{})
	go func() {
		if _, err := http.ReadRequest(bufio.NewReader(peer)); err == nil {
			close(got)
		}
	}()
	return c, peer, got
}

var upgradeEP = &relayTarget{"", "relay.test", "/app/x", ""}

func TestUpgrade_CancelInterruptsAndClosesConn(t *testing.T) {
	for _, c := range []struct {
		name string
		ctx  func() (context.Context, context.CancelFunc)
		want error
	}{
		{"cancel", func() (context.Context, context.CancelFunc) { return context.WithCancel(context.Background()) }, context.Canceled},
		{"deadline", func() (context.Context, context.CancelFunc) {
			return context.WithTimeout(context.Background(), 50*time.Millisecond)
		}, context.DeadlineExceeded},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, peer, got := upgradePeer(t)
			ctx, cancel := c.ctx()
			defer cancel()
			errc := make(chan error, 1)
			go func() {
				_, err := upgrade(ctx, conn, upgradeEP, testCred)
				errc <- err
			}()
			<-got
			if c.want == context.Canceled {
				cancel()
			}
			select {
			case err := <-errc:
				if err != c.want {
					t.Fatalf("err = %v, want %v", err, c.want)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("upgrade ignored the context")
			}
			peer.SetDeadline(time.Now().Add(3 * time.Second))
			if _, err := peer.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
				t.Fatalf("conn not closed: %v", err)
			}
		})
	}
}

func TestUpgrade_CancelAfterItLeavesConnAlone(t *testing.T) {
	conn, peer, got := upgradePeer(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-got
		io.WriteString(peer, answer101)
	}()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := upgrade(ctx, conn, upgradeEP, testCred); err != nil {
		t.Fatal(err)
	}
	cancel()
	time.Sleep(20 * time.Millisecond)
	go io.Copy(io.Discard, peer)
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatalf("a later cancel broke the conn: %v", err)
	}
}

// The race the watcher must not lose: an upgrade that reports success hands
// over a conn that works, even with the cancel landing on the reply.
func TestUpgrade_CancelRacingTheReply(t *testing.T) {
	for i := 0; i < 300; i++ {
		conn, peer, got := upgradePeer(t)
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			<-got
			go cancel()
			io.WriteString(peer, answer101)
			io.Copy(io.Discard, peer)
		}()
		conn.SetDeadline(time.Now().Add(3 * time.Second))
		_, err := upgrade(ctx, conn, upgradeEP, testCred)
		if err != nil {
			if err != context.Canceled {
				t.Fatalf("run %d: err = %v, want the cancel's", i, err)
			}
		} else if _, err := conn.Write([]byte("x")); err != nil {
			t.Fatalf("run %d: upgrade reported success on a closed conn: %v", i, err)
		}
		cancel()
		conn.Close()
		peer.Close()
	}
}

// handleTCP cancels its context right after a dial that worked.
func TestDialRelay_SessionOutlivesTheDialContext(t *testing.T) {
	fakeRelay(t)
	withConns(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	c, err := dialRelay(ctx, testCred, net.IPv4(1, 1, 1, 1), 443)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	time.Sleep(20 * time.Millisecond)
	c.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo %q %v", buf, err)
	}
}

// An app dial's own deadline during the upgrade is still the relay's silence,
// for health and the event alike; only a cancel is the caller's.
func TestRelayHandshake_DeadlineDuringUpgradeCounts(t *testing.T) {
	newStartRelay(t, upgradeSilent)
	out := captureLog(t)
	h := withDialEvents(t, 10000)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	t0 := time.Now()
	if _, _, err := relayHandshake(ctx, health.generation(), relayEP.Load(), testCred); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v", err)
	}
	if d := time.Since(t0); d > 3*time.Second {
		t.Fatalf("handshake outlived its context by %s", d)
	}
	if !health.down() || !logHas(out.lines, "^relay: upgrade failed: ") {
		t.Fatalf("down=%v lines %q", health.down(), out.lines)
	}
	if got := h.next(t); got != "nl1 upgrade_timeout" {
		t.Fatalf("event %q", got)
	}
}
