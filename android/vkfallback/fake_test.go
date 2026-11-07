//go:build vkfake

package vkfallback

import (
	"context"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

type yesProtector struct{ calls chan int }

func (p yesProtector) Protect(fd int) bool {
	select {
	case p.calls <- fd:
	default:
	}
	return true
}

func TestFakeRefusesOtherRooms(t *testing.T) {
	key := strings.Repeat("ab", 32)
	e, ok := newEngine().(*socksEngine)
	if !ok {
		t.Fatal("the vkfake build must not use olcrtc")
	}
	for _, room := range []string{"https://vk.ru/call/join/x", "", "fake-socks://nohost", "fake-socks://:1"} {
		err := e.Configure(room, key, 0)
		if err == nil {
			t.Errorf("%q accepted", room)
		} else if room != "" && strings.Contains(err.Error(), room) {
			t.Errorf("error quotes the room: %v", err)
		}
	}
	if e.Configure("fake-socks://127.0.0.1:1", "short", 0) == nil {
		t.Error("bad key accepted")
	}
}

// Through fallback.Dial and socksDial, to a SOCKS5 server that wants the
// user/password method, as testenv/socks.py.
func TestFakeCarriesDialThroughUpstream(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go fakeSocksServer(ln)

	f := newFallback(&socksEngine{})
	if err := f.configure("fake-socks://"+ln.Addr().String(), strings.Repeat("ab", 32), 0); err != nil {
		t.Fatal(err)
	}
	if err := f.setDNS("10.0.2.3"); err != nil {
		t.Fatal(err)
	}
	p := yesProtector{calls: make(chan int, 1)}
	if err := f.enable(p, nil); err != nil {
		t.Fatal(err)
	}
	defer f.disable("user")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := f.Dial(ctx, "tcp", "1.2.3.4:443")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "ping" {
		t.Fatalf("echo %q", buf)
	}
	select {
	case <-p.calls:
	default:
		t.Error("the upstream socket was not protected")
	}
	f.disable("user")
	if _, err := conn.Read(buf); err == nil {
		t.Error("the connection outlived Stop")
	}
}

func fakeSocksServer(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			buf := make([]byte, 512)
			n, err := c.Read(buf)
			if err != nil || n == 0 {
				return // the readiness probe
			}
			_, _ = c.Write([]byte{5, 2})
			if _, err = c.Read(buf); err != nil {
				return
			}
			_, _ = c.Write([]byte{1, 0})
			if _, err = c.Read(buf); err != nil {
				return
			}
			_, _ = c.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0})
			_, _ = io.Copy(c, c)
		}()
	}
}
