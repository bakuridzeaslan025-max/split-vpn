package tunnel

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

var errTimeout = &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// freshHealth resets the package tracker for one test, without logging:
// failed dials of one test must not open the breaker for the next.
func freshHealth(t *testing.T) {
	t.Helper()
	reset := func() {
		health.mu.Lock()
		health.now = time.Now
		health.gen++
		health.fails, health.refused, health.pending = 0, 0, false
		health.kinds, health.open = nil, false
		health.host, health.known, health.probeSoon = nil, false, false
		health.mu.Unlock()
		health.lastOK.Store(0)
		health.wake = nil
	}
	reset()
	t.Cleanup(reset)
}

// testHealth is a fresh tracker on a hand-driven clock, logging into out.
func testHealth(t *testing.T) (*relayHealth, *time.Time, *sliceLogger) {
	t.Helper()
	out := captureLog(t)
	now := time.Date(2026, 9, 25, 1, 42, 0, 0, time.UTC)
	return &relayHealth{now: func() time.Time { return now }}, &now, out
}

func captureLog(t *testing.T) *sliceLogger {
	t.Helper()
	out := &sliceLogger{}
	setLogger(out)
	t.Cleanup(func() { setLogger(&sliceLogger{}) })
	return out
}

func fail(h *relayHealth, err error) {
	h.failed(h.generation(), "connect", "tls: connect failed: "+err.Error(), err)
}

// openBreaker fails enough dials, long enough apart, to open h.
func openBreaker(h *relayHealth, now *time.Time) {
	for i := 0; i < breakerAfter; i++ {
		fail(h, errTimeout)
		*now = now.Add(breakerDelay / (breakerAfter - 1))
	}
}

// A 3 h outage with an attempt every 10 s: one full line, one breaker line,
// then only the 5-minute summaries.
func TestHealth_OutageIsFewLines(t *testing.T) {
	h, now, out := testHealth(t)
	for i := 0; i < 3*60*6; i++ {
		if h.allow() == nil {
			fail(h, errTimeout)
		}
		*now = now.Add(10 * time.Second)
	}
	if !strings.HasPrefix(out.lines[0], "tls: connect failed: ") || !strings.Contains(out.lines[1], "refusing app conns") {
		t.Fatalf("head: %q", out.lines[:2])
	}
	summaries := out.lines[2:]
	if want := int(3*time.Hour/summaryEvery) - 1; len(summaries) != want {
		t.Fatalf("%d summaries, want %d:\n%s", len(summaries), want, strings.Join(out.lines, "\n"))
	}
	for _, l := range summaries {
		if !strings.HasPrefix(l, "relay: down for ") {
			t.Fatalf("not a summary: %q", l)
		}
	}
}

// Partial blocking alternates kinds; each is written once, not per switch.
func TestHealth_EachKindLoggedOnce(t *testing.T) {
	h, _, out := testHealth(t)
	reset := errors.New("read tcp 10.0.0.1:5->195.0.2.1:443: connection reset by peer")
	for i := 0; i < 4; i++ {
		fail(h, errTimeout)
		h.failed(h.generation(), "hello", "tls: hello failed: "+reset.Error(), reset)
	}
	if len(out.lines) != 2 || !strings.Contains(out.lines[1], "reset by peer") {
		t.Fatalf("%q", out.lines)
	}
}

func TestErrKind(t *testing.T) {
	for _, c := range []struct {
		err  error
		want string
	}{
		{errTimeout, "timeout"},
		{&net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{}}, "timeout"},
		{context.DeadlineExceeded, "timeout"},
		{fmt.Errorf("relay reply: %v", &net.OpError{Op: "read", Net: "tcp", Err: timeoutErr{}}), "i/o timeout"},
		{errors.New("dial tcp 195.0.2.1:443: connect: network is unreachable"), "network is unreachable"},
		{errors.New("EOF"), "EOF"},
	} {
		if got := errKind(c.err); got != c.want {
			t.Errorf("errKind(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

// Telegram opens several connections at once: a blip that fails them all
// within a second is not an outage worth refusing apps for.
func TestHealth_BurstDoesNotOpenBreaker(t *testing.T) {
	h, _, _ := testHealth(t)
	for i := 0; i < 3*breakerAfter; i++ {
		fail(h, errTimeout)
	}
	if h.allow() != nil {
		t.Fatal("a burst of parallel failures opened the breaker")
	}
}

func TestHealth_BreakerRefusesThenProbes(t *testing.T) {
	h, now, _ := testHealth(t)
	openBreaker(h, now)
	if !errors.Is(h.allow(), errRelayDown) {
		t.Fatal("open breaker let a dial through")
	}
	*now = now.Add(probeInterval)
	if h.allow() != nil {
		t.Fatal("probe not let through")
	}
	if h.allow() == nil {
		t.Fatal("second dial let through within the same probe interval")
	}
	fail(h, errTimeout) // the probe failed
	*now = now.Add(probeInterval - time.Second)
	if h.allow() == nil {
		t.Fatal("next probe came early")
	}
	*now = now.Add(time.Second)
	if h.allow() != nil {
		t.Fatal("next probe not let through")
	}
	h.ok(h.generation())
	if h.allow() != nil || h.down() {
		t.Fatal("still refusing after the relay came back")
	}
}

// Wi-Fi came up while LTE was blocked: late results from the gone network
// don't count, apps stay refused while the prober tries the new one at
// once, and the outage keeps its start.
func TestHealth_NewNetwork(t *testing.T) {
	h, now, out := testHealth(t)
	start := *now
	oldGen := h.generation()
	openBreaker(h, now)
	h.newNetwork()
	if !h.probeSoon || !errors.Is(h.allow(), errRelayDown) {
		t.Fatalf("probeSoon=%v; apps must wait for the prober on the new network", h.probeSoon)
	}
	for i := 0; i < 2*breakerAfter; i++ {
		h.failed(oldGen, "connect", "late", errTimeout)
	}
	h.ok(oldGen)
	if !h.down() {
		t.Fatal("a late success of the gone network ended the outage")
	}
	n := len(out.lines)
	fail(h, errTimeout)
	if !strings.HasPrefix(out.lines[n], "tls: connect failed") {
		t.Fatalf("first failure on the new network not in full: %q", out.lines[n:])
	}
	*now = now.Add(time.Minute)
	h.ok(h.generation())
	want := fmt.Sprintf("relay: back after %s, %d dials failed, 1 app conns refused", now.Sub(start).Round(time.Second), breakerAfter+1)
	if last := out.lines[len(out.lines)-1]; last != want {
		t.Fatalf("got %q, want %q", last, want)
	}
	if h.allow() != nil {
		t.Fatal("still refusing after the relay came back")
	}
}

func TestHealth_StoppedSummarizesAndForgets(t *testing.T) {
	h, now, out := testHealth(t)
	openBreaker(h, now)
	h.stopped()
	if last := out.lines[len(out.lines)-1]; !strings.HasSuffix(last, "app conns refused, tunnel stopped") {
		t.Fatalf("%q", last)
	}
	if h.down() || h.allow() != nil {
		t.Fatal("outage survived Stop")
	}
}

func TestHealth_CanceledIgnored(t *testing.T) {
	h, _, out := testHealth(t)
	h.failed(h.generation(), "connect", "canceled", fmt.Errorf("dial: %w", context.Canceled))
	if h.down() || len(out.lines) != 0 {
		t.Fatal("a caller giving up counted as a relay failure")
	}
}

// Integration: the package tracker is wired into the real dial paths.

func TestDialRelay_OpenBreakerRefusesWithoutDial(t *testing.T) {
	_, got := fakeRelay(t)
	freshHealth(t)
	withConns(t)
	now := time.Now()
	health.now = func() time.Time { return now }
	openBreaker(&health, &now)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := dialRelay(ctx, testCred, net.IPv4(1, 1, 1, 1), 443); !errors.Is(err, errRelayDown) {
		t.Fatalf("err = %v, want errRelayDown", err)
	}
	select {
	case <-got:
		t.Fatal("refused dial reached the relay")
	case <-time.After(200 * time.Millisecond):
	}
	now = now.Add(probeInterval)
	c, err := dialRelay(ctx, testCred, net.IPv4(1, 1, 1, 1), 443)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	c.Close()
	<-got
	if health.down() {
		t.Fatal("accepted probe did not close the breaker")
	}
}

func TestDialTLS_FeedsHealth(t *testing.T) {
	freshHealth(t)
	out := captureLog(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	for i := 0; i < 3; i++ {
		dialTLS(context.Background(), health.generation(), closed, "relay.test")
	}
	if len(out.lines) != 1 || !strings.HasPrefix(out.lines[0], "tls: connect "+closed) || !health.down() {
		t.Fatalf("down=%v lines %q", health.down(), out.lines)
	}
}

// TLS passes, the upgrade gets no 101-or-404 answer: still an outage.
func TestDialRelay_UpgradeFailureCounts(t *testing.T) {
	ln := fakeTLSListener(t)
	withEndpoint(t, ln.Addr().String(), "relay.test", "/app/x")
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Read(make([]byte, 4096))
			c.Close()
		}
	}()
	withConns(t)
	out := captureLog(t)
	if _, err := dialRelay(context.Background(), testCred, net.IPv4(1, 1, 1, 1), 443); err == nil {
		t.Fatal("dial succeeded")
	}
	if !health.down() || len(out.lines) != 1 || !strings.HasPrefix(out.lines[0], "relay: upgrade failed") {
		t.Fatalf("down=%v lines %q", health.down(), out.lines)
	}
}

func TestResolveDNS_QuietDuringOutage(t *testing.T) {
	freshHealth(t)
	resetDNS()
	t.Cleanup(resetDNS)
	out := captureLog(t)
	var calls atomic.Int32
	doh = fakeDoH(t, &calls, func([]byte) ([]byte, error) { return nil, errors.New("boom") })
	setDomains("quiet.test")
	t.Cleanup(func() { setDomains("") })
	q := mustQuery(t, 7, "quiet.test.", dnsmessage.TypeA)

	resolveDNS(q)
	if len(out.lines) != 1 || !strings.Contains(out.lines[0], "doh error") {
		t.Fatalf("live relay: %q", out.lines)
	}
	fail(&health, errTimeout)
	out.lines = nil
	if rcode(resolveDNS(q)) != dnsmessage.RCodeServerFailure || len(out.lines) != 0 {
		t.Fatalf("outage: %q", out.lines)
	}
}

func TestLifecycleResetsHealth(t *testing.T) {
	for name, f := range map[string]func(){"Stop": Stop, "NetworkChanged": NetworkChanged, "NetworkLost": NetworkLost} {
		freshHealth(t)
		now := time.Now()
		health.now = func() time.Time { return now }
		openBreaker(&health, &now)
		f()
		refusing := health.allow() != nil
		if name == "Stop" && refusing {
			t.Errorf("Stop left the breaker open")
		}
		if name != "Stop" && (!refusing || !health.probeSoon) {
			t.Errorf("%s: refusing=%v probeSoon=%v, want the prober on the new network", name, refusing, health.probeSoon)
		}
	}
}

func TestHealth_TellsHostOnceEachWay(t *testing.T) {
	h, now, _ := testHealth(t)
	host := &fakeProtector{}
	h.setHost(host)
	openBreaker(h, now)
	fail(h, errTimeout)
	h.newNetwork()
	h.ok(h.generation())
	h.ok(h.generation())
	if d := host.relayDowns(); len(d) != 2 || !d[0] || d[1] {
		t.Fatalf("RelayDown calls %v, want [true false]", d)
	}
	// A new tunnel's host may still show the old verdict: the first one is
	// always repeated.
	h.setHost(host)
	h.ok(h.generation())
	if d := host.relayDowns(); len(d) != 3 || d[2] {
		t.Fatalf("RelayDown calls %v, want a false after setHost", d)
	}
}

type chanHost struct {
	fakeProtector
	down chan bool
}

func (c *chanHost) RelayDown(down bool) { c.down <- down }

// Nothing to send from the apps: the prober itself notices the relay back
// on a new network, without waiting for its backoff.
func TestProber_ProbesNewNetworkAtOnce(t *testing.T) {
	h, _, _ := testHealth(t)
	host := &chanHost{down: make(chan bool, 4)}
	wake := h.setHost(host)
	h.unreachableAtStart()
	if !<-host.down {
		t.Fatal("no RelayDown(true)")
	}
	var probes atomic.Int32
	stop := make(chan struct{})
	defer close(stop)
	go h.prober(stop, wake, func() error { probes.Add(1); return nil })
	time.Sleep(50 * time.Millisecond)
	if probes.Load() != 0 {
		t.Fatal("probed before the backoff or a new network")
	}
	h.newNetwork()
	select {
	case down := <-host.down:
		if down {
			t.Fatal("RelayDown(true) again")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the prober did not try the new network")
	}
	if probes.Load() != 1 {
		t.Fatalf("%d probes", probes.Load())
	}
}

// No network at start: the tunnel comes up anyway and the host hears that
// the relay is down. Start fails later, on the fake fd, and does not clean
// up the tracker, which is what lets this test look at it.
func TestStart_UnreachableRelayIsNotAnError(t *testing.T) {
	freshHealth(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	host := &fakeProtector{}
	err := Start(9999, closed, "relay.test", "/app/x", testCred, "", "", "", host, nil)
	if err == nil || !strings.Contains(err.Error(), "fdbased") {
		t.Fatalf("err = %v, want the fd failure after the probe", err)
	}
	if d := host.relayDowns(); len(d) != 1 || !d[0] || !errors.Is(health.allow(), errRelayDown) {
		t.Fatalf("RelayDown %v, breaker open=%v", d, health.allow() != nil)
	}
	if LastRelayOK() != 0 {
		t.Fatal("a failed probe set LastRelayOK")
	}
}

// The credential expired during the outage: the relay answering 404 is
// still the relay being back, or the warning would never go.
func TestProber_CredRejectedEndsOutage(t *testing.T) {
	h, _, _ := testHealth(t)
	host := &chanHost{down: make(chan bool, 4)}
	wake := h.setHost(host)
	h.unreachableAtStart()
	<-host.down
	stop := make(chan struct{})
	defer close(stop)
	go h.prober(stop, wake, func() error { return ErrCredRejected })
	h.newNetwork()
	select {
	case down := <-host.down:
		if down {
			t.Fatal("RelayDown(true) again")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a 404 left the outage on")
	}
}

// Only a 101 marks the relay as having carried traffic: a 404 ends the
// outage all the same, but nothing goes through on a refused credential.
func TestProber_LastRelayOKOnlyOn101(t *testing.T) {
	for _, c := range []struct {
		name string
		err  error
	}{{"101", nil}, {"404", ErrCredRejected}, {"timeout", errTimeout}} {
		h, _, _ := testHealth(t)
		host := &chanHost{down: make(chan bool, 4)}
		wake := h.setHost(host)
		h.unreachableAtStart()
		<-host.down
		probed := make(chan struct{}, 4)
		stop := make(chan struct{})
		go h.prober(stop, wake, func() error { probed <- struct{}{}; return c.err })
		awaitProbe := func() {
			select {
			case <-probed:
			case <-time.After(2 * time.Second):
				t.Fatalf("%s: no probe", c.name)
			}
		}
		h.newNetwork()
		awaitProbe()
		if c.err == errTimeout {
			// The next probe begins only once this one's outcome is recorded.
			h.newNetwork()
			awaitProbe()
		} else if verdict(t, host.down) {
			t.Fatalf("%s: RelayDown(true)", c.name)
		}
		close(stop)
		if set := h.lastOK.Load() != 0; set != (c.err == nil) {
			t.Errorf("%s: LastRelayOK set=%v", c.name, set)
		}
	}
}

// A stopped prober finishing its probe must not act again, nor take the
// next one's nudge.
func TestProber_StoppedDoesNotProbe(t *testing.T) {
	h, _, _ := testHealth(t)
	wake := h.setHost(&chanHost{down: make(chan bool, 4)})
	h.unreachableAtStart()
	var probes atomic.Int32
	stop := make(chan struct{})
	close(stop)
	h.newNetwork()
	h.prober(stop, wake, func() error { probes.Add(1); return nil })
	if probes.Load() != 0 {
		t.Fatal("a stopped prober probed")
	}
}

func fastVerdict(t *testing.T) {
	old := verdictRetry
	verdictRetry = 10 * time.Millisecond
	t.Cleanup(func() { verdictRetry = old })
}

// switched runs a prober over h, told the old relay was down, and switches
// to a new one; probe answers for it. Returns the host's verdicts.
func switched(t *testing.T, h *relayHealth, probe func() error) <-chan bool {
	t.Helper()
	fastVerdict(t)
	host := &chanHost{down: make(chan bool, 4)}
	wake := h.setHost(host)
	h.unreachableAtStart()
	<-host.down
	h.newEndpoint(false)
	if h.allow() != nil || h.down() {
		t.Fatal("the old relay's breaker survived the switch")
	}
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go h.prober(stop, wake, probe)
	return host.down
}

func verdict(t *testing.T, down <-chan bool) bool {
	t.Helper()
	select {
	case d := <-down:
		return d
	case <-time.After(2 * time.Second):
		t.Fatal("no verdict for the new endpoint")
		return false
	}
}

// The host heard the old relay was down; the new one is called down again
// only after verdictAfter failed probes, and one success is enough.
func TestProber_VerdictForNewEndpoint(t *testing.T) {
	h, _, _ := testHealth(t)
	var probes atomic.Int32
	down := switched(t, h, func() error { probes.Add(1); return errTimeout })
	if !verdict(t, down) || probes.Load() != verdictAfter || !errors.Is(h.allow(), errRelayDown) {
		t.Fatalf("after %d probes: want RelayDown(true) and refusals after %d", probes.Load(), verdictAfter)
	}

	h2, _, _ := testHealth(t)
	probes.Store(0)
	down = switched(t, h2, func() error {
		if probes.Add(1) < verdictAfter {
			return errTimeout
		}
		return nil
	})
	if verdict(t, down) {
		t.Fatalf("RelayDown(true) after %d probes, the last one succeeded", probes.Load())
	}
}

// A probe of the old relay still running at the switch: its answer is not
// the new relay's verdict, either way.
func TestProber_OldProbeVerdictDropped(t *testing.T) {
	for _, old := range []error{nil, errTimeout} {
		h, _, _ := testHealth(t)
		fastVerdict(t)
		host := &chanHost{down: make(chan bool, 4)}
		wake := h.setHost(host)
		h.unreachableAtStart()
		<-host.down
		started, release := make(chan struct{}), make(chan struct{})
		var probes atomic.Int32
		stop := make(chan struct{})
		go h.prober(stop, wake, func() error {
			if probes.Add(1) == 1 {
				close(started)
				<-release
				return old
			}
			if old == nil {
				return errTimeout
			}
			return nil
		})
		h.newNetwork() // wakes the prober into its first probe
		<-started
		h.newEndpoint(false)
		close(release)
		if got := verdict(t, host.down); got != (old == nil) {
			t.Fatalf("old probe %v: RelayDown(%v), want the new relay's", old, got)
		}
		close(stop)
	}
}

// Registration may go to any endpoint: its failures are not the tunnel's,
// but still logged.
func TestRegister_DoesNotFeedHealth(t *testing.T) {
	freshHealth(t)
	out := captureLog(t)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	for i := 0; i < breakerAfter; i++ {
		if _, err := Register(closed, "relay.test", "/app/x", KindInvite, []byte("x")); err == nil {
			t.Fatal("Register succeeded")
		}
	}
	if health.down() || len(out.lines) != breakerAfter || !strings.HasPrefix(out.lines[0], "tls: connect "+closed+" failed after ") {
		t.Fatalf("down=%v lines %q", health.down(), out.lines)
	}
}

// Failures on the old network do not add up with the new one's.
func TestNewNetwork_ResetsVerdictFails(t *testing.T) {
	h, _, _ := testHealth(t)
	h.setHost(&fakeProtector{})
	gen := h.generation()
	for i := 0; i < verdictAfter-1; i++ {
		h.unreachable(gen)
	}
	h.newNetwork()
	h.unreachable(h.generation())
	if h.known {
		t.Fatal("a failure on the new network made the verdict")
	}
}

// Moving on from a dead relay: the next may be dead too, so apps are
// refused until the prober gets through, not left to their dial timeouts.
func TestProber_SuspectEndpointRefusesUntilAProbeGetsThrough(t *testing.T) {
	h, _, out := testHealth(t)
	fastVerdict(t)
	host := &chanHost{down: make(chan bool, 4)}
	wake := h.setHost(host)
	h.unreachableAtStart()
	<-host.down
	h.newEndpoint(true)
	if !errors.Is(h.allow(), errRelayDown) {
		t.Fatal("suspect endpoint: apps let through before any probe")
	}
	release := make(chan struct{})
	stop := make(chan struct{})
	t.Cleanup(func() { close(stop) })
	go h.prober(stop, wake, func() error { <-release; return nil })
	close(release)
	if verdict(t, host.down) {
		t.Fatal("RelayDown(true) after a probe got through")
	}
	if h.allow() != nil {
		t.Fatal("breaker still open after the probe got through")
	}
	for _, l := range out.lines {
		if strings.Contains(l, "back after") {
			t.Fatalf("nothing was down on this endpoint: %q", l)
		}
	}
}

// A dead new relay: verdictAfter probes verdictRetry apart, then the
// backoff, never a tight loop. A network change during a probe drops that
// one's answer and costs exactly one more probe.
func TestProber_DeadNewEndpointProbeCadence(t *testing.T) {
	for _, suspect := range []bool{false, true} {
		for _, netChange := range []bool{false, true} {
			h, _, _ := testHealth(t)
			fastVerdict(t)
			host := &chanHost{down: make(chan bool, 4)}
			wake := h.setHost(host)
			h.unreachableAtStart()
			<-host.down
			h.newEndpoint(suspect)
			var mu sync.Mutex
			var at []time.Time
			stop := make(chan struct{})
			go h.prober(stop, wake, func() error {
				mu.Lock()
				at = append(at, time.Now())
				n := len(at)
				mu.Unlock()
				if netChange && n == 1 {
					h.newNetwork()
				}
				return errTimeout
			})
			if !verdict(t, host.down) {
				t.Fatalf("suspect=%v netChange=%v: RelayDown(false) from a dead relay", suspect, netChange)
			}
			time.Sleep(20 * verdictRetry)
			close(stop)
			mu.Lock()
			want, first := verdictAfter, 1
			if netChange {
				want, first = verdictAfter+1, 2
			}
			if len(at) != want {
				t.Fatalf("suspect=%v netChange=%v: %d probes, want %d", suspect, netChange, len(at), want)
			}
			for i := first; i < len(at); i++ {
				if gap := at[i].Sub(at[i-1]); gap < verdictRetry {
					t.Fatalf("suspect=%v netChange=%v: probe %d only %s after the last", suspect, netChange, i+1, gap)
				}
			}
			mu.Unlock()
		}
	}
}
