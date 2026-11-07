package vkfallback

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"regexp"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"splitvpn.local/shared"
)

const (
	stepAuth      = "auth"
	stepCaptcha   = "captcha"
	stepTransport = "transport"

	resultOK      = "ok"
	resultFail    = "fail"
	resultBackoff = "backoff"

	eventConnect = "fallback_connect"
	eventHelped  = "fallback_helped"
	eventOff     = "fallback_off"
	eventCaptcha = "fallback_captcha"

	offIdle  = "idle"
	offError = "error"

	reasonBudget = "join_budget"

	vkCaptchaCode = 14
)

var (
	errOff           = errors.New("vkfallback: disabled")
	errStopped       = errors.New("vkfallback: stopped")
	errNotConfigured = errors.New("vkfallback: not configured")
	errNoProtector   = errors.New("vkfallback: no protector")
	errNoDNS         = errors.New("vkfallback: no DNS servers yet")
	errNoNetwork     = errors.New("vkfallback: no network")
	errBackoff       = errors.New("vkfallback: pausing after a failed start")
	errBudget        = errors.New("vkfallback: join budget spent")
	errNetwork       = errors.New("vkfallback: only TCP")
)

// timing holds every interval, so tests can shrink them. Each start of a call
// is a new anonymous join, and VK answers too many of those with a captcha:
// most of these exist to keep joins rare.
type timing struct {
	connect     time.Duration // how long a start may take to get ready
	stop        time.Duration // how long leaving the call may take
	idle        time.Duration // silence before the call stops
	watch       time.Duration // how often a running call is checked
	debounce    time.Duration // network events closer than this count as one
	restartGap  time.Duration // the least time between a start and a restart for the network
	joinBudget  int           // joins allowed inside joinWindow, whatever became of them
	joinWindow  time.Duration
	retryBase   time.Duration
	retryMax    time.Duration
	captchaBase time.Duration
	captchaMax  time.Duration
}

var defaultTiming = timing{
	// Two handshake waits and the peer wait in olcrtc are 15 s each, on top of
	// the joins and ICE.
	connect:     90 * time.Second,
	stop:        5 * time.Second,
	idle:        5 * time.Minute,
	watch:       5 * time.Second,
	debounce:    3 * time.Second,
	restartGap:  30 * time.Second,
	joinBudget:  6,
	joinWindow:  time.Hour,
	retryBase:   15 * time.Second,
	retryMax:    5 * time.Minute,
	captchaBase: time.Minute,
	captchaMax:  30 * time.Minute,
}

// run is one start of the call, from the decision to start until it stops.
type run struct {
	startedAt time.Time
	readyAt   time.Time // under fallback.mu; zero until ready
	opened    bool      // under fallback.mu; the first session was seen

	// Set by startEngine before finish, read after settled.
	port       int
	user, pass string

	settled chan struct{} // ready, failed or stopped
	err     error         // written before settled closes
	once    sync.Once
	stopped chan struct{}
}

func (r *run) finish(err error) {
	r.once.Do(func() {
		r.err = err
		close(r.settled)
	})
}

type fallback struct {
	eng engine
	t   timing
	now func() time.Time

	// Serializes the engine's Start and Stop: olcrtc refuses a Start during a
	// Stop. Taken before mu, never while holding it.
	life sync.Mutex

	mu         sync.Mutex
	configured bool
	enabled    bool
	dnsSet     bool
	host       Host
	cur        *run

	enabledAt   time.Time
	helped      bool      // a Dial succeeded since Enable
	activeSince time.Time // first ready since the previous fallback_off

	lastStart   time.Time
	joins       []time.Time
	failures    int
	notBefore   time.Time
	captchaHold bool   // notBefore came from a captcha
	reason      string // why the last start or call failed

	networkDown bool
	netEventAt  time.Time
	netTimer    *time.Timer
	resume      bool // the call was stopped by NetworkLost

	bytes     atomic.Int64
	base      time.Time
	lastTouch atomic.Int64 // since base
}

func newFallback(eng engine) *fallback {
	f := &fallback{eng: eng, t: defaultTiming, now: time.Now}
	f.base = f.now()
	eng.SetSessionHandler(f.sessionOpened)
	return f
}

func (f *fallback) configure(room, keyHex string, fps int) error {
	if err := f.eng.Configure(room, keyHex, fps); err != nil {
		return err
	}
	f.mu.Lock()
	f.configured = true
	f.mu.Unlock()
	return nil
}

func (f *fallback) setDNS(servers string) error {
	if servers == "" {
		return nil
	}
	if err := f.eng.SetDNS(servers); err != nil {
		return err
	}
	f.mu.Lock()
	f.dnsSet = true
	f.mu.Unlock()
	return nil
}

func (f *fallback) enable(p Protector, h Host) error {
	if p == nil {
		return errNoProtector
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.configured {
		return errNotConfigured
	}
	if f.enabled {
		return nil
	}
	f.eng.SetProtector(p)
	f.enabled = true
	f.host = h
	f.enabledAt = f.now()
	f.helped = false
	shared.Register(f)
	return nil
}

func (f *fallback) disable(detail string) {
	// Before anything stops, so that later dials no longer reach the transport.
	shared.Register(nil)
	f.mu.Lock()
	if !f.enabled {
		f.mu.Unlock()
		return
	}
	f.enabled = false
	f.resume = false
	if f.netTimer != nil {
		f.netTimer.Stop()
	}
	if f.cur != nil {
		f.dropLocked(f.cur)
	}
	off := f.offEventLocked(detail)
	f.mu.Unlock()
	f.stopEngine()
	off()
}

func (f *fallback) State() shared.State {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case !f.enabled:
		return shared.State{Phase: shared.Off}
	case f.cur != nil && !f.cur.readyAt.IsZero():
		return shared.State{Phase: shared.Working}
	case f.cur != nil:
		return shared.State{Phase: shared.Connecting}
	case f.reason != "":
		return shared.State{Phase: shared.Error, Reason: f.reason}
	}
	return shared.State{Phase: shared.Off}
}

// Dial must fail once stopped by itself: a caller may have read shared.Active
// just before the fallback was cleared from it.
func (f *fallback) Dial(ctx context.Context, network, addr string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" {
		return nil, errNetwork
	}
	r, err := f.acquire()
	if err != nil {
		return nil, err
	}
	// The start belongs to the run, not to this Dial: tunnel's short dial
	// context must not throw away a join that the next dial can use.
	select {
	case <-r.settled:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	if r.err != nil {
		return nil, r.err
	}
	conn, err := socksDial(ctx, r.port, r.user, r.pass, addr)
	if err != nil {
		return nil, err
	}
	f.touch()
	if !f.dialed(r) {
		_ = conn.Close()
		return nil, errStopped
	}
	return &countedConn{Conn: conn, f: f}, nil
}

func (f *fallback) acquire() (*run, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case !f.enabled:
		return nil, errOff
	case f.networkDown:
		return nil, errNoNetwork
	}
	if f.cur != nil {
		return f.cur, nil
	}
	switch {
	case !f.dnsSet:
		return nil, errNoDNS
	case f.now().Before(f.notBefore):
		return nil, errBackoff
	case f.budgetSpentLocked():
		f.reason = reasonBudget
		return nil, errBudget
	}
	// Only here and on traffic: dials that keep failing must not hold a call
	// open, a dead exit would otherwise rejoin for as long as apps retry.
	f.touch()
	return f.startLocked(), nil
}

func (f *fallback) startLocked() *run {
	r := &run{startedAt: f.now(), settled: make(chan struct{}), stopped: make(chan struct{})}
	f.cur = r
	f.lastStart = r.startedAt
	f.joins = append(f.joins, r.startedAt)
	f.resume = false
	go f.connect(r)
	return r
}

// budgetSpentLocked bounds what the pauses cannot: a call that works and then
// dies, a network that keeps clearing them, rejoins inside olcrtc.
func (f *fallback) budgetSpentLocked() bool {
	cutoff := f.now().Add(-f.t.joinWindow)
	f.joins = slices.DeleteFunc(f.joins, func(t time.Time) bool { return !t.After(cutoff) })
	return len(f.joins) >= f.t.joinBudget
}

// sessionOpened hears a session open inside olcrtc, under its reconnect lock:
// the stop has to happen elsewhere. A generation's first session is the join
// its start already counted; any later one is taken for a rejoin, because
// olcrtc cannot be asked. With the budget reached the call stops here, since
// the next rejoin could not be refused.
func (f *fallback) sessionOpened() {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := f.cur
	if r != nil && !r.opened {
		r.opened = true
		return
	}
	f.joins = append(f.joins, f.now())
	if r == nil || !f.budgetSpentLocked() {
		return
	}
	up := !r.readyAt.IsZero()
	f.dropLocked(r)
	f.reason = reasonBudget
	off := func() {}
	if up {
		off = f.offEventLocked(offError)
	}
	go func() {
		f.stopEngine()
		off()
	}()
}

// dropLocked ends r for everyone waiting on it. The engine stops afterwards,
// outside mu, in stopEngine or at the next start.
func (f *fallback) dropLocked(r *run) {
	f.cur = nil
	close(r.stopped)
	r.finish(errStopped)
}

// dialed records a successful CONNECT unless r was stopped during it.
func (f *fallback) dialed(r *run) bool {
	f.mu.Lock()
	if f.cur != r {
		f.mu.Unlock()
		return false
	}
	first := !f.helped
	f.helped = true
	ms := f.now().Sub(f.enabledAt).Milliseconds()
	f.mu.Unlock()
	if first {
		f.emit(eventHelped, "", "", ms, 0)
	}
	return true
}

func (f *fallback) connect(r *run) {
	err := f.startEngine(r)
	if err == nil {
		err = f.eng.WaitReady(int(f.t.connect.Milliseconds()))
	}
	f.mu.Lock()
	if f.cur != r {
		f.mu.Unlock()
		return
	}
	ms := f.now().Sub(r.startedAt).Milliseconds()
	if err != nil {
		step := stepOf(err)
		f.failedLocked(step)
		// Only the step: tunnel logs this, and olcrtc's text may name the room.
		r.finish(errors.New("vkfallback: start failed: " + step))
		f.dropLocked(r)
		f.mu.Unlock()
		f.emit(eventConnect, resultFail, step, ms, 0)
		if step == stepCaptcha {
			f.emit(eventCaptcha, resultBackoff, "", 0, 0)
		}
		f.stopEngine()
		return
	}
	r.readyAt = f.now()
	if f.activeSince.IsZero() {
		f.activeSince = r.readyAt
	}
	f.reason = ""
	f.mu.Unlock()
	f.emit(eventConnect, resultOK, "", ms, 0)
	r.finish(nil)
	f.watch(r)
}

// startEngine stops whatever generation is left first. A stop that was asked
// for at the same moment sees f.cur set and leaves this to us.
func (f *fallback) startEngine(r *run) error {
	f.life.Lock()
	defer f.life.Unlock()
	f.mu.Lock()
	current := f.cur == r
	f.mu.Unlock()
	if !current {
		return errStopped
	}
	_ = f.eng.Stop(int(f.t.stop.Milliseconds()))
	port, err := freePort()
	if err != nil {
		return err
	}
	if r.user, err = randomHex(); err != nil {
		return err
	}
	if r.pass, err = randomHex(); err != nil {
		return err
	}
	r.port = port
	return f.eng.Start(r.port, r.user, r.pass)
}

// stopEngine leaves the call. With a newer run in place it does nothing: that
// run's startEngine stops the old generation, and stopping here could hit the
// new one.
func (f *fallback) stopEngine() {
	f.life.Lock()
	defer f.life.Unlock()
	f.mu.Lock()
	busy := f.cur != nil
	f.mu.Unlock()
	if !busy {
		_ = f.eng.Stop(int(f.t.stop.Milliseconds()))
	}
}

// watch polls because olcrtc's mobile API has no callback for a generation
// that ended. Next to a call sending 30 video frames a second the poll costs
// nothing.
func (f *fallback) watch(r *run) {
	tick := time.NewTicker(f.t.watch)
	defer tick.Stop()
	for {
		select {
		case <-r.stopped:
			return
		case <-tick.C:
		}
		switch {
		case !f.eng.IsRunning():
			f.end(r, offError)
			return
		case f.isIdle():
			f.end(r, offIdle)
			return
		}
	}
}

func (f *fallback) end(r *run, detail string) {
	f.mu.Lock()
	if f.cur != r {
		f.mu.Unlock()
		return
	}
	if detail == offError {
		f.failedLocked(stepTransport)
	} else {
		f.failures = 0
	}
	f.dropLocked(r)
	off := f.offEventLocked(detail)
	f.mu.Unlock()
	f.stopEngine()
	off()
}

// failedLocked pauses the next start. Nothing retries by itself: the next
// attempt is the next Dial after the pause.
func (f *fallback) failedLocked(step string) {
	base, limit := f.t.retryBase, f.t.retryMax
	if step == stepCaptcha {
		base, limit = f.t.captchaBase, f.t.captchaMax
	}
	f.failures++
	wait := limit
	if f.failures <= 16 {
		wait = min(base<<(f.failures-1), limit)
	}
	f.notBefore = f.now().Add(wait)
	f.captchaHold = step == stepCaptcha
	f.reason = step
}

// offEventLocked resets the counters and returns the event, to send outside mu.
func (f *fallback) offEventLocked(detail string) func() {
	result := resultFail
	if f.helped {
		result = resultOK
	}
	var ms int64
	if !f.activeSince.IsZero() {
		ms = f.now().Sub(f.activeSince).Milliseconds()
	}
	f.activeSince = time.Time{}
	mb := megabytes(f.bytes.Swap(0))
	return func() { f.emit(eventOff, result, detail, ms, mb) }
}

func (f *fallback) networkEvent(up bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.networkDown = !up
	f.netEventAt = f.now()
	// A captcha stays: it answers the joins made so far, and a network that
	// flaps must not turn the pause into a retry loop. A transport failure may
	// have been the old network's.
	if up && !f.captchaHold {
		f.failures = 0
		f.notBefore = time.Time{}
	}
	if !f.enabled || (f.cur == nil && !f.resume) {
		return
	}
	if f.netTimer != nil {
		f.netTimer.Stop()
	}
	f.netTimer = time.AfterFunc(f.t.debounce, f.networkSettled)
}

func (f *fallback) networkSettled() {
	f.mu.Lock()
	r := f.cur
	switch {
	case !f.enabled, r == nil && !f.resume, r != nil && r.startedAt.After(f.netEventAt):
		f.mu.Unlock()
		return
	case f.networkDown:
		if r != nil {
			f.dropLocked(r)
			f.resume = true
		}
		f.mu.Unlock()
		f.stopEngine()
		return
	}
	if wait := f.lastStart.Add(f.t.restartGap).Sub(f.now()); wait > 0 {
		f.netTimer = time.AfterFunc(wait, f.networkSettled)
		f.mu.Unlock()
		return
	}
	up := r != nil && !r.readyAt.IsZero()
	if r != nil {
		f.dropLocked(r)
	}
	f.resume = false
	off := func() {}
	// A start here is the same join a Dial would make, so it obeys the same
	// limits; and with nothing to carry, the next Dial can make it.
	switch {
	case f.now().Before(f.notBefore), f.isIdle():
	case f.budgetSpentLocked():
		f.reason = reasonBudget
		if up {
			off = f.offEventLocked(offError)
		}
	default:
		f.startLocked()
	}
	f.mu.Unlock()
	f.stopEngine()
	off()
}

func (f *fallback) touch() {
	f.lastTouch.Store(int64(f.now().Sub(f.base)))
}

func (f *fallback) isIdle() bool {
	return f.now().Sub(f.base)-time.Duration(f.lastTouch.Load()) >= f.t.idle
}

func (f *fallback) emit(name, result, step string, ms, mb int64) {
	f.mu.Lock()
	h := f.host
	f.mu.Unlock()
	if h != nil {
		h.Event(name, result, step, ms, mb)
	}
}

// vkAPIError matches the text of olcrtc's GuestAPIError.Error, which at the
// pinned commit reads "vkcalls: <stage>: api error <code> (<kind>)"
// (internal/auth/vkcalls/vkcalls.go). errors.As cannot reach the type, it lives
// under olcrtc's internal/. Recheck on every fork bump: if the text changes,
// captcha silently turns into transport.
var vkAPIError = regexp.MustCompile(`api error (\d+) \(`)

// stepOf names why a start failed. The other steps of the plan (signaling, ice,
// handshake) have no type or text worth trusting yet, so they stay transport.
func stepOf(err error) string {
	m := vkAPIError.FindStringSubmatch(err.Error())
	if m == nil {
		return stepTransport
	}
	if code, _ := strconv.Atoi(m[1]); code == vkCaptchaCode {
		return stepCaptcha
	}
	return stepAuth
}

// megabytes rounds up, as relay_day does: any traffic counts as at least 1.
func megabytes(n int64) int64 {
	return (n + 1<<20 - 1) >> 20
}

// freePort has a tiny race with whoever binds next, which only costs that
// start. olcrtc refuses port 0 and does not report the port it bound.
func freePort() (int, error) {
	l, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// randomHex guards the local SOCKS port: any app on the phone can reach loopback.
func randomHex() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

type countedConn struct {
	net.Conn
	f *fallback
}

func (c *countedConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.count(n)
	return n, err
}

func (c *countedConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.count(n)
	return n, err
}

func (c *countedConn) count(n int) {
	if n > 0 {
		c.f.bytes.Add(int64(n))
		c.f.touch()
	}
}

// CloseWrite passes half-close on: tunnel looks for it on the connection.
func (c *countedConn) CloseWrite() error {
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return c.Conn.Close()
}
