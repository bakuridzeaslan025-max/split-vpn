package tunnel

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
)

const (
	dohURL  = "https://cloudflare-dns.com/dns-query"
	dohSNI  = "cloudflare-dns.com"
	dohPort = 443

	dnsMinTTL = 30 * time.Second
	dnsMaxTTL = 5 * time.Minute
	dnsMaxMsg = 4096

	directDNSTimeout = 2 * time.Second
)

// verbose logs every direct lookup: the whole phone's browsing, so debug
// builds only, never into a log the user may share.
var verbose atomic.Bool

func SetVerbose(v bool) { verbose.Store(v) }

var dohIP = net.IPv4(1, 1, 1, 1).To4()

type dnsCacheEntry struct {
	msg []byte
	exp time.Time
}

var dnsCache sync.Map // question key -> dnsCacheEntry

// A dead network resolver fails every lookup; a line a minute per kind of
// failure says so. The count of the muted ones is flushed on a network
// change or stop, so it never lands on an unrelated later line.
var directFailLog = throttle{every: time.Minute}

type throttle struct {
	mu    sync.Mutex
	every time.Duration
	kinds map[string]*throttled
}

type throttled struct {
	logged, latest time.Time
	muted          int
}

// allow reports whether to log a failure of this kind now; more tells how
// many were muted since the last line of the kind.
func (t *throttle) allow(kind string, now time.Time) (more string, ok bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	k := t.kinds[kind]
	if k == nil {
		if t.kinds == nil {
			t.kinds = map[string]*throttled{}
		}
		k = &throttled{}
		t.kinds[kind] = k
	} else if now.Sub(k.logged) < t.every {
		k.muted++
		k.latest = now
		return "", false
	}
	if k.muted > 0 {
		more = fmt.Sprintf(" (+%d more, latest %s ago)", k.muted, now.Sub(k.latest).Round(time.Second))
	}
	k.logged, k.muted = now, 0
	return more, true
}

func flushDirectFails() {
	for _, l := range directFailLog.flush(time.Now()) {
		log.Printf("dns: direct failed, %s", l)
	}
}

// flush returns what was muted and forgets every kind.
func (t *throttle) flush(now time.Time) []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []string
	for kind, k := range t.kinds {
		if k.muted > 0 {
			out = append(out, fmt.Sprintf("%s: %d more, latest %s ago", kind, k.muted, now.Sub(k.latest).Round(time.Second)))
		}
	}
	t.kinds = nil
	sort.Strings(out)
	return out
}

// newDoHClient talks to Cloudflare DoH through the relay: TLS to VDS,
// header to 1.1.1.1:443, then a second TLS inside it. HTTP/1.1 keep-alive
// so consecutive lookups reuse the relay connection.
func newDoHClient(cred []byte) *http.Client {
	tr := &http.Transport{
		DialTLSContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			c, err := dialRelay(ctx, cred, dohIP, dohPort)
			if err != nil {
				return nil, err
			}
			tc := tls.Client(c, &tls.Config{ServerName: dohSNI})
			if err := tc.HandshakeContext(ctx); err != nil {
				c.Close()
				return nil, err
			}
			return tc, nil
		},
		MaxIdleConns:        4,
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     60 * time.Second,
	}
	return &http.Client{Transport: tr, Timeout: 8 * time.Second}
}

func serveDNSUDP(c *gonet.UDPConn) {
	defer guard("serveDNSUDP")
	defer c.Close()
	buf := make([]byte, dnsMaxMsg)
	for {
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		n, addr, err := c.ReadFrom(buf)
		if err != nil {
			return
		}
		q := append([]byte(nil), buf[:n]...)
		go func() {
			defer guard("resolveDNS")
			// Fails when the client stopped waiting (the late answer is
			// cached for its retry) or the TUN is being rebuilt, which the
			// log already says.
			c.WriteTo(resolveDNS(q), addr)
		}()
	}
}

func serveDNSTCP(c net.Conn) {
	var lenBuf [2]byte
	for {
		c.SetReadDeadline(time.Now().Add(30 * time.Second))
		if _, err := io.ReadFull(c, lenBuf[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(lenBuf[:]))
		if n == 0 || n > dnsMaxMsg {
			return
		}
		q := make([]byte, n)
		if _, err := io.ReadFull(c, q); err != nil {
			return
		}
		resp := resolveDNS(q)
		out := make([]byte, 2+len(resp))
		binary.BigEndian.PutUint16(out, uint16(len(resp)))
		copy(out[2:], resp)
		c.SetWriteDeadline(time.Now().Add(10 * time.Second))
		if _, err := c.Write(out); err != nil {
			return
		}
	}
}

// resolveDNS answers a wire-format query, from cache or via DoH.
// Never returns nil: on failure the client gets SERVFAIL immediately
// instead of waiting for its own timeout.
func resolveDNS(q []byte) []byte {
	if len(q) < 12 {
		return servfail(q)
	}
	key := questionKey(q)
	// Ad domains get NXDOMAIN, any type. Never cached, here or (no SOA) by
	// clients: with blocking turned off they resolve right after the restart.
	if adBlock.Load() {
		if name, _, _ := strings.Cut(key, "/"); blocked(name) {
			if verbose.Load() {
				log.Printf("dns %s: blocked", name)
			}
			return emptyAnswer(q, dnsmessage.RCodeNameError)
		}
	}
	// HTTPS records carry the ECH config that lets Chrome hide the site
	// name from the ClientHello, and routing by name needs that name. An
	// empty answer makes clients fall back to plain SNI (and to TCP: the
	// same record advertises h3, which the TUN refuses anyway).
	if questionType(q) == dnsmessage.TypeHTTPS {
		return emptyAnswer(q, dnsmessage.RCodeSuccess)
	}
	// The TUN carries no IPv6 routes, so a v6 address of our site would be
	// dialed straight into the block and only then retried over v4.
	if name, ok := strings.CutSuffix(key, "/"+dnsmessage.TypeAAAA.String()); ok && relayByName(name) {
		return emptyAnswer(q, dnsmessage.RCodeSuccess)
	}
	if key != "" {
		if v, ok := dnsCache.Load(key); ok {
			e := v.(dnsCacheEntry)
			if time.Now().Before(e.exp) {
				return withID(e.msg, q[:2])
			}
			dnsCache.Delete(key)
		}
	}

	if name, _, _ := strings.Cut(key, "/"); key != "" && !relayByName(name) {
		if resp, err := directQuery(q); err == nil {
			dnsCache.Store(key, dnsCacheEntry{msg: resp, exp: time.Now().Add(responseTTL(resp))})
			return withID(resp, q[:2])
		} else if more, ok := directFailLog.allow(errKind(err), time.Now()); ok {
			log.Printf("dns %s: direct failed, falling back to doh: %v%s", key, err, more)
		}
	} else if key != "" && directDNSWanted(name) {
		if resp, err := directQuery(q); err == nil {
			observeRoutes(resp)
			dnsCache.Store(key, dnsCacheEntry{msg: resp, exp: time.Now().Add(responseTTL(resp))})
			// After the Store: a drop in between must find the flag still set.
			directDNSUsed.Store(true)
			return withID(resp, q[:2])
		}
	}

	mu.Lock()
	client := doh
	mu.Unlock()
	if client == nil {
		log.Printf("dns %s: no doh client", key)
		return servfail(q)
	}
	t0 := time.Now()
	resp, err := dohQuery(client, q)
	if err != nil {
		if !health.down() {
			log.Printf("dns %s: doh error: %v", key, err)
		}
		return servfail(q)
	}
	log.Printf("dns %s: %d bytes in %s", key, len(resp), time.Since(t0).Round(time.Millisecond))
	observeRoutes(resp)
	if key != "" {
		dnsCache.Store(key, dnsCacheEntry{msg: resp, exp: time.Now().Add(responseTTL(resp))})
	}
	return withID(resp, q[:2])
}

// observeRoutes feeds the route cache with an answer for one of our sites.
func observeRoutes(resp []byte) {
	mu.Lock()
	c := rc
	mu.Unlock()
	if c != nil {
		if site, ips := answerA(resp); site != "" && len(ips) > 0 {
			c.observe(site, ips)
		}
	}
}

// directQuery asks the underlying network's own resolver, past the TUN.
// Sites that are not ours are dialed directly, so they need the answer the
// user's network would get (nearby CDN node, local-only names), not the one
// the relay's country gets. Truncated answers count as failures: DoH has
// no size limit. The servers are those of the network protect()ed sockets
// leave by, the system's default one.
func directQuery(q []byte) ([]byte, error) {
	p := protector
	if p == nil {
		return nil, errors.New("no host")
	}
	err := errors.New("no system resolvers")
	for _, srv := range strings.Fields(p.DnsServers()) {
		var resp []byte
		t0 := time.Now()
		if resp, err = directQueryTo(net.JoinHostPort(srv, "53"), q); err == nil {
			if verbose.Load() {
				log.Printf("dns %s: direct via %s, %d bytes in %s", questionKey(q), srv, len(resp), time.Since(t0).Round(time.Millisecond))
			}
			return resp, nil
		}
	}
	return nil, err
}

func directQueryTo(addr string, q []byte) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), directDNSTimeout)
	defer cancel()
	c, err := dialDirect(ctx, "udp", addr)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(directDNSTimeout))
	if _, err := c.Write(q); err != nil {
		return nil, err
	}
	buf := make([]byte, 4096)
	n, err := c.Read(buf)
	if err != nil {
		return nil, err
	}
	if n < 12 || !bytes.Equal(buf[:2], q[:2]) || buf[2]&0x80 == 0 {
		return nil, errors.New("bad response")
	}
	if buf[2]&0x02 != 0 {
		return nil, errors.New("truncated")
	}
	// As bionic's res_send: these say nothing about the name, so the next
	// server is asked. A carrier's resolver refuses queries from outside
	// its network.
	switch rc := dnsmessage.RCode(buf[3] & 0x0f); rc {
	case dnsmessage.RCodeServerFailure, dnsmessage.RCodeNotImplemented, dnsmessage.RCodeRefused:
		return nil, fmt.Errorf("answer %v", rc)
	}
	return buf[:n], nil
}

func dohQuery(client *http.Client, q []byte) ([]byte, error) {
	req, err := http.NewRequest(http.MethodPost, dohURL, bytes.NewReader(q))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/dns-message")
	req.Header.Set("Accept", "application/dns-message")
	// Marks the POST replayable: without it net/http gives up when a
	// kept-alive relay connection turns out dead instead of redialing.
	req.Header.Set("Idempotency-Key", "dns")
	res, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("doh status %d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, dnsMaxMsg))
	if err != nil || len(body) < 12 {
		return nil, fmt.Errorf("doh short body")
	}
	return body, nil
}

func questionKey(q []byte) string {
	var p dnsmessage.Parser
	if _, err := p.Start(q); err != nil {
		return ""
	}
	qs, err := p.Question()
	if err != nil {
		return ""
	}
	return qs.Name.String() + "/" + qs.Type.String()
}

func responseTTL(msg []byte) time.Duration {
	var p dnsmessage.Parser
	if _, err := p.Start(msg); err != nil {
		return dnsMinTTL
	}
	if err := p.SkipAllQuestions(); err != nil {
		return dnsMinTTL
	}
	ttl := dnsMaxTTL
	found := false
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			break
		}
		found = true
		if d := time.Duration(h.TTL) * time.Second; d < ttl {
			ttl = d
		}
		if err := p.SkipAnswer(); err != nil {
			break
		}
	}
	if !found {
		return dnsMinTTL
	}
	if ttl < dnsMinTTL {
		ttl = dnsMinTTL
	}
	return ttl
}

// answerA returns the question name and the A records of a response.
func answerA(msg []byte) (string, []net.IP) {
	var p dnsmessage.Parser
	if _, err := p.Start(msg); err != nil {
		return "", nil
	}
	qs, err := p.Question()
	if err != nil {
		return "", nil
	}
	p.SkipAllQuestions()
	var ips []net.IP
	for {
		h, err := p.AnswerHeader()
		if err != nil {
			break
		}
		if h.Type != dnsmessage.TypeA {
			p.SkipAnswer()
			continue
		}
		r, err := p.AResource()
		if err != nil {
			break
		}
		ips = append(ips, net.IP(r.A[:]))
	}
	return qs.Name.String(), ips
}

func questionType(q []byte) dnsmessage.Type {
	var p dnsmessage.Parser
	if _, err := p.Start(q); err != nil {
		return 0
	}
	qs, err := p.Question()
	if err != nil {
		return 0
	}
	return qs.Type
}

// emptyAnswer is code with the question echoed and no records.
func emptyAnswer(q []byte, code dnsmessage.RCode) []byte {
	var p dnsmessage.Parser
	h, err := p.Start(q)
	if err != nil {
		return servfail(q)
	}
	qs, err := p.Question()
	if err != nil {
		return servfail(q)
	}
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: h.ID, Response: true, RCode: code, RecursionDesired: h.RecursionDesired, RecursionAvailable: true})
	b.StartQuestions()
	b.Question(qs)
	out, err := b.Finish()
	if err != nil {
		return servfail(q)
	}
	return out
}

func withID(msg, id []byte) []byte {
	out := make([]byte, len(msg))
	copy(out, msg)
	copy(out[:2], id)
	return out
}

func servfail(q []byte) []byte {
	if len(q) < 12 {
		return nil
	}
	out := make([]byte, len(q))
	copy(out, q)
	out[2] |= 0x80           // QR
	out[3] = out[3]&0xF0 | 2 // RCODE SERVFAIL
	return out
}
