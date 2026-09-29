package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// Protocol v3: the relay hides behind the cover site. nginx terminates TLS
// for the site's own domain and proxies one secret location here as a
// WebSocket upgrade, so the relay speaks a few lines of HTTP first:
//
//	GET <path> + Upgrade: websocket + Sec-WebSocket-Protocol: <hex credential>
//	→ 101, then the 7-byte header and raw bytes as in v2 (no WS framing)
//	POST <path>, body [1 kind][proof] → 200, body [1 status][1 credLen][cred],
//	status always 0: a refusal is the 404 below
//
// Anything else gets nginx's own 404, so a probe that somehow found the path
// sees a static site. The 101 doubles as the ping: a rejected credential is
// visible to the client before it sends anything.
const notFound = "<html>\r\n<head><title>404 Not Found</title></head>\r\n<body>\r\n<center><h1>404 Not Found</h1></center>\r\n<hr><center>nginx</center>\r\n</body>\r\n</html>\r\n"

// bufConn reads through the bufio.Reader that parsed the request; bytes the
// client sent right after the headers are not lost.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (c *bufConn) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *bufConn) CloseWrite() error {
	if tc, ok := c.Conn.(*net.TCPConn); ok {
		return tc.CloseWrite()
	}
	return nil
}

func httpReply(w io.Writer, status string, ctype string, body []byte) {
	fmt.Fprintf(w, "HTTP/1.1 %s\r\nServer: nginx\r\nDate: %s\r\nContent-Type: %s\r\nContent-Length: %d\r\nConnection: close\r\n\r\n",
		status, time.Now().UTC().Format(http.TimeFormat), ctype, len(body))
	w.Write(body)
}

// Only nginx talks to the relay, and it proxies any method it accepts: every
// request that is not ours, even unparsable, gets the 404 — a dropped
// connection would be nginx's 502, which no static site gives.
func handleHTTP(client net.Conn, src string) {
	br := bufio.NewReader(client)
	req, err := http.ReadRequest(br)
	if err != nil {
		log.Printf("[%s] http: %v", src, err)
		httpReply(client, "404 Not Found", "text/html", []byte(notFound))
		return
	}
	switch {
	case req.Method == http.MethodPost:
		n := req.ContentLength
		if n < 2 || n > 1+proofMax {
			log.Printf("[%s] register: bad body length %d", src, n)
			httpReply(client, "404 Not Found", "text/html", []byte(notFound))
			return
		}
		body := make([]byte, n)
		if _, err := io.ReadFull(req.Body, body); err != nil {
			log.Printf("[%s] register: body read: %v", src, err)
			httpReply(client, "404 Not Found", "text/html", []byte(notFound))
			return
		}
		// The path ships in the APK: a refusal that differed from nginx's 404
		// would let one POST tell the relay apart from the site.
		st, cred := registerProof(body[0], body[1:], src)
		if st != statusOK {
			httpReply(client, "404 Not Found", "text/html", []byte(notFound))
			return
		}
		httpReply(client, "200 OK", "application/octet-stream", append([]byte{st, byte(len(cred))}, cred...))

	case req.Method == http.MethodGet && strings.EqualFold(req.Header.Get("Upgrade"), "websocket"):
		cred, _ := hex.DecodeString(req.Header.Get("Sec-WebSocket-Protocol"))
		dev, err := verifyCred(cred, time.Now())
		if err != nil {
			log.Printf("[%s] http: %v (cred %d bytes, ua %q)", src, err, len(cred), req.UserAgent())
			httpReply(client, "404 Not Found", "text/html", []byte(notFound))
			return
		}
		src = "dev:" + dev
		if !acquireKey(src, maxConnsPerDevice) {
			log.Printf("[%s] per-device limit %d reached (rejected total %d)", src, maxConnsPerDevice, rejected.Add(1))
			httpReply(client, "404 Not Found", "text/html", []byte(notFound))
			return
		}
		defer releaseIP(src)
		accept := ""
		if k := req.Header.Get("Sec-WebSocket-Key"); k != "" {
			h := sha1.Sum([]byte(k + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
			accept = "Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(h[:]) + "\r\n"
		}
		fmt.Fprintf(client, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n%s\r\n", accept)
		var hdr [headerSize]byte
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			log.Printf("[%s] header read: %v", src, err)
			return
		}
		session(&bufConn{client, br}, hdr[:], src)

	default:
		log.Printf("[%s] http: %s not ours, host %q ua %q", src, req.Method, req.Host, req.UserAgent())
		var b bytes.Buffer
		httpReply(&b, "404 Not Found", "text/html", []byte(notFound))
		if req.Method == http.MethodHead {
			b.Truncate(b.Len() - len(notFound))
		}
		client.Write(b.Bytes())
	}
}
