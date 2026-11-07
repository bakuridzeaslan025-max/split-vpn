package vkfallback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"testing"
	"time"

	"splitvpn.local/shared"
)

const (
	testAddr = "example.com:443"
	waitFor  = 5 * time.Second
)

// fakeEngine stands in for olcrtc: Start opens a fake SOCKS5 on the given port.
type fakeEngine struct {
	mu        sync.Mutex
	starts    int
	stops     int // Stops that found a generation running
	dns       []string
	protector Protector
	session   func()
	running   bool
	socks     *fakeSocks
	ports     []int

	readyErr  func(start int) error // nil: every start gets ready
	hold      chan struct{}         // when set, WaitReady waits for it
	reply     byte
	onStop    func()
	stopDelay time.Duration
}

func (e *fakeEngine) Configure(room, keyHex string, fps int) error { return nil }

func (e *fakeEngine) SetDNS(servers string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dns = append(e.dns, servers)
	return nil
}

func (e *fakeEngine) SetProtector(p Protector) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.protector = p
}

func (e *fakeEngine) SetSessionHandler(fn func()) { e.session = fn }

func (e *fakeEngine) Start(port int, user, pass string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running {
		return errors.New("already running")
	}
	socks, err := newFakeSocks(port, user, pass, e.reply)
	if err != nil {
		return err
	}
	e.socks = socks
	e.starts++
	e.running = true
	e.ports = append(e.ports, port)
	return nil
}

func (e *fakeEngine) WaitReady(int) error {
	e.mu.Lock()
	hold, n, readyErr := e.hold, e.starts, e.readyErr
	e.mu.Unlock()
	if hold != nil {
		<-hold
	}
	if readyErr != nil {
		if err := readyErr(n); err != nil {
			return err
		}
	}
	// As in olcrtc: the first session opens before the generation is ready.
	e.session()
	return nil
}

func (e *fakeEngine) IsRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running
}

func (e *fakeEngine) Stop(int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return nil
	}
	if e.onStop != nil {
		e.onStop()
	}
	time.Sleep(e.stopDelay)
	e.stops++
	e.dieLocked()
	return nil
}

func (e *fakeEngine) die() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.dieLocked()
}

func (e *fakeEngine) dieLocked() {
	e.running = false
	if e.socks != nil {
		e.socks.close()
		e.socks = nil
	}
}

func (e *fakeEngine) counts() (starts, stops int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.starts, e.stops
}

// fakeSocks is RFC 1928 with credentials; a successful CONNECT echoes.
type fakeSocks struct {
	l          net.Listener
	user, pass string
	reply      byte

	mu      sync.Mutex
	conns   map[net.Conn]struct{}
	targets []string
}

func newFakeSocks(port int, user, pass string, reply byte) (*fakeSocks, error) {
	l, err := net.Listen("tcp4", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return nil, err
	}
	s := &fakeSocks{l: l, user: user, pass: pass, reply: reply, conns: map[net.Conn]struct{}{}}
	go s.accept()
	return s, nil
}

func (s *fakeSocks) accept() {
	for {
		c, err := s.l.Accept()
		if err != nil {
			return
		}
		s.mu.Lock()
		s.conns[c] = struct{}{}
		s.mu.Unlock()
		go s.serve(c)
	}
}

func (s *fakeSocks) close() {
	_ = s.l.Close()
	s.mu.Lock()
	defer s.mu.Unlock()
	for c := range s.conns {
		_ = c.Close()
	}
}

func (s *fakeSocks) serve(c net.Conn) {
	defer c.Close()
	head := make([]byte, 2)
	if _, err := io.ReadFull(c, head); err != nil {
		return
	}
	methods := make([]byte, head[1])
	if _, err := io.ReadFull(c, methods); err != nil || !slices.Contains(methods, socksUserPass) {
		return
	}
	_, _ = c.Write([]byte{socksVersion, socksUserPass})
	if _, err := io.ReadFull(c, head); err != nil {
		return
	}
	user := make([]byte, head[1])
	if _, err := io.ReadFull(c, user); err != nil {
		return
	}
	plen := make([]byte, 1)
	if _, err := io.ReadFull(c, plen); err != nil {
		return
	}
	pass := make([]byte, plen[0])
	if _, err := io.ReadFull(c, pass); err != nil {
		return
	}
	if string(user) != s.user || string(pass) != s.pass {
		_, _ = c.Write([]byte{1, 1})
		return
	}
	_, _ = c.Write([]byte{1, 0})
	req := make([]byte, 4)
	if _, err := io.ReadFull(c, req); err != nil {
		return
	}
	var host string
	switch req[3] {
	case socksDomain:
		if _, err := io.ReadFull(c, plen); err != nil {
			return
		}
		b := make([]byte, plen[0])
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = string(b)
	case socksIPv4, socksIPv6:
		b := make([]byte, map[byte]int{socksIPv4: 4, socksIPv6: 16}[req[3]])
		if _, err := io.ReadFull(c, b); err != nil {
			return
		}
		host = net.IP(b).String()
	}
	port := make([]byte, 2)
	if _, err := io.ReadFull(c, port); err != nil {
		return
	}
	s.mu.Lock()
	s.targets = append(s.targets, net.JoinHostPort(host, fmt.Sprint(int(port[0])<<8|int(port[1]))))
	s.mu.Unlock()
	_, _ = c.Write([]byte{socksVersion, s.reply, 0, socksIPv4, 0, 0, 0, 0, 0, 0})
	if s.reply == 0 {
		_, _ = io.Copy(c, c)
	}
}

type event struct {
	name, result, step string
	ms, mb             int64
}

type fakeHost struct {
	mu     sync.Mutex
	events []event
}

func (h *fakeHost) Event(name, result, step string, ms, mb int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.events = append(h.events, event{name, result, step, ms, mb})
}

func (h *fakeHost) named(name string) []event {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []event
	for _, e := range h.events {
		if e.name == name {
			out = append(out, e)
		}
	}
	return out
}

type allowAll struct{}

func (allowAll) Protect(int) bool { return true }

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type rig struct {
	f     *fallback
	eng   *fakeEngine
	host  *fakeHost
	clock *fakeClock
}

// newRig returns an enabled fallback on a fake clock. Only the debounce and the
// watch tick run on real time; everything else moves by clock.advance.
func newRig(t *testing.T) *rig {
	t.Helper()
	eng := &fakeEngine{}
	clock := &fakeClock{t: time.Unix(1000, 0)}
	f := newFallback(eng)
	f.now = clock.now
	f.base = clock.now()
	f.t.watch = 2 * time.Millisecond
	f.t.debounce = 20 * time.Millisecond
	f.t.restartGap = 0
	host := &fakeHost{}
	if err := f.configure("https://vk.ru/call/join/x", "00", 30); err != nil {
		t.Fatal(err)
	}
	if err := f.setDNS("10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := f.enable(allowAll{}, host); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		f.disable("user")
		shared.Register(nil)
	})
	return &rig{f, eng, host, clock}
}

func dial(t *testing.T, f *fallback) net.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	conn, err := f.Dial(ctx, "tcp", testAddr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

func dialErr(f *fallback) error {
	ctx, cancel := context.WithTimeout(context.Background(), waitFor)
	defer cancel()
	conn, err := f.Dial(ctx, "tcp", testAddr)
	if err == nil {
		_ = conn.Close()
	}
	return err
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(waitFor)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func wantStarts(t *testing.T, e *fakeEngine, want int) {
	t.Helper()
	eventually(t, fmt.Sprintf("%d starts", want), func() bool {
		starts, _ := e.counts()
		return starts == want
	})
}

// settle gives a timer that must not fire its chance to do so.
func settle(r *rig) { time.Sleep(10 * r.f.t.debounce) }

func TestLazyStart(t *testing.T) {
	r := newRig(t)
	if starts, _ := r.eng.counts(); starts != 0 {
		t.Fatalf("started before the first Dial: %d", starts)
	}
	if got := r.f.State().Phase; got != shared.Off {
		t.Fatalf("State before Dial = %v", got)
	}
	dial(t, r.f)
	dial(t, r.f)
	wantStarts(t, r.eng, 1)
	if got := r.f.State().Phase; got != shared.Working {
		t.Fatalf("State after Dial = %v", got)
	}
	if got := r.eng.socks.targets; len(got) != 2 || got[0] != testAddr {
		t.Fatalf("SOCKS targets = %v", got)
	}
	if r.eng.protector == nil {
		t.Fatal("protector not passed to the engine")
	}
	if got := r.host.named(eventConnect); len(got) != 1 || got[0].result != resultOK {
		t.Fatalf("fallback_connect = %+v", got)
	}
}

func TestEnableRegistersAndDisableClearsFirst(t *testing.T) {
	r := newRig(t)
	if shared.Active() != shared.Fallback(r.f) {
		t.Fatal("Enable did not register")
	}
	dial(t, r.f)
	var activeAtStop shared.Fallback = r.f
	r.eng.mu.Lock()
	r.eng.onStop = func() { activeAtStop = shared.Active() }
	r.eng.mu.Unlock()
	r.f.disable("relay_back")
	if activeAtStop != nil {
		t.Fatalf("Active at Stop = %v, want nil", activeAtStop)
	}
	if shared.Active() != nil {
		t.Fatal("Active after Disable is not nil")
	}
	_, stops := r.eng.counts()
	if stops != 1 {
		t.Fatalf("stops = %d", stops)
	}
}

func TestNeedsConfigAndProtector(t *testing.T) {
	f := newFallback(&fakeEngine{})
	if err := f.enable(allowAll{}, nil); !errors.Is(err, errNotConfigured) {
		t.Fatalf("enable before configure: %v", err)
	}
	if err := f.configure("room", "00", 0); err != nil {
		t.Fatal(err)
	}
	if err := f.enable(nil, nil); !errors.Is(err, errNoProtector) {
		t.Fatalf("enable without protector: %v", err)
	}
}

func TestNoStartWithoutSystemDNS(t *testing.T) {
	eng := &fakeEngine{}
	f := newFallback(eng)
	if err := f.configure("room", "00", 0); err != nil {
		t.Fatal(err)
	}
	if err := f.enable(allowAll{}, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.disable("user") })
	if err := dialErr(f); !errors.Is(err, errNoDNS) {
		t.Fatalf("Dial without DNS: %v", err)
	}
	if starts, _ := eng.counts(); starts != 0 {
		t.Fatalf("started without DNS: %d", starts)
	}
	if err := f.setDNS(""); err != nil {
		t.Fatal(err)
	}
	if len(eng.dns) != 0 {
		t.Fatalf("an empty list reached the engine: %v", eng.dns)
	}
	if err := f.setDNS("10.0.0.2 10.0.0.3"); err != nil {
		t.Fatal(err)
	}
	dial(t, f)
	wantStarts(t, eng, 1)
	if err := f.setDNS("10.0.0.4"); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(eng.dns, []string{"10.0.0.2 10.0.0.3", "10.0.0.4"}) {
		t.Fatalf("dns = %v", eng.dns)
	}
	if _, stops := eng.counts(); stops != 0 {
		t.Fatalf("a DNS change stopped the call: %d", stops)
	}
}

func TestDialDoesNotOutliveItsContextButTheStartDoes(t *testing.T) {
	r := newRig(t)
	r.eng.hold = make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := r.f.Dial(ctx, "tcp", testAddr); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial with a short context: %v", err)
	}
	if got := r.f.State().Phase; got != shared.Connecting {
		t.Fatalf("State = %v", got)
	}
	close(r.eng.hold)
	dial(t, r.f)
	wantStarts(t, r.eng, 1)
}

func TestIdleStopThenLazyRestart(t *testing.T) {
	r := newRig(t)
	conn := dial(t, r.f)
	r.clock.advance(r.f.t.idle - time.Second)
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	r.clock.advance(r.f.t.idle - time.Second)
	settle(r)
	if _, stops := r.eng.counts(); stops != 0 {
		t.Fatalf("stopped although traffic went %v ago", r.f.t.idle-time.Second)
	}

	r.clock.advance(2 * time.Second)
	eventually(t, "idle stop", func() bool { _, stops := r.eng.counts(); return stops == 1 })
	eventually(t, "fallback_off", func() bool { return len(r.host.named(eventOff)) == 1 })
	off := r.host.named(eventOff)[0]
	if off.step != offIdle || off.result != resultOK || off.mb != 1 {
		t.Fatalf("fallback_off = %+v", off)
	}
	if want := (2 * r.f.t.idle).Milliseconds(); off.ms != want {
		t.Fatalf("fallback_off ms = %d, want %d", off.ms, want)
	}
	if shared.Active() == nil {
		t.Fatal("an idle stop must leave the fallback registered")
	}
	if got := r.f.State().Phase; got != shared.Off {
		t.Fatalf("State after idle = %v", got)
	}

	dial(t, r.f)
	wantStarts(t, r.eng, 2)
}

func TestDialAfterDisableFails(t *testing.T) {
	r := newRig(t)
	dial(t, r.f)
	var holder shared.Fallback = r.f // as a caller that read Active before the clear
	r.f.disable("relay_back")
	if _, err := holder.Dial(context.Background(), "tcp", testAddr); !errors.Is(err, errOff) {
		t.Fatalf("late Dial: %v", err)
	}
	if starts, _ := r.eng.counts(); starts != 1 {
		t.Fatalf("a late Dial started the call: %d", starts)
	}
}

func TestDialWaitingForStartFailsOnDisable(t *testing.T) {
	r := newRig(t)
	r.eng.hold = make(chan struct{})
	defer close(r.eng.hold)
	done := make(chan error, 1)
	go func() { done <- dialErr(r.f) }()
	wantStarts(t, r.eng, 1)
	r.f.disable("relay_back")
	select {
	case err := <-done:
		if !errors.Is(err, errStopped) {
			t.Fatalf("waiting Dial: %v", err)
		}
	case <-time.After(waitFor):
		t.Fatal("Dial did not return after Disable")
	}
	if got := r.host.named(eventConnect); len(got) != 0 {
		t.Fatalf("a stop we asked for is not a connect result: %+v", got)
	}
}

func TestDialInHandshakeDuringStopFails(t *testing.T) {
	r := newRig(t)
	dial(t, r.f)
	r.f.mu.Lock()
	cur := r.f.cur
	r.f.mu.Unlock()
	r.f.disable("relay_back")
	if r.f.dialed(cur) {
		t.Fatal("dialed accepted a stopped run")
	}
}

func TestEnableAgainStartsFresh(t *testing.T) {
	r := newRig(t)
	dial(t, r.f)
	r.eng.mu.Lock()
	r.eng.stopDelay = 20 * time.Millisecond
	r.eng.mu.Unlock()
	r.f.disable("relay_back")
	if err := r.f.enable(allowAll{}, r.host); err != nil {
		t.Fatal(err)
	}
	dial(t, r.f)
	wantStarts(t, r.eng, 2)
	if got := r.f.State().Phase; got != shared.Working {
		t.Fatalf("State = %v", got)
	}
}

func TestRestartWaitsForASlowStop(t *testing.T) {
	r := newRig(t)
	r.eng.mu.Lock()
	r.eng.stopDelay = 20 * time.Millisecond
	r.eng.mu.Unlock()
	dial(t, r.f)
	r.f.networkEvent(true)
	r.clock.advance(time.Second)
	wantStarts(t, r.eng, 2)
	dial(t, r.f)
	if starts, stops := r.eng.counts(); starts != 2 || stops != 1 {
		t.Fatalf("starts = %d, stops = %d", starts, stops)
	}
}

func TestNetworkRestartDebounce(t *testing.T) {
	r := newRig(t)
	dial(t, r.f)
	r.clock.advance(time.Second)
	for range 5 {
		r.f.networkEvent(true)
		time.Sleep(r.f.t.debounce / 10)
	}
	wantStarts(t, r.eng, 2)
	settle(r)
	starts, stops := r.eng.counts()
	if starts != 2 || stops != 1 {
		t.Fatalf("after 5 quick events: starts = %d, stops = %d", starts, stops)
	}
	dial(t, r.f)
}

func TestNetworkEventWithoutCallDoesNothing(t *testing.T) {
	r := newRig(t)
	r.f.networkEvent(true)
	settle(r)
	if starts, _ := r.eng.counts(); starts != 0 {
		t.Fatalf("a network event started the call: %d", starts)
	}
}

func TestCallStartedOnTheNewNetworkIsNotRestarted(t *testing.T) {
	r := newRig(t)
	r.f.t.debounce = 100 * time.Millisecond
	dial(t, r.f)
	r.f.networkEvent(false)
	eventually(t, "stop", func() bool { _, stops := r.eng.counts(); return stops == 1 })
	r.f.networkEvent(true)
	r.clock.advance(time.Second)
	dial(t, r.f)
	settle(r)
	if starts, stops := r.eng.counts(); starts != 2 || stops != 1 {
		t.Fatalf("starts = %d, stops = %d", starts, stops)
	}
}

func TestNetworkRestartGap(t *testing.T) {
	r := newRig(t)
	r.f.t.restartGap = 30 * time.Millisecond
	dial(t, r.f)
	r.f.networkEvent(true)
	settle(r)
	if starts, _ := r.eng.counts(); starts != 1 {
		t.Fatalf("restarted inside the gap: %d", starts)
	}
	r.clock.advance(time.Second)
	wantStarts(t, r.eng, 2)
}

func TestNetworkLostStopsAndChangedResumes(t *testing.T) {
	r := newRig(t)
	dial(t, r.f)
	r.clock.advance(time.Second)
	r.f.networkEvent(false)
	if err := dialErr(r.f); !errors.Is(err, errNoNetwork) {
		t.Fatalf("Dial with no network: %v", err)
	}
	eventually(t, "stop", func() bool { _, stops := r.eng.counts(); return stops == 1 })
	if starts, _ := r.eng.counts(); starts != 1 {
		t.Fatalf("starts = %d", starts)
	}
	if got := r.host.named(eventOff); len(got) != 0 {
		t.Fatalf("losing the network is not fallback_off: %+v", got)
	}
	r.f.networkEvent(true)
	wantStarts(t, r.eng, 2)
	dial(t, r.f)
}

func TestLostThenChangedInsideDebounceIsOneRestart(t *testing.T) {
	r := newRig(t)
	dial(t, r.f)
	r.clock.advance(time.Second)
	r.f.networkEvent(false)
	r.f.networkEvent(true)
	wantStarts(t, r.eng, 2)
	settle(r)
	if starts, stops := r.eng.counts(); starts != 2 || stops != 1 {
		t.Fatalf("starts = %d, stops = %d", starts, stops)
	}
}

func TestNoResumeAfterIdleTimePassed(t *testing.T) {
	r := newRig(t)
	dial(t, r.f)
	r.f.networkEvent(false)
	eventually(t, "stop", func() bool { _, stops := r.eng.counts(); return stops == 1 })
	r.clock.advance(r.f.t.idle)
	r.f.networkEvent(true)
	settle(r)
	if starts, _ := r.eng.counts(); starts != 1 {
		t.Fatalf("resumed with nothing to carry: %d", starts)
	}
	dial(t, r.f)
	wantStarts(t, r.eng, 2)
}

// The text olcrtc's WaitReady really returned for a fake VK API answering 14,
// from a run of the fork against a local server.
func captchaErr() error {
	return errors.New(`supervisor: max failover cycles exceeded after 1 cycle(s): ` +
		`profile "https://vk.ru/call/join/x": run public client: client: failed to create link: ` +
		`open engine session: auth provider rejected the request: ` +
		`vkcalls: auth.getAnonymToken: api error 14 (Captcha needed)`)
}

func TestCaptchaBackoffGrows(t *testing.T) {
	r := newRig(t)
	r.eng.readyErr = func(int) error { return captchaErr() }
	base := r.f.t.captchaBase

	for i, wait := range []time.Duration{base, 2 * base, 4 * base} {
		if err := dialErr(r.f); err == nil || errors.Is(err, errBackoff) {
			t.Fatalf("attempt %d: %v", i, err)
		}
		wantStarts(t, r.eng, i+1)
		eventually(t, "stop", func() bool { return !r.eng.IsRunning() })

		for range 3 {
			if err := dialErr(r.f); !errors.Is(err, errBackoff) {
				t.Fatalf("Dial inside the pause: %v", err)
			}
		}
		r.clock.advance(wait - time.Second)
		if err := dialErr(r.f); !errors.Is(err, errBackoff) {
			t.Fatalf("Dial a second before the pause %v ends: %v", wait, err)
		}
		if starts, _ := r.eng.counts(); starts != i+1 {
			t.Fatalf("started during the pause: %d", starts)
		}
		r.clock.advance(time.Second)
	}

	eventually(t, "events", func() bool { return len(r.host.named(eventCaptcha)) == 3 })
	for _, e := range r.host.named(eventCaptcha) {
		if e.result != resultBackoff {
			t.Fatalf("fallback_captcha = %+v", e)
		}
	}
	for _, e := range r.host.named(eventConnect) {
		if e.result != resultFail || e.step != stepCaptcha {
			t.Fatalf("fallback_connect = %+v", e)
		}
	}
	if got := r.f.State(); got.Phase != shared.Error || got.Reason != stepCaptcha {
		t.Fatalf("State = %+v", got)
	}
}

func TestCaptchaBackoffIsCapped(t *testing.T) {
	r := newRig(t)
	r.f.mu.Lock()
	for range 100 {
		r.f.failedLocked(stepCaptcha)
	}
	got := r.f.notBefore.Sub(r.clock.t)
	r.f.mu.Unlock()
	if got != r.f.t.captchaMax {
		t.Fatalf("pause = %v", got)
	}
}

func TestCaptchaPauseSurvivesReEnableAndNetworkChange(t *testing.T) {
	r := newRig(t)
	r.eng.readyErr = func(int) error { return captchaErr() }
	if err := dialErr(r.f); err == nil {
		t.Fatal("no error")
	}
	eventually(t, "captcha event", func() bool { return len(r.host.named(eventCaptcha)) == 1 })
	r.f.networkEvent(true)
	if err := dialErr(r.f); !errors.Is(err, errBackoff) {
		t.Fatalf("after a network change: %v", err)
	}
	r.f.disable("relay_back")
	if err := r.f.enable(allowAll{}, r.host); err != nil {
		t.Fatal(err)
	}
	if err := dialErr(r.f); !errors.Is(err, errBackoff) {
		t.Fatalf("after Disable and Enable: %v", err)
	}
	if starts, _ := r.eng.counts(); starts != 1 {
		t.Fatalf("starts = %d", starts)
	}
}

func TestTransportFailurePausesUntilNewNetwork(t *testing.T) {
	r := newRig(t)
	r.eng.readyErr = func(start int) error {
		if start == 1 {
			return errors.New("no route")
		}
		return nil
	}
	if err := dialErr(r.f); err == nil {
		t.Fatal("no error")
	}
	if got := r.host.named(eventConnect); len(got) != 1 || got[0].step != stepTransport {
		t.Fatalf("fallback_connect = %+v", got)
	}
	if got := r.host.named(eventCaptcha); len(got) != 0 {
		t.Fatalf("fallback_captcha = %+v", got)
	}
	if err := dialErr(r.f); !errors.Is(err, errBackoff) {
		t.Fatalf("second Dial: %v", err)
	}
	r.f.networkEvent(true)
	dial(t, r.f)
	wantStarts(t, r.eng, 2)
}

func TestErrorTextDoesNotReachCaller(t *testing.T) {
	r := newRig(t)
	r.eng.readyErr = func(int) error { return errors.New("join https://vk.ru/call/join/secret failed") }
	err := dialErr(r.f)
	if err == nil || err.Error() != "vkfallback: start failed: transport" {
		t.Fatalf("Dial: %v", err)
	}
}

func TestStepOf(t *testing.T) {
	for _, tc := range []struct {
		err  string
		want string
	}{
		{captchaErr().Error(), stepCaptcha},
		{"vkcalls: join: api error 5 (auth failed)", stepAuth},
		{"vkcalls: join: api error 140 (other)", stepAuth},
		{"vkcalls: preview: missing_secret", stepTransport},
		{"handshake: open control stream: timeout", stepTransport},
		// olcrtc wraps a failed DNS lookup as a provider rejection too.
		{"open engine session: auth provider rejected the request: vkcalls: do: Post: dial failed", stepTransport},
		{"olcRTC runtime readiness timed out", stepTransport},
	} {
		if got := stepOf(errors.New(tc.err)); got != tc.want {
			t.Errorf("stepOf(%q) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestGenerationDeathIsFallbackOffError(t *testing.T) {
	r := newRig(t)
	dial(t, r.f)
	r.eng.die()
	eventually(t, "fallback_off", func() bool { return len(r.host.named(eventOff)) == 1 })
	if off := r.host.named(eventOff)[0]; off.step != offError || off.result != resultOK {
		t.Fatalf("fallback_off = %+v", off)
	}
	if got := r.f.State(); got.Phase != shared.Error || got.Reason != stepTransport {
		t.Fatalf("State = %+v", got)
	}
	if err := dialErr(r.f); !errors.Is(err, errBackoff) {
		t.Fatalf("a call that died at once must pause the next join: %v", err)
	}
	r.clock.advance(r.f.t.retryBase)
	dial(t, r.f)
	wantStarts(t, r.eng, 2)
}

func TestLongCallDeathStillPauses(t *testing.T) {
	r := newRig(t)
	dial(t, r.f)
	r.clock.advance(4 * time.Minute)
	r.eng.die()
	eventually(t, "fallback_off", func() bool { return len(r.host.named(eventOff)) == 1 })
	if err := dialErr(r.f); !errors.Is(err, errBackoff) {
		t.Fatalf("Dial after a death: %v", err)
	}
	if starts, _ := r.eng.counts(); starts != 1 {
		t.Fatalf("starts = %d", starts)
	}
}

// useJoins makes n starts, each of a call that then stops for idleness.
func useJoins(t *testing.T, r *rig, n int) {
	t.Helper()
	offs := len(r.host.named(eventOff))
	starts, _ := r.eng.counts()
	for i := range n {
		dial(t, r.f)
		wantStarts(t, r.eng, starts+i+1)
		r.clock.advance(r.f.t.idle)
		eventually(t, "idle stop", func() bool { return len(r.host.named(eventOff)) == offs+i+1 })
	}
}

func TestJoinBudgetCoversCallsThatWork(t *testing.T) {
	r := newRig(t)
	r.f.t.joinWindow = 10 * r.f.t.idle
	useJoins(t, r, r.f.t.joinBudget)
	if err := dialErr(r.f); !errors.Is(err, errBudget) {
		t.Fatalf("seventh join: %v", err)
	}
	if got := r.f.State(); got.Phase != shared.Error || got.Reason != reasonBudget {
		t.Fatalf("State = %+v", got)
	}
	r.f.disable("relay_back")
	if err := r.f.enable(allowAll{}, r.host); err != nil {
		t.Fatal(err)
	}
	if err := dialErr(r.f); !errors.Is(err, errBudget) {
		t.Fatalf("a new Enable must not refill the budget: %v", err)
	}
	if starts, _ := r.eng.counts(); starts != r.f.t.joinBudget {
		t.Fatalf("starts = %d", starts)
	}
}

func TestJoinBudgetWindowSlides(t *testing.T) {
	r := newRig(t)
	r.f.t.joinWindow = 10 * r.f.t.idle
	useJoins(t, r, 6)
	// Joins went at 0..5 idle periods and the clock is at 6: the oldest leaves
	// the window at 10.
	r.clock.advance(4*r.f.t.idle - time.Second)
	if err := dialErr(r.f); !errors.Is(err, errBudget) {
		t.Fatalf("a second before the oldest join expires: %v", err)
	}
	r.clock.advance(time.Second)
	dial(t, r.f)
	wantStarts(t, r.eng, 7)
}

func TestSessionOpensCountAsJoinsAndSpendStopsTheCall(t *testing.T) {
	r := newRig(t)
	conn := dial(t, r.f)
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	r.f.mu.Lock()
	joins := len(r.f.joins)
	r.f.mu.Unlock()
	if joins != 1 {
		t.Fatalf("the first session is the start's own join, joins = %d", joins)
	}
	for range 4 {
		r.eng.session()
	}
	if _, stops := r.eng.counts(); stops != 0 {
		t.Fatalf("stopped at 5 joins: %d", stops)
	}
	r.eng.session()
	eventually(t, "stop", func() bool { _, stops := r.eng.counts(); return stops == 1 })
	eventually(t, "fallback_off", func() bool { return len(r.host.named(eventOff)) == 1 })
	if off := r.host.named(eventOff)[0]; off.step != offError || off.mb != 1 {
		t.Fatalf("fallback_off = %+v", off)
	}
	if err := dialErr(r.f); !errors.Is(err, errBudget) {
		t.Fatalf("Dial with the budget spent: %v", err)
	}
	if got := r.f.State(); got.Phase != shared.Error || got.Reason != reasonBudget {
		t.Fatalf("State = %+v", got)
	}
	r.clock.advance(r.f.t.joinWindow)
	dial(t, r.f)
	wantStarts(t, r.eng, 2)
}

func TestSixthStartRunsUntilItsFirstRejoin(t *testing.T) {
	r := newRig(t)
	r.f.t.joinWindow = 10 * r.f.t.idle
	useJoins(t, r, 5)
	dial(t, r.f)
	wantStarts(t, r.eng, 6)
	settle(r)
	if !r.eng.IsRunning() {
		t.Fatal("the last join allowed must be usable")
	}
	r.eng.session()
	eventually(t, "stop", func() bool { return !r.eng.IsRunning() })
}

func TestBudgetStopOfACallNotReadyIsNotFallbackOff(t *testing.T) {
	r := newRig(t)
	r.f.mu.Lock()
	for range r.f.t.joinBudget - 1 {
		r.f.joins = append(r.f.joins, r.clock.t)
	}
	r.f.mu.Unlock()
	r.eng.hold = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- dialErr(r.f) }()
	wantStarts(t, r.eng, 1)
	r.eng.session()
	r.eng.session()
	if err := <-done; !errors.Is(err, errStopped) {
		t.Fatalf("Dial: %v", err)
	}
	close(r.eng.hold)
	eventually(t, "stop", func() bool { return !r.eng.IsRunning() })
	if got := r.host.named(eventOff); len(got) != 0 {
		t.Fatalf("fallback_off = %+v", got)
	}
}

func TestFlappingCallCannotExceedBudget(t *testing.T) {
	r := newRig(t)
	dial(t, r.f)
	for i := range r.f.t.joinBudget - 1 {
		r.clock.advance(time.Second)
		r.f.networkEvent(true)
		wantStarts(t, r.eng, i+2)
	}
	// Up to here each restart was allowed. The next finds no room, stops the
	// call that was on the old network and reports it.
	r.clock.advance(time.Second)
	r.f.networkEvent(true)
	eventually(t, "stop", func() bool { return !r.eng.IsRunning() })
	eventually(t, "fallback_off", func() bool { return len(r.host.named(eventOff)) == 1 })
	if off := r.host.named(eventOff)[0]; off.step != offError {
		t.Fatalf("fallback_off = %+v", off)
	}
	for range 3 {
		r.clock.advance(time.Second)
		r.f.networkEvent(true)
	}
	settle(r)
	if starts, _ := r.eng.counts(); starts != r.f.t.joinBudget {
		t.Fatalf("starts = %d", starts)
	}
}

func TestNetworkClearingBackoffCannotExceedBudget(t *testing.T) {
	r := newRig(t)
	r.eng.readyErr = func(int) error { return errors.New("no route") }
	for range 3 * r.f.t.joinBudget {
		_ = dialErr(r.f)
		r.f.networkEvent(true)
	}
	if starts, _ := r.eng.counts(); starts != r.f.t.joinBudget {
		t.Fatalf("starts = %d", starts)
	}
	if err := dialErr(r.f); !errors.Is(err, errBudget) {
		t.Fatalf("Dial: %v", err)
	}
}

func TestFailingDialsDoNotHoldACallOpen(t *testing.T) {
	r := newRig(t)
	r.eng.reply = 5
	_ = dialErr(r.f)
	wantStarts(t, r.eng, 1)
	for range 4 {
		r.clock.advance(r.f.t.idle / 5)
		if err := dialErr(r.f); err == nil {
			t.Fatal("Dial succeeded")
		}
	}
	r.clock.advance(r.f.t.idle / 5)
	eventually(t, "idle stop", func() bool { return len(r.host.named(eventOff)) == 1 })
	if off := r.host.named(eventOff)[0]; off.step != offIdle || off.result != resultFail {
		t.Fatalf("fallback_off = %+v", off)
	}
}

// The Dial that caused the join may give up before the call is ready, leaving
// nothing but the start itself to say the call is wanted.
func TestStartAfterIdleSurvivesItsDialGivingUp(t *testing.T) {
	r := newRig(t)
	dial(t, r.f)
	r.clock.advance(r.f.t.idle)
	eventually(t, "idle stop", func() bool { return len(r.host.named(eventOff)) == 1 })

	hold := make(chan struct{})
	r.eng.mu.Lock()
	r.eng.hold = hold
	r.eng.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	if _, err := r.f.Dial(ctx, "tcp", testAddr); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Dial with a short context: %v", err)
	}
	wantStarts(t, r.eng, 2)
	close(hold)
	eventually(t, "second fallback_connect", func() bool { return len(r.host.named(eventConnect)) == 2 })

	settle(r)
	if len(r.host.named(eventOff)) != 1 || !r.eng.IsRunning() {
		t.Fatalf("the new call stopped: fallback_off = %+v", r.host.named(eventOff))
	}
	dial(t, r.f)
	if starts, stops := r.eng.counts(); starts != 2 || stops != 1 {
		t.Fatalf("starts = %d, stops = %d", starts, stops)
	}
}

// Run A's stop is deferred outside the locks; by the time it arrives a Dial has
// started run B, whose startEngine already stopped A's generation.
func TestLateStopOfAnOldRunSparesTheNewOne(t *testing.T) {
	r := newRig(t)
	dial(t, r.f)
	r.f.mu.Lock()
	r.f.dropLocked(r.f.cur)
	r.f.mu.Unlock()
	dial(t, r.f)
	wantStarts(t, r.eng, 2)

	r.f.stopEngine()

	if !r.eng.IsRunning() {
		t.Fatal("run A's late stop killed run B")
	}
	if _, stops := r.eng.counts(); stops != 1 {
		t.Fatalf("stops = %d", stops)
	}
	dial(t, r.f)
}

func TestSocksFailure(t *testing.T) {
	r := newRig(t)
	r.eng.reply = 5
	err := dialErr(r.f)
	if err == nil || err.Error() != "vkfallback: SOCKS CONNECT failed: reply 5" {
		t.Fatalf("Dial: %v", err)
	}
	wantStarts(t, r.eng, 1)
	if got := r.host.named(eventHelped); len(got) != 0 {
		t.Fatalf("a refused CONNECT helped: %+v", got)
	}
	if got := r.f.State().Phase; got != shared.Working {
		t.Fatalf("a refused CONNECT is not the call failing: %v", got)
	}
	r.f.disable("relay_back")
	if off := r.host.named(eventOff)[0]; off.result != resultFail || off.mb != 0 {
		t.Fatalf("fallback_off = %+v", off)
	}
}

func TestSocksWrongCredentials(t *testing.T) {
	s, err := newFakeSocks(0, "user", "pass", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	port := s.l.Addr().(*net.TCPAddr).Port
	if _, err := socksDial(context.Background(), port, "user", "wrong", testAddr); !errors.Is(err, errSocksAuth) {
		t.Fatalf("wrong password: %v", err)
	}
}

func TestSocksSilentServerEndsWithContext(t *testing.T) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err = socksDial(ctx, l.Addr().(*net.TCPAddr).Port, "u", "p", testAddr)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("silent server: %v", err)
	}
}

func TestSocksAddressTypes(t *testing.T) {
	s, err := newFakeSocks(0, "u", "p", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer s.close()
	port := s.l.Addr().(*net.TCPAddr).Port
	for _, addr := range []string{"1.1.1.1:443", "[2606:4700::1111]:443", "example.com:80"} {
		conn, err := socksDial(context.Background(), port, "u", "p", addr)
		if err != nil {
			t.Fatalf("%s: %v", addr, err)
		}
		_ = conn.Close()
		eventually(t, addr, func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return len(s.targets) > 0 && s.targets[len(s.targets)-1] == addr
		})
	}
	if _, err := socksDial(context.Background(), port, "u", "p", "no-port"); err == nil {
		t.Fatal("an address without a port passed")
	}
}

func TestOnlyTCP(t *testing.T) {
	r := newRig(t)
	if _, err := r.f.Dial(context.Background(), "udp", testAddr); !errors.Is(err, errNetwork) {
		t.Fatalf("udp: %v", err)
	}
}

func TestByteCountingAndHelpedOncePerEnable(t *testing.T) {
	r := newRig(t)
	r.clock.advance(1234 * time.Millisecond)
	conn := dial(t, r.f)

	// 3 MiB out and back: 6 MiB in both directions, 7 with the extra byte.
	payload := make([]byte, 3<<20)
	go func() { _, _ = conn.Write(payload) }()
	if _, err := io.ReadFull(conn, make([]byte, len(payload))); err != nil {
		t.Fatal(err)
	}
	if got := r.f.bytes.Load(); got != 6<<20 {
		t.Fatalf("bytes = %d", got)
	}
	if _, err := conn.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}

	dial(t, r.f)
	dial(t, r.f)
	helped := r.host.named(eventHelped)
	if len(helped) != 1 || helped[0].ms != 1234 {
		t.Fatalf("fallback_helped = %+v", helped)
	}

	r.clock.advance(time.Second)
	r.f.disable("relay_back")
	off := r.host.named(eventOff)
	if len(off) != 1 || off[0].step != "relay_back" || off[0].result != resultOK || off[0].mb != 7 || off[0].ms != 1000 {
		t.Fatalf("fallback_off = %+v", off)
	}

	if err := r.f.enable(allowAll{}, r.host); err != nil {
		t.Fatal(err)
	}
	dial(t, r.f)
	if got := r.host.named(eventHelped); len(got) != 2 {
		t.Fatalf("fallback_helped after a new Enable = %+v", got)
	}
	r.f.disable("user")
	off = r.host.named(eventOff)
	if len(off) != 2 || off[1].mb != 0 {
		t.Fatalf("counters not reset: %+v", off)
	}
}

func TestHelpedSurvivesIdleInsideOneEnable(t *testing.T) {
	r := newRig(t)
	dial(t, r.f)
	r.clock.advance(r.f.t.idle)
	eventually(t, "idle stop", func() bool { return len(r.host.named(eventOff)) == 1 })
	dial(t, r.f)
	if got := r.host.named(eventHelped); len(got) != 1 {
		t.Fatalf("fallback_helped = %+v", got)
	}
	r.f.disable("relay_back")
	off := r.host.named(eventOff)
	if len(off) != 2 || off[1].result != resultOK {
		t.Fatalf("fallback_off = %+v", off)
	}
}

func TestDisableWithoutACallStillReports(t *testing.T) {
	r := newRig(t)
	r.f.disable("flag_off")
	off := r.host.named(eventOff)
	if len(off) != 1 || off[0].step != "flag_off" || off[0].result != resultFail || off[0].ms != 0 {
		t.Fatalf("fallback_off = %+v", off)
	}
	r.f.disable("flag_off")
	if got := r.host.named(eventOff); len(got) != 1 {
		t.Fatalf("second Disable reported again: %+v", got)
	}
}

func TestCloseWritePassesThrough(t *testing.T) {
	r := newRig(t)
	conn := dial(t, r.f)
	cw, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatal("no CloseWrite")
	}
	if _, err := conn.Write([]byte("bye")); err != nil {
		t.Fatal(err)
	}
	if err := cw.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(conn)
	if err != nil || string(got) != "bye" {
		t.Fatalf("read after half-close: %q, %v", got, err)
	}
}

func TestMegabytesRoundUp(t *testing.T) {
	for in, want := range map[int64]int64{0: 0, 1: 1, 1 << 20: 1, 1<<20 + 1: 2} {
		if got := megabytes(in); got != want {
			t.Errorf("megabytes(%d) = %d, want %d", in, got, want)
		}
	}
}
