package tunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"net"
	"slices"
	"strings"
	"syscall"
	"time"
)

var (
	protector    Host     // Protect keeps a direct socket out of the TUN
	relayDomains []string // lower-case suffixes from Start
)

// Remote Config and the Installations token it needs. The app asks them for
// a new relay list exactly when the relay is down, and googleapis.com is a
// listed domain: through the relay the fetch would die in DoH. Exact names,
// resolved by the network and dialed direct like any site not on the list.
var alwaysDirect = []string{"firebaseremoteconfig.googleapis.com", "firebaseinstallations.googleapis.com"}

func setDomains(list string) {
	relayDomains = relayDomains[:0]
	for _, d := range strings.Split(list, "\n") {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			relayDomains = append(relayDomains, strings.TrimPrefix(d, "."))
		}
	}
}

// relayByName: the site (or one of its parents) is on the list.
func relayByName(name string) bool {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if slices.Contains(alwaysDirect, name) {
		return false
	}
	for _, d := range relayDomains {
		if name == d || strings.HasSuffix(name, "."+d) {
			return true
		}
	}
	return false
}

// dialDirect opens a plain socket past the TUN for a site that is not ours.
func dialDirect(ctx context.Context, network, dst string) (net.Conn, error) {
	d := &net.Dialer{Timeout: handshakeTimeout}
	if p := protector; p != nil {
		d.Control = func(_, _ string, c syscall.RawConn) error {
			var ok bool
			c.Control(func(fd uintptr) { ok = p.Protect(int(fd)) })
			if !ok {
				return errProtect
			}
			return nil
		}
	}
	return d.DialContext(ctx, network, dst)
}

// Routing by name. The first bytes of a TCP session from the
// app say which site it is: a TLS ClientHello carries the hostname in the
// SNI extension. TLS and MTProto clients speak first, so waiting for them
// costs nothing; a protocol where the server speaks first loses peekWait.
var errProtect = errors.New("protect failed")

const (
	peekWait = 100 * time.Millisecond
	peekMax  = 16 * 1024 // a TLS record at most
)

// peekClientHello reads what the app sends first, up to a full TLS record
// if the first bytes look like one (Chrome's hello can span two segments).
// Returns whatever was read; the caller forwards it verbatim.
func peekClientHello(c net.Conn) []byte {
	buf := make([]byte, 0, 2048)
	deadline := time.Now().Add(peekWait)
	want := 0
	for len(buf) < peekMax {
		c.SetReadDeadline(deadline)
		chunk := make([]byte, 2048)
		n, err := c.Read(chunk)
		buf = append(buf, chunk[:n]...)
		if err != nil {
			break
		}
		if want == 0 && len(buf) >= 5 && buf[0] == 0x16 {
			want = 5 + int(binary.BigEndian.Uint16(buf[3:5]))
		}
		if want == 0 || len(buf) >= want {
			break
		}
	}
	c.SetReadDeadline(time.Time{})
	return buf
}

// parseSNI returns the server_name from a TLS ClientHello, "" if b is not
// one or carries none.
func parseSNI(b []byte) string {
	// Record header: type 0x16, version, length. Handshake: type 1, len24.
	if len(b) < 5+4 || b[0] != 0x16 || b[5] != 0x01 {
		return ""
	}
	hs := b[5:]
	hl := int(hs[1])<<16 | int(hs[2])<<8 | int(hs[3])
	if len(hs)-4 < hl {
		hl = len(hs) - 4 // truncated hello: parse what is there
	}
	p := hs[4 : 4+hl]
	// version(2) random(32) session_id
	if len(p) < 35 {
		return ""
	}
	p = p[34:]
	n := int(p[0])
	if len(p) < 1+n+2 {
		return ""
	}
	p = p[1+n:]
	// cipher suites
	n = int(binary.BigEndian.Uint16(p))
	if len(p) < 2+n+1 {
		return ""
	}
	p = p[2+n:]
	// compression methods
	n = int(p[0])
	if len(p) < 1+n+2 {
		return ""
	}
	p = p[1+n:]
	// extensions
	n = int(binary.BigEndian.Uint16(p))
	p = p[2:]
	if len(p) < n {
		n = len(p)
	}
	p = p[:n]
	for len(p) >= 4 {
		typ := binary.BigEndian.Uint16(p)
		l := int(binary.BigEndian.Uint16(p[2:]))
		if len(p) < 4+l {
			return ""
		}
		ext := p[4 : 4+l]
		p = p[4+l:]
		if typ != 0 { // server_name
			continue
		}
		// list length(2), then entries: type(1) len(2) name
		if len(ext) < 2 {
			return ""
		}
		ext = ext[2:]
		for len(ext) >= 3 {
			nt := ext[0]
			nl := int(binary.BigEndian.Uint16(ext[1:]))
			if len(ext) < 3+nl {
				return ""
			}
			if nt == 0 {
				return string(ext[3 : 3+nl])
			}
			ext = ext[3+nl:]
		}
		return ""
	}
	return ""
}
