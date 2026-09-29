package main

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// The relay side of `make rc-promote`: a credential by invite code, then per
// endpoint a TLS handshake, the upgrade and one page through the relay.
// Requests as android/tunnel/tunnel.go makes them (upgrade, Register); the
// pinned pair in relay_test.go and the tunnel's v3_test.go holds them
// together. Plain crypto/tls, not the app's Chrome hello: nginx routes by
// SNI only, and the check runs from outside the censored network anyway.
const (
	kindInvite = 2
	credSize   = 77
	chromeUA   = "Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/140.0.0.0 Mobile Safari/537.36"
	padMin     = 16
	padMax     = 256
	// A credential closer to its end than this is not used: it could lapse mid-check.
	credMargin = time.Hour
)

var (
	errCredRejected = errors.New("credential refused (404)")
	errNeedInvite   = errors.New("an invite code is needed: `make invite` in relay/ on the VDS, then make rc-promote INVITE=<code>")
)

func credExpires(cred []byte) time.Time {
	if len(cred) != credSize {
		return time.Time{}
	}
	return time.Unix(int64(binary.BigEndian.Uint32(cred[9:13])), 0)
}

func browserHeaders(sni string) string {
	var n [1]byte
	rand.Read(n[:])
	pad := make([]byte, padMin+int(n[0])*(padMax-padMin)/255)
	rand.Read(pad)
	return "User-Agent: " + chromeUA + "\r\nOrigin: https://" + sni + "\r\n" +
		"Accept-Encoding: gzip, deflate, br\r\nAccept-Language: ru-RU,ru;q=0.9,en-US;q=0.8\r\n" +
		"Cache-Control: no-cache\r\nPragma: no-cache\r\n" +
		"Cookie: sid=" + base64.RawURLEncoding.EncodeToString(pad) + "\r\n"
}

func upgradeRequest(ep endpoint, cred []byte) string {
	var k [16]byte
	rand.Read(k[:])
	return "GET " + ep.Path + " HTTP/1.1\r\nHost: " + ep.Host + "\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n" +
		browserHeaders(ep.Host) +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + base64.StdEncoding.EncodeToString(k[:]) + "\r\n" +
		"Sec-WebSocket-Extensions: permessage-deflate; client_max_window_bits\r\n" +
		"Sec-WebSocket-Protocol: " + hex.EncodeToString(cred) + "\r\n\r\n"
}

func registerRequest(ep endpoint, kind byte, proof []byte) string {
	body := append([]byte{kind}, proof...)
	return fmt.Sprintf("POST %s HTTP/1.1\r\nHost: %s\r\n%sContent-Type: application/octet-stream\r\nContent-Length: %d\r\n\r\n%s",
		ep.Path, ep.Host, browserHeaders(ep.Host), len(body), body)
}

// relayHeader is the 7 bytes after the 101: TCP to ip:port.
func relayHeader(dst *net.TCPAddr) []byte {
	h := []byte{0x01, 0, 0, 0, 0, 0, 0}
	copy(h[1:5], dst.IP.To4())
	binary.BigEndian.PutUint16(h[5:], uint16(dst.Port))
	return h
}

// target is where the one page through the relay comes from.
type target struct {
	addr  string // ip:port, the relay gets no name
	sni   string
	roots *x509.CertPool // nil: system roots
}

var cloudflare = target{addr: "1.1.1.1:443", sni: "one.one.one.one"}

type prober struct {
	roots   *x509.CertPool // the relays' CA; nil: system roots
	timeout time.Duration
	target  target
}

func (p prober) dial(ep endpoint) (net.Conn, error) {
	raw, err := net.DialTimeout("tcp", net.JoinHostPort(ep.IP, fmt.Sprint(ep.Port)), p.timeout)
	if err != nil {
		return nil, err
	}
	raw.SetDeadline(time.Now().Add(2 * p.timeout))
	c := tls.Client(raw, &tls.Config{ServerName: ep.Host, RootCAs: p.roots, NextProtos: []string{"http/1.1"}})
	if err := c.Handshake(); err != nil {
		raw.Close()
		return nil, fmt.Errorf("tls: %w", err)
	}
	return c, nil
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(b []byte) (int, error) { return c.r.Read(b) }

func (p prober) register(ep endpoint, invite string) ([]byte, error) {
	c, err := p.dial(ep)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	if _, err := io.WriteString(c, registerRequest(ep, kindInvite, []byte(invite))); err != nil {
		return nil, err
	}
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		return nil, fmt.Errorf("reply: %w", err)
	}
	// The relay's every refusal is the site's 404, not-an-issuer included.
	if res.StatusCode == http.StatusNotFound {
		return nil, errors.New("code refused (404): used, mistyped, or this relay issues no credentials")
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("http %d", res.StatusCode)
	}
	var st [2]byte
	if _, err := io.ReadFull(br, st[:]); err != nil || st[0] != 0 || st[1] != credSize {
		return nil, fmt.Errorf("bad reply (status %d, size %d, %v)", st[0], st[1], err)
	}
	cred := make([]byte, credSize)
	if _, err := io.ReadFull(br, cred); err != nil {
		return nil, fmt.Errorf("reply: %w", err)
	}
	return cred, nil
}

// check: the relay behind ep takes cred and carries one HEAD to the target.
func (p prober) check(ep endpoint, cred []byte) error {
	c, err := p.dial(ep)
	if err != nil {
		return err
	}
	defer c.Close()
	if _, err := io.WriteString(c, upgradeRequest(ep, cred)); err != nil {
		return err
	}
	br := bufio.NewReader(c)
	res, err := http.ReadResponse(br, nil)
	if err != nil {
		return fmt.Errorf("upgrade reply: %w", err)
	}
	switch res.StatusCode {
	case http.StatusSwitchingProtocols:
	case http.StatusNotFound:
		return errCredRejected
	default:
		// 502/504: nginx is up, the relay behind it is not.
		return fmt.Errorf("upgrade: http %d", res.StatusCode)
	}
	dst, err := net.ResolveTCPAddr("tcp", p.target.addr)
	if err != nil {
		return err
	}
	if _, err := c.Write(relayHeader(dst)); err != nil {
		return err
	}
	tc := tls.Client(&bufConn{c, br}, &tls.Config{ServerName: p.target.sni, RootCAs: p.target.roots, NextProtos: []string{"http/1.1"}})
	if err := tc.Handshake(); err != nil {
		return fmt.Errorf("through the relay: tls: %w", err)
	}
	if _, err := io.WriteString(tc, "HEAD / HTTP/1.1\r\nHost: "+p.target.sni+"\r\nConnection: close\r\n\r\n"); err != nil {
		return fmt.Errorf("through the relay: %w", err)
	}
	if _, err := http.ReadResponse(bufio.NewReader(tc), &http.Request{Method: http.MethodHead}); err != nil {
		return fmt.Errorf("through the relay: %w", err)
	}
	return nil
}

// scrub keeps the endpoint's own strings out of an error: the output goes
// to terminals and chats, endpoints are named by number only.
func scrub(err error, ep endpoint) string {
	s := err.Error()
	for _, v := range []string{net.JoinHostPort(ep.IP, fmt.Sprint(ep.Port)), ep.IP, ep.Host, ep.Path} {
		s = strings.ReplaceAll(s, v, "…")
	}
	return s
}

// credential returns the cached credential while it is good, else one
// registered with invite on the first endpoint that issues. The cache file
// is written in place: under Docker it is a bind-mounted file.
func credential(p prober, eps []endpoint, path, invite string, now time.Time, out io.Writer) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	cached, _ := hex.DecodeString(strings.TrimSpace(string(b)))
	exp := credExpires(cached)
	if exp.After(now.Add(credMargin)) {
		if invite != "" {
			fmt.Fprintf(out, "credential: cached until %s, the invite code stays unused\n", exp.UTC().Format(time.DateTime))
		}
		return cached, nil
	}
	if invite == "" {
		if !exp.IsZero() {
			return nil, fmt.Errorf("credential expired %s: %w", exp.UTC().Format(time.DateTime), errNeedInvite)
		}
		return nil, errNeedInvite
	}
	var errs []string
	for i, ep := range eps {
		cred, err := p.register(ep, invite)
		if err != nil {
			errs = append(errs, fmt.Sprintf("endpoint %d: %s", i+1, scrub(err, ep)))
			continue
		}
		fmt.Fprintf(out, "credential: registered via endpoint %d, valid until %s\n", i+1, credExpires(cred).UTC().Format(time.DateTime))
		// The code is spent by now: this run still uses the credential.
		if err := os.WriteFile(path, []byte(hex.EncodeToString(cred)+"\n"), 0o600); err != nil {
			fmt.Fprintf(out, "warning: credential not cached (%v), the next run needs a new invite code\n", err)
		}
		return cred, nil
	}
	return nil, fmt.Errorf("register: %s", strings.Join(errs, "; "))
}

// checkAll reports each endpoint by number and whether all of them work.
func checkAll(p prober, eps []endpoint, cred []byte, out io.Writer) bool {
	ok, refused := true, false
	for i, ep := range eps {
		t0 := time.Now()
		if err := p.check(ep, cred); err != nil {
			fmt.Fprintf(out, "endpoint %d/%d: %s\n", i+1, len(eps), scrub(err, ep))
			ok = false
			refused = refused || errors.Is(err, errCredRejected)
			continue
		}
		fmt.Fprintf(out, "endpoint %d/%d: ok in %s\n", i+1, len(eps), time.Since(t0).Round(time.Millisecond))
	}
	if refused {
		fmt.Fprintln(out, "a refused credential: revoked, or the relay has another issuer key; delete rc/.check-cred and run again with INVITE=<code>")
	}
	return ok
}
