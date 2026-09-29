package main

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

const (
	typeTCP byte = 0x01
	typeUDP byte = 0x02

	headerSize   = 7 // 1 type + 4 IP + 2 port
	dialTimeout  = 10 * time.Second
	idleTimeout  = 2 * time.Minute
	proxyLineMax = 108 // PROXY v1 spec: longest line incl. CRLF
)

var (
	activeConns atomic.Int64
	rejected    atomic.Int64

	// One device opens 20+ connections per page (1 TCP = 1 TLS, no mux).
	// MAX_CONNS caps the whole relay; the VDS is a 1-CPU box.
	maxConns = envInt("MAX_CONNS", 4096)
	// Per-source cap needs the real client IP. Behind nginx that only comes
	// with PROXY protocol, so the cap is tied to it: without it every client
	// would share nginx's address and the cap would be a global one.
	proxyProto    = os.Getenv("PROXY_PROTOCOL") == "1"
	maxConnsPerIP = envInt("MAX_CONNS_PER_IP", 128)
	// Per-credential cap: the real anti-hog limit once clients carry
	// credentials, works behind NAT and without PROXY protocol.
	maxConnsPerDevice = envInt("MAX_CONNS_PER_DEVICE", 128)

	perIPMu sync.Mutex
	perIP   = map[string]int{}
)

func main() {
	addr := envOr("LISTEN_ADDR", "0.0.0.0:4460")

	if len(os.Args) > 1 && os.Args[1] == "keygen" {
		priv, pub := keygen()
		fmt.Printf("ISSUER_KEY=%s\nCRED_PUB=%s\n", priv, pub)
		return
	}

	loadKeys()
	if credPub == nil {
		log.Fatal("CRED_PUB or ISSUER_KEY must be set")
	}
	integrity = integrityFromEnv()
	log.Printf("auth: cred_pub=%v issuer=%v integrity=%v invites=%q revoked=%q",
		credPub != nil, issuerPriv != nil, integrity != nil, invitesFile, revokedFile)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("listen %s: %v", addr, err)
	}
	log.Printf("relay listening on %s", addr)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Println("shutting down")
		ln.Close()
	}()

	log.Printf("limits: max_conns=%d proxy_protocol=%v max_conns_per_ip=%d", maxConns, proxyProto, maxConnsPerIP)
	serve(ln)
}

func serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			log.Printf("accept: %v", err)
			return
		}
		if n := activeConns.Load(); n >= int64(maxConns) {
			log.Printf("too many connections (%d/%d, rejected total %d), rejecting %s",
				n, maxConns, rejected.Add(1), conn.RemoteAddr())
			conn.Close()
			continue
		}
		activeConns.Add(1)
		go handleConn(conn)
	}
}

// acquireIP takes a per-source slot; false when the source is at its cap.
func acquireIP(ip string) bool { return acquireKey(ip, maxConnsPerIP) }

func acquireKey(key string, limit int) bool {
	perIPMu.Lock()
	defer perIPMu.Unlock()
	if perIP[key] >= limit {
		return false
	}
	perIP[key]++
	return true
}

func releaseIP(ip string) {
	perIPMu.Lock()
	defer perIPMu.Unlock()
	if perIP[ip] <= 1 {
		delete(perIP, ip)
	} else {
		perIP[ip]--
	}
}

// readProxyLine parses a PROXY protocol v1 header and returns the source IP.
// Reads byte by byte so nothing past the line is consumed.
func readProxyLine(r io.Reader) (string, error) {
	var line []byte
	b := make([]byte, 1)
	for len(line) < proxyLineMax {
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		line = append(line, b[0])
		if b[0] == '\n' {
			break
		}
	}
	f := strings.Fields(strings.TrimSuffix(string(line), "\r\n"))
	// PROXY TCP4 src dst sport dport
	if len(f) != 6 || f[0] != "PROXY" || net.ParseIP(f[2]) == nil {
		return "", fmt.Errorf("bad PROXY line %q", line)
	}
	return f[2], nil
}

func handleConn(client net.Conn) {
	defer activeConns.Add(-1)
	defer client.Close()

	client.SetDeadline(time.Now().Add(30 * time.Second))

	src := client.RemoteAddr().String()
	if proxyProto {
		ip, err := readProxyLine(client)
		if err != nil {
			log.Printf("[%s] %v", src, err)
			return
		}
		src = ip
		if !acquireIP(ip) {
			log.Printf("[%s] per-ip limit %d reached (rejected total %d)", ip, maxConnsPerIP, rejected.Add(1))
			return
		}
		defer releaseIP(ip)
	}

	handleHTTP(client, src)
}

// session relays one authenticated connection described by the 7-byte header.
func session(client net.Conn, hdr []byte, src string) {
	connType := hdr[0]
	dstIP := net.IP(hdr[1:5])
	dstPort := binary.BigEndian.Uint16(hdr[5:7])
	dst := fmt.Sprintf("%s:%d", dstIP, dstPort)

	if connType == typeUDP {
		log.Printf("UDP not implemented yet, dest=%s", dst)
		return
	}
	if connType != typeTCP {
		log.Printf("[%s] unknown type 0x%02x", src, connType)
		return
	}

	if !isAllowedDest(dstIP, dstPort) {
		log.Printf("blocked dest %s", dst)
		return
	}

	// Who went where is never logged: a line with both src and dst is a
	// browsing history. Failures below name the destination only.
	remote, err := net.DialTimeout("tcp", dst, dialTimeout)
	if err != nil {
		log.Printf("dial %s: %v", dst, err)
		return
	}
	defer remote.Close()

	// Clear the header-read deadline, set idle timeout
	client.SetDeadline(time.Time{})

	relay(client, remote)
}

func relay(a, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)

	cp := func(dst, src net.Conn) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			src.SetReadDeadline(time.Now().Add(idleTimeout))
			n, err := src.Read(buf)
			if n > 0 {
				dst.SetWriteDeadline(time.Now().Add(30 * time.Second))
				if _, werr := dst.Write(buf[:n]); werr != nil {
					break
				}
			}
			if err != nil {
				break
			}
		}
		// Half-close to signal EOF
		if hc, ok := dst.(interface{ CloseWrite() error }); ok {
			hc.CloseWrite()
		}
	}

	go cp(b, a)
	go cp(a, b)
	wg.Wait()
}

// isAllowedDest: anything public. Clients are authenticated and pick their
// own sites, so no site list here. Two things stay closed for the server's
// sake: private/local ranges (the compose network next door: panel,
// matrix) and SMTP (spam from our IP would blacklist it).
// testLoopback lets the tests relay to echo servers on 127.0.0.1.
var testLoopback bool

func isAllowedDest(ip net.IP, port uint16) bool {
	if port == 25 {
		return false
	}
	ip4 := ip.To4()
	if testLoopback && ip4 != nil && ip4.IsLoopback() {
		return true
	}
	if ip4 == nil || ip4.IsLoopback() || ip4.IsPrivate() || ip4.IsLinkLocalUnicast() ||
		ip4.IsMulticast() || ip4.IsUnspecified() || ip4[0] == 100 && ip4[1]&0xC0 == 64 {
		return false
	}
	return true
}

func envInt(key string, fallback int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil && v > 0 {
		return v
	}
	return fallback
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
