package tunnel

import (
	"context"
	"crypto/tls"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// clientHello captures the ClientHello Go's tls client writes for name.
func clientHello(t *testing.T, name string) []byte {
	t.Helper()
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		c := tls.Client(a, &tls.Config{ServerName: name, InsecureSkipVerify: true})
		c.Handshake()
	}()
	b.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 4096)
	n, err := b.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	return buf[:n]
}

func TestParseSNI(t *testing.T) {
	for _, name := range []string{"x.com", "web.telegram.org", "rr1---sn-5hnednsz.googlevideo.com"} {
		if got := parseSNI(clientHello(t, name)); got != name {
			t.Fatalf("got %q want %q", got, name)
		}
	}
	hello := clientHello(t, "x.com")
	for _, b := range [][]byte{nil, []byte("GET / HTTP/1.1\r\n"), hello[:20], hello[:len(hello)/2], {0x16, 0x03, 0x01, 0x00, 0x05, 0x02, 0, 0, 1, 0}} {
		if got := parseSNI(b); got != "" && got != "x.com" {
			t.Fatalf("garbage %x parsed as %q", b, got)
		}
	}
	if parseSNI(clientHello(t, "")) != "" {
		t.Fatal("hello without SNI yielded a name")
	}
}

func TestPeekClientHello_JoinsSplitRecordAndGivesUpQuietly(t *testing.T) {
	hello := clientHello(t, "x.com")
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		a.Write(hello[:7])
		time.Sleep(20 * time.Millisecond)
		a.Write(hello[7:])
	}()
	got := peekClientHello(b)
	if string(got) != string(hello) {
		t.Fatalf("peeked %d of %d bytes", len(got), len(hello))
	}

	// Server-speaks-first protocol: nothing arrives, peek returns empty after peekWait.
	c, d := net.Pipe()
	defer c.Close()
	defer d.Close()
	start := time.Now()
	if got := peekClientHello(d); len(got) != 0 {
		t.Fatalf("peeked %d bytes from a silent client", len(got))
	}
	if time.Since(start) > peekWait*5 {
		t.Fatal("peek waited too long")
	}
}

func TestRelayByName(t *testing.T) {
	setDomains("Telegram.org\n.x.com\n\ngooglevideo.com\ngoogle.com\n")
	t.Cleanup(func() { setDomains("") })
	for name, want := range map[string]bool{
		"x.com": true, "api.x.com": true, "X.COM.": true, "notx.com": false, "x.com.evil.io": false,
		"web.telegram.org": true, "rr1---sn-5hnednsz.googlevideo.com": true, "youtube.com": false,
		"mtalk.google.com": false, "alt3-mtalk.google.com": false, "www.google.com": true,
	} {
		if got := relayByName(name); got != want {
			t.Fatalf("%s: got %v want %v", name, got, want)
		}
	}
}

// The app fetches its relay list from Remote Config when the relay is down:
// those names go direct though googleapis.com is listed, their neighbours not.
func TestRelayByName_RemoteConfigDirect(t *testing.T) {
	setDomains("googleapis.com")
	t.Cleanup(func() { setDomains("") })
	for name, want := range map[string]bool{
		"firebaseremoteconfig.googleapis.com": false, "FirebaseInstallations.googleapis.com.": false,
		"www.googleapis.com": true, "x.firebaseremoteconfig.googleapis.com": true,
	} {
		if got := relayByName(name); got != want {
			t.Fatalf("%s: got %v want %v", name, got, want)
		}
	}
}

type fakeProtector struct {
	mu    sync.Mutex
	fds   []int
	ok    bool
	stale atomic.Int32
	dns   string
	down  []bool
	quota atomic.Int32
	steps atomic.Int32
	told  []string
}

func (f *fakeProtector) Protect(fd int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fds = append(f.fds, fd)
	return f.ok
}
func (f *fakeProtector) RoutesStale() { f.stale.Add(1) }
func (f *fakeProtector) DnsServers() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dns
}
func (f *fakeProtector) RelayDown(down bool) {
	f.mu.Lock()
	f.down = append(f.down, down)
	f.mu.Unlock()
}
func (f *fakeProtector) QuotaExceeded() { f.quota.Add(1) }
func (f *fakeProtector) QuotaProgress() { f.steps.Add(1) }
func (f *fakeProtector) DirectVerdict(v string) {
	f.mu.Lock()
	f.told = append(f.told, v)
	f.mu.Unlock()
}

func (f *fakeProtector) verdicts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.told...)
}

func (f *fakeProtector) protected() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int(nil), f.fds...)
}

func (f *fakeProtector) relayDowns() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]bool(nil), f.down...)
}

// sockopt reads an int socket option of c, which must still be open.
func sockopt(t *testing.T, c net.Conn, level, name int) int {
	t.Helper()
	rc, err := c.(*net.TCPConn).SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var v int
	var serr error
	if err := rc.Control(func(fd uintptr) { v, serr = syscall.GetsockoptInt(int(fd), level, name) }); err != nil || serr != nil {
		t.Fatal(err, serr)
	}
	return v
}

func TestDialDirect_ProtectsSocket(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
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
			c.Close()
		}
	}()
	fp := &fakeProtector{ok: true}
	protector = fp
	t.Cleanup(func() { protector = nil })
	c, err := dialDirect(context.Background(), "tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if v := sockopt(t, c, syscall.SOL_SOCKET, syscall.SO_KEEPALIVE); v != 0 {
		t.Fatalf("SO_KEEPALIVE %d on a direct socket", v)
	}
	c.Close()
	if fds := fp.protected(); len(fds) != 1 || fds[0] <= 0 {
		t.Fatalf("protect calls %v", fds)
	}
	fp.ok = false
	if _, err := dialDirect(context.Background(), "tcp", ln.Addr().String()); err == nil {
		t.Fatal("dial succeeded although protect failed")
	}
}
