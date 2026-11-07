//go:build vkfake

package vkfallback

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const fakeScheme = "fake-socks://"

var fakeKey = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// Any other room is refused, not handed to olcrtc: a build with this tag can
// never reach VK, whatever room a test or a leftover pref gives it.
func init() {
	newEngine = func() engine { return &socksEngine{} }
}

// socksEngine puts the SOCKS5 server named by the room where olcrtc's local port
// would be, so that fallback's socksDial and everything above it run unchanged.
type socksEngine struct {
	mu        sync.Mutex
	upstream  string
	protector Protector
	opened    func()
	ln        net.Listener
	conns     map[net.Conn]struct{}
}

// No error here quotes the room: Kotlin logs them.
func (e *socksEngine) Configure(room, keyHex string, _ int) error {
	addr, ok := strings.CutPrefix(room, fakeScheme)
	if !ok {
		return errors.New("vkfake: not a " + fakeScheme + " room")
	}
	if host, port, err := net.SplitHostPort(addr); err != nil || host == "" || port == "" {
		return errors.New("vkfake: bad address in the room")
	}
	if !fakeKey.MatchString(keyHex) {
		return errors.New("vkfake: bad key")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.upstream = addr
	return nil
}

func (e *socksEngine) SetDNS(string) error { return nil }

func (e *socksEngine) SetProtector(p Protector) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.protector = p
}

func (e *socksEngine) SetSessionHandler(fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.opened = fn
}

func (e *socksEngine) Start(socksPort int, _, _ string) error {
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(socksPort)))
	if err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ln != nil {
		_ = ln.Close()
		return errors.New("vkfake: already running")
	}
	e.ln = ln
	e.conns = map[net.Conn]struct{}{}
	log.Print("vkfake: start, no VK involved")
	go e.accept(ln)
	return nil
}

func (e *socksEngine) WaitReady(timeoutMillis int) error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMillis)*time.Millisecond)
	defer cancel()
	conn, err := e.dial(ctx)
	if err != nil {
		return errors.New("vkfake: upstream unreachable")
	}
	_ = conn.Close()
	e.mu.Lock()
	opened := e.opened
	e.mu.Unlock()
	if opened != nil {
		opened()
	}
	return nil
}

func (e *socksEngine) IsRunning() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.ln != nil
}

func (e *socksEngine) Stop(int) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ln == nil {
		return nil
	}
	log.Print("vkfake: stop")
	_ = e.ln.Close()
	e.ln = nil
	for c := range e.conns {
		_ = c.Close()
	}
	e.conns = nil
	return nil
}

// dial goes out through the real Protector, as olcrtc's sockets do: a socket it
// refused would loop through the TUN.
func (e *socksEngine) dial(ctx context.Context) (net.Conn, error) {
	e.mu.Lock()
	addr, p := e.upstream, e.protector
	e.mu.Unlock()
	d := net.Dialer{Control: func(_, _ string, c syscall.RawConn) error {
		if p == nil {
			return errNoProtector
		}
		var ok bool
		if err := c.Control(func(fd uintptr) { ok = p.Protect(int(fd)) }); err != nil {
			return err
		}
		if !ok {
			return errors.New("vkfake: protect refused")
		}
		return nil
	}}
	return d.DialContext(ctx, "tcp4", addr)
}

func (e *socksEngine) accept(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go e.forward(c)
	}
}

func (e *socksEngine) track(c net.Conn) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.conns == nil {
		return false
	}
	e.conns[c] = struct{}{}
	return true
}

func (e *socksEngine) untrack(c net.Conn) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.conns, c)
	_ = c.Close()
}

func (e *socksEngine) forward(in net.Conn) {
	defer e.untrack(in)
	if !e.track(in) {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := e.dial(ctx)
	if err != nil {
		return
	}
	defer e.untrack(out)
	if !e.track(out) {
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		pipe(out, in)
	}()
	pipe(in, out)
	<-done
}

func pipe(dst, src net.Conn) {
	_, _ = io.Copy(dst, src)
	if hc, ok := dst.(interface{ CloseWrite() error }); ok {
		_ = hc.CloseWrite()
	}
}
