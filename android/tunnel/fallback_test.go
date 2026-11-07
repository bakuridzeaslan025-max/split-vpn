package tunnel

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"splitvpn.local/shared"
)

// fakeFallback's exit is a local listener: remote is what olcrtc's exit would
// hold.
type fakeFallback struct {
	t   *testing.T
	ln  net.Listener
	err error

	mu    sync.Mutex
	dials []string
}

func (f *fakeFallback) Dial(_ context.Context, network, addr string) (net.Conn, error) {
	f.mu.Lock()
	f.dials = append(f.dials, network+" "+addr)
	f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	return net.Dial("tcp", f.ln.Addr().String())
}

func (f *fakeFallback) State() shared.State { return shared.State{Phase: shared.Working} }

func (f *fakeFallback) dialed() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dials...)
}

// remote returns the exit's side of the next accepted connection.
func (f *fakeFallback) remote() net.Conn {
	f.t.Helper()
	f.ln.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second))
	c, err := f.ln.Accept()
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { c.Close() })
	c.SetDeadline(time.Now().Add(2 * time.Second))
	return c
}

func withFallback(t *testing.T) *fakeFallback {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	f := &fakeFallback{t: t, ln: ln}
	shared.Register(f)
	t.Cleanup(func() { shared.Register(nil) })
	return f
}

type healthSnapshot struct {
	gen                      uint64
	fails, refused, netFails int
	open, known, told        bool
	nextProbe                time.Time
	lastOK, alive            int64
}

func snapshotHealth() healthSnapshot {
	health.mu.Lock()
	defer health.mu.Unlock()
	return healthSnapshot{
		gen: health.gen, fails: health.fails, refused: health.refused, netFails: health.netFails,
		open: health.open, known: health.known, told: health.told,
		nextProbe: health.nextProbe, lastOK: health.lastOK.Load(), alive: relayAlive.Load(),
	}
}

func dialTest(t *testing.T) (net.Conn, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return dialRelay(ctx, testCred, net.IPv4(149, 154, 167, 99), 443)
}

func TestFallback_DialsThroughOpenBreaker(t *testing.T) {
	_, got := fakeRelay(t)
	withConns(t)
	now := time.Now()
	health.now = func() time.Time { return now }
	openBreaker(&health, &now)
	withQuota(t, 100, 0)
	f := withFallback(t)
	before := snapshotHealth()
	if !before.open {
		t.Fatal("breaker not open")
	}

	c, err := dialTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	remote := f.remote()

	if d := f.dialed(); len(d) != 1 || d[0] != "tcp 149.154.167.99:443" {
		t.Fatalf("dials %q", d)
	}
	connsMu.Lock()
	_, tracked := activeConns[c]
	connsMu.Unlock()
	if !tracked {
		t.Fatal("not tracked")
	}

	c.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	// The exit gets the app's bytes from the first one: no header, no upgrade.
	buf := make([]byte, 4)
	if _, err := io.ReadFull(remote, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("exit got %q, %v", buf, err)
	}
	if _, err := remote.Write([]byte("pong!")); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(c, make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	if u := Usage(); u != 100+4+5 {
		t.Fatalf("usage %d, want 109", u)
	}

	if after := snapshotHealth(); after != before {
		t.Fatalf("health changed:\nbefore %+v\nafter  %+v", before, after)
	}
	select {
	case <-got:
		t.Fatal("the dial reached the relay")
	case <-time.After(100 * time.Millisecond):
	}

	c.Close()
	connsMu.Lock()
	n := len(activeConns)
	connsMu.Unlock()
	if n != 0 {
		t.Fatalf("not untracked on Close: %d", n)
	}
}

// A failed fallback dial is the answer: the relay is not tried behind it, and
// no failure lands on the relay's record.
func TestFallback_FailureStaysOffTheRelay(t *testing.T) {
	_, got := fakeRelay(t)
	withConns(t)
	f := withFallback(t)
	f.err = errors.New("no call")
	before := snapshotHealth()

	if _, err := dialTest(t); !errors.Is(err, f.err) {
		t.Fatalf("err = %v", err)
	}
	if after := snapshotHealth(); after != before {
		t.Fatalf("health changed:\nbefore %+v\nafter  %+v", before, after)
	}
	select {
	case <-got:
		t.Fatal("the dial reached the relay")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestFallback_FailuresLogOncePerKindUntilSuccess(t *testing.T) {
	withConns(t)
	f := withFallback(t)
	if c, err := dialTest(t); err != nil { // clears what earlier tests logged
		t.Fatal(err)
	} else {
		c.Close()
	}
	out := captureLog(t)
	fail := func(text string, times int) {
		t.Helper()
		f.err = errors.New(text)
		for range times {
			if _, err := dialTest(t); !errors.Is(err, f.err) {
				t.Fatalf("err = %v", err)
			}
		}
	}
	lines := func() (n int) {
		t.Helper()
		for _, l := range out.lines {
			if strings.Contains(l, "149.154.167.99") {
				t.Fatalf("destination in the log: %q", l)
			}
			if strings.HasPrefix(l, "fallback: ") {
				n++
			}
		}
		return n
	}

	fail("vkfallback: start failed: captcha", 3)
	if n := lines(); n != 1 {
		t.Fatalf("%d lines after one kind, want 1: %q", n, out.lines)
	}
	fail("vkfallback: start failed: auth", 2)
	if n := lines(); n != 2 {
		t.Fatalf("%d lines after two kinds, want 2: %q", n, out.lines)
	}
	fail("read tcp 127.0.0.1:1000->127.0.0.1:2000: connection reset by peer", 1)
	fail("read tcp 127.0.0.1:1001->127.0.0.1:2000: connection reset by peer", 1)
	if n := lines(); n != 3 {
		t.Fatalf("%d lines, a new local port must not be a new kind: %q", n, out.lines)
	}

	f.err = nil
	c, err := dialTest(t)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	fail("vkfallback: start failed: captcha", 2)
	if n := lines(); n != 4 {
		t.Fatalf("%d lines, a success must let a kind log again: %q", n, out.lines)
	}
}

func TestFallback_OverQuotaDoesNotDial(t *testing.T) {
	withConns(t)
	withQuota(t, 10, 10)
	f := withFallback(t)
	if _, err := dialTest(t); !errors.Is(err, errQuota) {
		t.Fatalf("err = %v, want errQuota", err)
	}
	if d := f.dialed(); len(d) != 0 {
		t.Fatalf("dials %q", d)
	}
}

func TestFallback_ClearedGoesBackToRelay(t *testing.T) {
	_, got := fakeRelay(t)
	withConns(t)
	f := withFallback(t)
	shared.Register(nil)
	c, err := dialTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	<-got
	if d := f.dialed(); len(d) != 0 {
		t.Fatalf("dials %q", d)
	}
}

func TestFallback_HalfClosePassesThrough(t *testing.T) {
	withConns(t)
	f := withFallback(t)
	c, err := dialTest(t)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	remote := f.remote()

	if err := c.(*trackedConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	if _, err := remote.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("exit read %v, want EOF", err)
	}
	if _, err := remote.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := io.ReadFull(c, make([]byte, 1)); err != nil {
		t.Fatalf("the way back died with the half-close: %v", err)
	}
}

func TestFallback_StopClosesConn(t *testing.T) {
	freshHealth(t)
	ep := channel.New(64, 1500, "")
	defer ep.Close()
	ns, err := newStack(ep, testCred)
	if err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	setConns(make(map[net.Conn]struct{}))
	s = ns
	mu.Unlock()
	t.Cleanup(Stop)
	f := withFallback(t)

	c, err := dialTest(t)
	if err != nil {
		t.Fatal(err)
	}
	remote := f.remote()

	Stop()

	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read after Stop: %v, want closed", err)
	}
	if _, err := remote.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("exit read after Stop: %v, want EOF", err)
	}
}

func TestFallback_NetworkLostClosesConn(t *testing.T) {
	freshHealth(t)
	withConns(t)
	f := withFallback(t)
	c, err := dialTest(t)
	if err != nil {
		t.Fatal(err)
	}
	remote := f.remote()

	NetworkLost()

	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read after NetworkLost: %v, want closed", err)
	}
	if _, err := remote.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("exit read after NetworkLost: %v, want EOF", err)
	}
}

// A dial that outlived Stop: the connection it brought must not stay open.
func TestFallback_DialAfterStopCloses(t *testing.T) {
	setConns(nil)
	f := withFallback(t)
	if _, err := dialTest(t); err == nil {
		t.Fatal("dial must fail after Stop")
	}
	remote := f.remote()
	if _, err := remote.Read(make([]byte, 1)); err != io.EOF {
		t.Fatalf("exit read: %v, want EOF", err)
	}
}
