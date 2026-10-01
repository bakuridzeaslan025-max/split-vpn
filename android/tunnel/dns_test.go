package tunnel

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func mustQuery(t *testing.T, id uint16, name string, typ dnsmessage.Type) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, RecursionDesired: true})
	b.StartQuestions()
	if err := b.Question(dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: typ, Class: dnsmessage.ClassINET}); err != nil {
		t.Fatal(err)
	}
	q, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func mustAnswer(t *testing.T, id uint16, name string, ttl uint32, ip [4]byte) []byte {
	t.Helper()
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: id, Response: true})
	b.StartQuestions()
	n := dnsmessage.MustNewName(name)
	b.Question(dnsmessage.Question{Name: n, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET})
	b.StartAnswers()
	if err := b.AResource(dnsmessage.ResourceHeader{Name: n, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: ttl}, dnsmessage.AResource{A: ip}); err != nil {
		t.Fatal(err)
	}
	r, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func fakeDoH(t *testing.T, calls *atomic.Int32, respond func(q []byte) ([]byte, error)) *http.Client {
	t.Helper()
	return &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/dns-message" || r.Header.Get("Idempotency-Key") == "" {
			t.Errorf("bad DoH request: %s %v", r.Method, r.Header)
		}
		q, _ := io.ReadAll(r.Body)
		resp, err := respond(q)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(resp)), Header: http.Header{}}, nil
	})}
}

func resetDNS() {
	dnsCache.Range(func(k, _ any) bool { dnsCache.Delete(k); return true })
	doh = nil
}

func rcode(msg []byte) dnsmessage.RCode { return dnsmessage.RCode(msg[3] & 0x0F) }

func TestQuestionKey(t *testing.T) {
	q := mustQuery(t, 1, "example.com.", dnsmessage.TypeA)
	if got := questionKey(q); got != "example.com./TypeA" {
		t.Fatalf("questionKey = %q", got)
	}
	if got := questionKey([]byte{1, 2, 3}); got != "" {
		t.Fatalf("garbage key = %q", got)
	}
}

func TestResponseTTL(t *testing.T) {
	cases := []struct {
		ttl  uint32
		want time.Duration
	}{
		{5, dnsMinTTL},
		{60, 60 * time.Second},
		{86400, dnsMaxTTL},
	}
	for _, c := range cases {
		r := mustAnswer(t, 1, "a.test.", c.ttl, [4]byte{1, 2, 3, 4})
		if got := responseTTL(r); got != c.want {
			t.Errorf("ttl %d: got %v want %v", c.ttl, got, c.want)
		}
	}
	if got := responseTTL(mustQuery(t, 1, "a.test.", dnsmessage.TypeA)); got != dnsMinTTL {
		t.Errorf("no answers: got %v", got)
	}
}

func TestServfail(t *testing.T) {
	q := mustQuery(t, 0xBEEF, "x.test.", dnsmessage.TypeA)
	r := servfail(q)
	if binary.BigEndian.Uint16(r[:2]) != 0xBEEF || r[2]&0x80 == 0 || rcode(r) != dnsmessage.RCodeServerFailure {
		t.Fatalf("servfail = % x", r[:4])
	}
	if servfail([]byte{1}) != nil {
		t.Fatal("short query should give nil")
	}
}

func TestResolveDNS_CacheAndID(t *testing.T) {
	resetDNS()
	var calls atomic.Int32
	doh = fakeDoH(t, &calls, func(q []byte) ([]byte, error) {
		return mustAnswer(t, binary.BigEndian.Uint16(q[:2]), "cached.test.", 300, [4]byte{9, 9, 9, 9}), nil
	})
	r1 := resolveDNS(mustQuery(t, 0x1111, "cached.test.", dnsmessage.TypeA))
	r2 := resolveDNS(mustQuery(t, 0x2222, "cached.test.", dnsmessage.TypeA))
	if calls.Load() != 1 {
		t.Fatalf("DoH called %d times, want 1", calls.Load())
	}
	if binary.BigEndian.Uint16(r1[:2]) != 0x1111 || binary.BigEndian.Uint16(r2[:2]) != 0x2222 {
		t.Fatalf("IDs not patched: %x %x", r1[:2], r2[:2])
	}
	if !bytes.Equal(r1[2:], r2[2:]) {
		t.Fatal("cached body differs")
	}
	resolveDNS(mustQuery(t, 3, "cached.test.", dnsmessage.TypeAAAA))
	if calls.Load() != 2 {
		t.Fatal("different qtype must miss the cache")
	}
}

func TestResolveDNS_Failure(t *testing.T) {
	resetDNS()
	var calls atomic.Int32
	doh = fakeDoH(t, &calls, func([]byte) ([]byte, error) { return nil, errors.New("boom") })
	r := resolveDNS(mustQuery(t, 7, "fail.test.", dnsmessage.TypeA))
	if rcode(r) != dnsmessage.RCodeServerFailure {
		t.Fatalf("rcode = %v, want SERVFAIL", rcode(r))
	}
	if _, ok := dnsCache.Load("fail.test./TypeA"); ok {
		t.Fatal("failures must not be cached")
	}
	doh = nil
	if rcode(resolveDNS(mustQuery(t, 8, "stopped.test.", dnsmessage.TypeA))) != dnsmessage.RCodeServerFailure {
		t.Fatal("nil client must SERVFAIL")
	}
}

func TestServeDNSTCP(t *testing.T) {
	resetDNS()
	var calls atomic.Int32
	doh = fakeDoH(t, &calls, func(q []byte) ([]byte, error) {
		return mustAnswer(t, binary.BigEndian.Uint16(q[:2]), "tcp.test.", 60, [4]byte{1, 1, 1, 1}), nil
	})
	client, server := net.Pipe()
	go serveDNSTCP(server)
	defer client.Close()

	q := mustQuery(t, 42, "tcp.test.", dnsmessage.TypeA)
	msg := make([]byte, 2+len(q))
	binary.BigEndian.PutUint16(msg, uint16(len(q)))
	copy(msg[2:], q)
	client.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err := client.Write(msg); err != nil {
		t.Fatal(err)
	}
	var l [2]byte
	if _, err := io.ReadFull(client, l[:]); err != nil {
		t.Fatal(err)
	}
	resp := make([]byte, binary.BigEndian.Uint16(l[:]))
	if _, err := io.ReadFull(client, resp); err != nil {
		t.Fatal(err)
	}
	if binary.BigEndian.Uint16(resp[:2]) != 42 || rcode(resp) != dnsmessage.RCodeSuccess {
		t.Fatalf("bad tcp response: % x", resp[:4])
	}
}

func TestResolveDNS_HTTPSAnsweredEmptyWithoutUpstream(t *testing.T) {
	q, _ := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0x1234, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("x.com."), Type: dnsmessage.TypeHTTPS, Class: dnsmessage.ClassINET}},
	}).Pack()
	resp := resolveDNS(q)
	var m dnsmessage.Message
	if err := m.Unpack(resp); err != nil {
		t.Fatal(err)
	}
	if m.ID != 0x1234 || !m.Response || m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 || len(m.Questions) != 1 {
		t.Fatalf("response %+v", m)
	}
}

func TestResolveDNS_AAAAOfRelaySiteAnsweredEmpty(t *testing.T) {
	setDomains("x.com")
	defer setDomains("")
	q, _ := (&dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 0x1234, RecursionDesired: true},
		Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("api.x.com."), Type: dnsmessage.TypeAAAA, Class: dnsmessage.ClassINET}},
	}).Pack()
	var m dnsmessage.Message
	if err := m.Unpack(resolveDNS(q)); err != nil {
		t.Fatal(err)
	}
	if m.ID != 0x1234 || m.RCode != dnsmessage.RCodeSuccess || len(m.Answers) != 0 {
		t.Fatalf("response %+v", m)
	}
}

// fakeResolver answers every query on addr:53 with 5.5.5.5; skips the test
// when the address cannot be bound (no root, no IPv6 loopback).
func fakeResolver(t *testing.T, addr string, truncate *atomic.Bool) *atomic.Int32 {
	pc, err := net.ListenPacket("udp", net.JoinHostPort(addr, "53"))
	if err != nil {
		t.Skip("cannot bind: ", err)
	}
	t.Cleanup(func() { pc.Close() })
	var calls atomic.Int32
	go func() {
		buf := make([]byte, 4096)
		for {
			_, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			calls.Add(1)
			resp := mustAnswer(t, binary.BigEndian.Uint16(buf[:2]), "local.test.", 300, [4]byte{5, 5, 5, 5})
			if truncate != nil && truncate.Load() {
				resp[2] |= 0x02
			}
			pc.WriteTo(resp, from)
		}
	}()
	return &calls
}

func splitDNS(t *testing.T, servers string) *atomic.Int32 {
	resetDNS()
	setDomains("ours.test")
	protector = &fakeProtector{ok: true, dns: servers}
	t.Cleanup(func() { setDomains(""); protector = nil })
	var dohCalls atomic.Int32
	doh = fakeDoH(t, &dohCalls, func(q []byte) ([]byte, error) {
		return mustAnswer(t, binary.BigEndian.Uint16(q[:2]), "ours.test.", 300, [4]byte{9, 9, 9, 9}), nil
	})
	return &dohCalls
}

// Sites that are not ours are resolved by the network's own resolver, ours by DoH.
func TestResolveDNS_SplitsByName(t *testing.T) {
	var truncate atomic.Bool
	direct := fakeResolver(t, "127.0.0.1", &truncate)
	dohCalls := splitDNS(t, "127.0.0.1")

	resolveDNS(mustQuery(t, 1, "local.test.", dnsmessage.TypeA))
	if direct.Load() != 1 || dohCalls.Load() != 0 {
		t.Fatalf("foreign name: direct %d, doh %d", direct.Load(), dohCalls.Load())
	}
	resolveDNS(mustQuery(t, 2, "www.ours.test.", dnsmessage.TypeA))
	if direct.Load() != 1 || dohCalls.Load() != 1 {
		t.Fatalf("our name: direct %d, doh %d", direct.Load(), dohCalls.Load())
	}
	truncate.Store(true)
	resolveDNS(mustQuery(t, 3, "big.test.", dnsmessage.TypeA))
	if direct.Load() != 2 || dohCalls.Load() != 2 {
		t.Fatalf("truncated answer must fall back to doh: direct %d, doh %d", direct.Load(), dohCalls.Load())
	}
}

// Remote Config under a listed parent still asks the network: the app
// fetches it when the relay, and with it DoH, is down.
func TestResolveDNS_RemoteConfigAsksTheNetwork(t *testing.T) {
	direct := fakeResolver(t, "127.0.0.1", nil)
	dohCalls := splitDNS(t, "127.0.0.1")
	setDomains("googleapis.com")
	resolveDNS(mustQuery(t, 1, "firebaseremoteconfig.googleapis.com.", dnsmessage.TypeA))
	resolveDNS(mustQuery(t, 2, "firebaseinstallations.googleapis.com.", dnsmessage.TypeAAAA))
	if direct.Load() != 2 || dohCalls.Load() != 0 {
		t.Fatalf("direct %d, doh %d", direct.Load(), dohCalls.Load())
	}
	resolveDNS(mustQuery(t, 3, "www.googleapis.com.", dnsmessage.TypeA))
	if direct.Load() != 2 || dohCalls.Load() != 1 {
		t.Fatalf("listed neighbour: direct %d, doh %d", direct.Load(), dohCalls.Load())
	}
}

// LTE networks often hand out IPv6-only resolvers.
func TestResolveDNS_IPv6Resolver(t *testing.T) {
	direct := fakeResolver(t, "::1", nil)
	dohCalls := splitDNS(t, "::1")
	resp := resolveDNS(mustQuery(t, 1, "local.test.", dnsmessage.TypeA))
	if direct.Load() != 1 || dohCalls.Load() != 0 || rcode(resp) != dnsmessage.RCodeSuccess {
		t.Fatalf("direct %d, doh %d, rcode %v", direct.Load(), dohCalls.Load(), rcode(resp))
	}
}

func TestResolveDNS_DeadResolverTriesNext(t *testing.T) {
	direct := fakeResolver(t, "127.0.0.1", nil)
	dohCalls := splitDNS(t, "127.0.0.2\n127.0.0.1")
	resolveDNS(mustQuery(t, 1, "local.test.", dnsmessage.TypeA))
	if direct.Load() != 1 || dohCalls.Load() != 0 {
		t.Fatalf("direct %d, doh %d", direct.Load(), dohCalls.Load())
	}
}

// refusingResolver answers every query on addr:53 with a bare header
// carrying rc, as a carrier's resolver does to a foreign address.
func refusingResolver(t *testing.T, addr string, rc dnsmessage.RCode) *atomic.Int32 {
	pc, err := net.ListenPacket("udp", net.JoinHostPort(addr, "53"))
	if err != nil {
		t.Skip("cannot bind: ", err)
	}
	t.Cleanup(func() { pc.Close() })
	var calls atomic.Int32
	go func() {
		buf := make([]byte, 4096)
		for {
			_, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			calls.Add(1)
			resp := make([]byte, 12)
			copy(resp, buf[:2])
			resp[2], resp[3] = 0x81, byte(rc)
			pc.WriteTo(resp, from)
		}
	}()
	return &calls
}

// A refusal or failure is the server's, not the name's: the next server is
// asked, then DoH; the app never sees it and it is not cached.
func TestResolveDNS_RefusedTriesNext(t *testing.T) {
	for _, rc := range []dnsmessage.RCode{dnsmessage.RCodeRefused, dnsmessage.RCodeServerFailure, dnsmessage.RCodeNotImplemented} {
		t.Run(rc.String(), func(t *testing.T) {
			refusing := refusingResolver(t, "127.0.0.2", rc)
			direct := fakeResolver(t, "127.0.0.1", nil)
			dohCalls := splitDNS(t, "127.0.0.2\n127.0.0.1")
			resp := resolveDNS(mustQuery(t, 1, "local.test.", dnsmessage.TypeA))
			if refusing.Load() != 1 || direct.Load() != 1 || dohCalls.Load() != 0 || rcode(resp) != dnsmessage.RCodeSuccess {
				t.Fatalf("refusing %d, direct %d, doh %d, rcode %v", refusing.Load(), direct.Load(), dohCalls.Load(), rcode(resp))
			}

			protector = &fakeProtector{ok: true, dns: "127.0.0.2"}
			resp = resolveDNS(mustQuery(t, 2, "other.test.", dnsmessage.TypeA))
			if refusing.Load() != 2 || dohCalls.Load() != 1 || rcode(resp) != dnsmessage.RCodeSuccess {
				t.Fatalf("all refuse: refusing %d, doh %d, rcode %v", refusing.Load(), dohCalls.Load(), rcode(resp))
			}

			// DoH down too: nothing answered, so nothing may be cached.
			doh = fakeDoH(t, dohCalls, func([]byte) ([]byte, error) { return nil, errors.New("relay down") })
			resolveDNS(mustQuery(t, 3, "third.test.", dnsmessage.TypeA))
			resolveDNS(mustQuery(t, 4, "third.test.", dnsmessage.TypeA))
			if refusing.Load() != 4 {
				t.Fatalf("refusal cached: refusing %d", refusing.Load())
			}
		})
	}
}

func TestResolveDNS_NoResolversFallsBackToDoH(t *testing.T) {
	dohCalls := splitDNS(t, "")
	resp := resolveDNS(mustQuery(t, 1, "local.test.", dnsmessage.TypeA))
	if dohCalls.Load() != 1 || rcode(resp) != dnsmessage.RCodeSuccess {
		t.Fatalf("doh %d, rcode %v", dohCalls.Load(), rcode(resp))
	}
}

// The old network's resolver may have answered with addresses local to it.
func TestNetworkChanged_DropsDNSCache(t *testing.T) {
	direct := fakeResolver(t, "127.0.0.1", nil)
	splitDNS(t, "127.0.0.1")
	resolveDNS(mustQuery(t, 1, "local.test.", dnsmessage.TypeA))
	resolveDNS(mustQuery(t, 2, "local.test.", dnsmessage.TypeA))
	if direct.Load() != 1 {
		t.Fatalf("second lookup must hit the cache, direct %d", direct.Load())
	}
	NetworkChanged()
	resolveDNS(mustQuery(t, 3, "local.test.", dnsmessage.TypeA))
	if direct.Load() != 2 {
		t.Fatalf("lookup after a network change must go out again, direct %d", direct.Load())
	}
}

func TestThrottle_LineAMinutePerKind(t *testing.T) {
	now := time.Date(2026, 9, 25, 4, 30, 0, 0, time.UTC)
	th := throttle{every: time.Minute}
	if more, ok := th.allow("timeout", now); !ok || more != "" {
		t.Fatalf("first: %q %v", more, ok)
	}
	if _, ok := th.allow("no system resolvers", now); !ok {
		t.Fatal("another kind muted by the first")
	}
	for i := 0; i < 3; i++ {
		now = now.Add(10 * time.Second)
		if _, ok := th.allow("timeout", now); ok {
			t.Fatal("logged within the minute")
		}
	}
	now = now.Add(40 * time.Second)
	if more, ok := th.allow("timeout", now); !ok || more != " (+3 more, latest 40s ago)" {
		t.Fatalf("after a minute: %q %v", more, ok)
	}
}

// A network change flushes the muted count instead of leaving it for a
// line hours later about something else.
func TestThrottle_FlushForgets(t *testing.T) {
	now := time.Date(2026, 9, 25, 13, 44, 32, 0, time.UTC)
	th := throttle{every: time.Minute}
	th.allow("timeout", now)
	th.allow("timeout", now.Add(30*time.Second))
	got := th.flush(now.Add(time.Minute))
	if len(got) != 1 || got[0] != "timeout: 1 more, latest 30s ago" {
		t.Fatalf("%q", got)
	}
	if more, ok := th.allow("timeout", now.Add(61*time.Second)); !ok || more != "" {
		t.Fatalf("after flush: %q %v", more, ok)
	}
}
