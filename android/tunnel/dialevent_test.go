package tunnel

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"syscall"
	"testing"
	"time"
)

// dialHost hears DialEvent on a channel.
type dialHost struct {
	fakeProtector
	ch chan string
}

func (h *dialHost) DialEvent(id, result string, ms int64) {
	if ms < 0 {
		result += " negative ms"
	}
	h.ch <- id + " " + result
}

func withDialEvents(t *testing.T, bp int) *dialHost {
	t.Helper()
	h := &dialHost{ch: make(chan string, 1024)}
	protector = h
	SetDialEvents(bp)
	t.Cleanup(func() { protector = nil; SetDialEvents(0) })
	return h
}

func (h *dialHost) next(t *testing.T) string {
	t.Helper()
	select {
	case e := <-h.ch:
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("no DialEvent")
		return ""
	}
}

// scriptedRelay: a TLS listener that reads each upgrade request and
// answers it with answer (nil: never).
func scriptedRelay(t *testing.T, id string, answer func(c net.Conn)) {
	t.Helper()
	ln := fakeTLSListener(t)
	t.Cleanup(func() { ln.Close() })
	old := relayEP.Swap(&relayTarget{ln.Addr().String(), "relay.test", "/app/x", id})
	t.Cleanup(func() { relayEP.Store(old) })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
					c.Close()
					return
				}
				if answer == nil {
					t.Cleanup(func() { c.Close() })
					return
				}
				answer(c)
			}()
		}
	}()
}

func reply(s string) func(net.Conn) {
	return func(c net.Conn) {
		io.WriteString(c, s)
		c.Close()
	}
}

const answer101 = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n"

func shortHandshake(t *testing.T) {
	old := handshakeTimeout
	handshakeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { handshakeTimeout = old })
}

// Through the real handshake, so httpRoundTrip's %w stays pinned.
func TestDialEvent_UpgradeResults(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer func(net.Conn)
		want   string
	}{
		{"101", reply(answer101), "ok"},
		{"nginx without relay", reply("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"), "upgrade_http"},
		{"closed before an answer", reply(""), "upgrade_reset"},
		{"closed mid-headers", reply("HTTP/1.1 101 Switching"), "upgrade_reset"},
		{"silence", nil, "upgrade_timeout"},
	} {
		t.Run(c.name, func(t *testing.T) {
			shortHandshake(t)
			scriptedRelay(t, "nl1", c.answer)
			h := withDialEvents(t, 10000)
			probeRelay(testCred)
			if got := h.next(t); got != "nl1 "+c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestDialEvent_ConnectAndHello(t *testing.T) {
	shortHandshake(t)
	freshHealth(t)
	h := withDialEvents(t, 10000)
	try := func(addr string, want string) {
		t.Helper()
		old := relayEP.Swap(&relayTarget{addr, "relay.test", "/app/x", "nl1"})
		defer relayEP.Store(old)
		probeRelay(testCred)
		if got := h.next(t); got != "nl1 "+want {
			t.Fatalf("%s: got %q, want %q", addr, got, want)
		}
	}

	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	closed := ln.Addr().String()
	ln.Close()
	try(closed, "connect_refused")

	silent, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { silent.Close() })
	go func() {
		for {
			c, err := silent.Accept()
			if err != nil {
				return
			}
			t.Cleanup(func() { c.Close() })
		}
	}()
	try(silent.Addr().String(), "hello_timeout")

	hangup, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { hangup.Close() })
	go func() {
		for {
			c, err := hangup.Accept()
			if err != nil {
				return
			}
			c.Read(make([]byte, 1))
			c.Close()
		}
	}()
	try(hangup.Addr().String(), "hello_reset")

	// Chrome offers h2: a server that takes it is a hello we cannot use.
	fakeTLSNextProtos = []string{"h2"}
	h2 := fakeTLSListener(t)
	fakeTLSNextProtos = nil
	go func() {
		for {
			c, err := h2.Accept()
			if err != nil {
				return
			}
			go func() { c.(*tls.Conn).Handshake(); c.Read(make([]byte, 1)); c.Close() }()
		}
	}()
	try(h2.Addr().String(), "hello_other")
}

func TestDialResult(t *testing.T) {
	sys := func(op string, e syscall.Errno) error {
		return &net.OpError{Op: op, Net: "tcp", Err: os.NewSyscallError(op, e)}
	}
	for _, c := range []struct {
		stage string
		err   error
		want  string
	}{
		{"upgrade", nil, "ok"},
		{"connect", errTimeout, "connect_timeout"},
		{"connect", context.DeadlineExceeded, "connect_timeout"},
		{"hello", context.DeadlineExceeded, "hello_timeout"},
		{"connect", sys("connect", syscall.ECONNREFUSED), "connect_refused"},
		{"connect", sys("connect", syscall.ENETUNREACH), "connect_unreach"},
		{"connect", sys("connect", syscall.EHOSTUNREACH), "connect_unreach"},
		{"connect", sys("connect", syscall.ECONNRESET), "connect_other"},
		{"hello", sys("read", syscall.ECONNRESET), "hello_reset"},
		{"hello", io.EOF, "hello_reset"},
		{"hello", errors.New("x509: certificate signed by unknown authority"), "hello_other"},
		{"upgrade", sys("write", syscall.ECONNRESET), "upgrade_reset"},
		{"upgrade", httpStatus(504), "upgrade_http"},
		{"upgrade", errors.New("odd"), "upgrade_other"},
		{"connect", errors.New("odd"), "connect_other"},
	} {
		if got := dialResult(c.stage, c.err); got != c.want {
			t.Errorf("dialResult(%s, %v) = %q, want %q", c.stage, c.err, got, c.want)
		}
	}
}

// Nothing is heard of a handshake that says nothing about the relay's
// reach, or that is not sampled. Each case is followed by one that is
// heard: the first event to arrive must be that one.
func TestDialEvent_NotHeard(t *testing.T) {
	scriptedRelay(t, "nl1", reply(answer101))
	h := withDialEvents(t, 10000)
	heard := func(what string) {
		t.Helper()
		probeRelay(testCred)
		if got := h.next(t); got != "nl1 ok" {
			t.Fatalf("after %s: got %q", what, got)
		}
	}

	SetDialEvents(0)
	probeRelay(testCred)
	SetDialEvents(10001)
	probeRelay(testCred)
	SetDialEvents(10000)
	heard("bp 0 and out of range")

	ep := *relayEP.Load()
	ep.id = ""
	relayHandshake(context.Background(), health.generation(), &ep, testCred)
	heard("no id")

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	relayHandshake(ctx, health.generation(), relayEP.Load(), testCred)
	heard("cancelled")

	relayHandshake(context.Background(), notCounted, relayEP.Load(), testCred)
	heard("not counted")

	health.newNetwork()
	stale := health.generation() - 1
	relayHandshake(context.Background(), stale, relayEP.Load(), testCred)
	heard("a network change meanwhile")

	health.mu.Lock()
	health.open, health.nextProbe = true, time.Now().Add(time.Hour)
	health.mu.Unlock()
	if _, err := dialRelay(context.Background(), testCred, net.IPv4(1, 1, 1, 1), 443); !errors.Is(err, errRelayDown) {
		t.Fatalf("breaker: %v", err)
	}
	health.mu.Lock()
	health.open = false
	health.mu.Unlock()
	heard("the breaker")

	SetQuota(10, 10)
	if _, err := dialRelay(context.Background(), testCred, net.IPv4(1, 1, 1, 1), 443); !errors.Is(err, errQuota) {
		t.Fatalf("quota: %v", err)
	}
	SetQuota(0, 0)
	heard("the quota")
}

func TestDialEvent_CredRejectedAndRegisterNotHeard(t *testing.T) {
	scriptedRelay(t, "nl1", reply("HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n"))
	h := withDialEvents(t, 10000)
	if err := probeRelay(testCred); !errors.Is(err, ErrCredRejected) {
		t.Fatalf("err = %v", err)
	}
	ep := relayEP.Load()
	if _, err := Register(ep.addr, ep.sni, ep.path, KindInvite, []byte("code")); !errors.Is(err, ErrRejected) {
		t.Fatalf("register: %v", err)
	}
	select {
	case e := <-h.ch:
		t.Fatalf("heard %q", e)
	case <-time.After(300 * time.Millisecond):
	}
}

// One in a hundred, roughly: Go decides, the host never hears the rest.
func TestDialEvent_Sampled(t *testing.T) {
	scriptedRelay(t, "nl1", reply(answer101))
	h := withDialEvents(t, 5000)
	const n = 200
	for i := 0; i < n; i++ {
		probeRelay(testCred)
	}
	time.Sleep(200 * time.Millisecond)
	if got := len(h.ch); got < n/4 || got > 3*n/4 {
		t.Fatalf("%d of %d heard at 50%%", got, n)
	}
}

func TestSetEndpointID_RelabelsOnly(t *testing.T) {
	freshHealth(t)
	old := relayEP.Swap(&relayTarget{"127.0.0.1:1", "relay.test", "/app/x", ""})
	t.Cleanup(func() { relayEP.Store(old) })
	gen := health.generation()
	SetEndpointID("nl1")
	if ep := relayEP.Load(); ep.id != "nl1" || ep.addr != "127.0.0.1:1" || ep.path != "/app/x" {
		t.Fatalf("%+v", *ep)
	}
	if health.generation() != gen {
		t.Fatal("a new label reset the relay's health")
	}
}
