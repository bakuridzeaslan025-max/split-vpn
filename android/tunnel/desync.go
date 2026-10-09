package tunnel

// YouTube direct, past the relay: the ClientHello goes out cut up so that
// the provider's DPI does not see the name whole (ByeDPI-style, no root:
// TCP segmentation, TTL=1 disorder, an OOB byte, TLS record split). A
// connection the server does not answer falls back to the relay with the
// same hello: the app sees nothing but a delay. Which strategy works is
// picked on live traffic per network, see desyncpick.go. Every line starts
// with "sni ": Crashlytics never gets them.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var desyncHosts = []string{
	"youtube.com", "youtu.be", "googlevideo.com", "ytimg.com", "ggpht.com",
	"youtube-nocookie.com", "youtubei.googleapis.com",
}

// desyncDomain is the entry of desyncHosts that name falls under, "" if none.
func desyncDomain(name string) string {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	for _, d := range desyncHosts {
		if name == d || strings.HasSuffix(name, "."+d) {
			return d
		}
	}
	return ""
}

func desyncByName(name string) bool { return desyncDomain(name) != "" }

var (
	// hello → first server byte, connect included. ServerHello comes in
	// 40–300 ms on LTE, disorder adds the kernel's RTO. A var for tests.
	desyncWait = 3 * time.Second
	// The whole wait for a ClientHello that came in two segments: its
	// header already told the length, the rest is on its way.
	helloWait = 300 * time.Millisecond
)

// Strategies are data (Remote Config yt_strategies), parsed here and,
// duplicated, in rc/tool/strategies.go: keep the two in step, the vector in
// testdata/strategies.json is shared (make tunnel-test compares the copies).
//
// A spec is a ;-list: rec=<positions> (TLS record boundaries),
// cut=<positions> (TCP segment boundaries), ttl1=<segment numbers, from 1,
// or mid: the segment holding midsld> (sent with TTL 1), oob (an urgent
// byte after the first segment), tiny=<1|2> (segments of that size across
// the name). Positions: a number from the start of the ClientHello,
// host+N (N bytes into the name), midsld (middle of the second-level
// label), hostend (the name's last byte). "" is no tricks at all.
type strategy struct {
	id   string
	norm string // the spec as normalized: what logs and tests compare
	recs []position
	cuts []position
	ttl1 []int // segment numbers from 1; 0 is mid
	oob  bool
	tiny int
}

type position struct {
	kind byte // 'n' number, 'h' host+n, 'm' midsld, 'e' hostend
	n    int
}

func (p position) String() string {
	switch p.kind {
	case 'h':
		return "host+" + strconv.Itoa(p.n)
	case 'm':
		return "midsld"
	case 'e':
		return "hostend"
	}
	return strconv.Itoa(p.n)
}

// at is the offset in the hello; off is where the name starts.
func (p position) at(off int, sni string) int {
	switch p.kind {
	case 'h':
		return off + p.n
	case 'm':
		labels := strings.Split(sni, ".")
		if len(labels) < 2 {
			return off + len(sni)/2
		}
		sld := labels[len(labels)-2]
		start := len(sni) - len(labels[len(labels)-1]) - 1 - len(sld)
		return off + start + len(sld)/2
	case 'e':
		return off + len(sni) - 1
	}
	return p.n
}

func parsePosition(s string) (position, error) {
	switch {
	case s == "midsld":
		return position{kind: 'm'}, nil
	case s == "hostend":
		return position{kind: 'e'}, nil
	case strings.HasPrefix(s, "host+"):
		n, err := strconv.Atoi(s[len("host+"):])
		if err != nil || n < 0 || n > 255 || !isDigits(s[len("host+"):]) {
			return position{}, fmt.Errorf("bad position %q", s)
		}
		return position{kind: 'h', n: n}, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > peekMax || !isDigits(s) {
		return position{}, fmt.Errorf("bad position %q", s)
	}
	return position{kind: 'n', n: n}, nil
}

func isDigits(s string) bool {
	return s != "" && strings.Trim(s, "0123456789") == ""
}

// items splits a comma list; an empty item is an error.
func items(v string) ([]string, error) {
	var out []string
	for _, it := range strings.Split(v, ",") {
		it = strings.TrimSpace(it)
		if it == "" {
			return nil, errors.New("empty item")
		}
		out = append(out, it)
	}
	return out, nil
}

func parseSpec(spec string) (strategy, error) {
	var st strategy
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return st, nil
	}
	seen := map[string]bool{}
	for _, part := range strings.Split(spec, ";") {
		part = strings.TrimSpace(part)
		key, val, hasVal := strings.Cut(part, "=")
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if key == "" {
			return st, errors.New("empty part")
		}
		if seen[key] {
			return st, fmt.Errorf("%s twice", key)
		}
		seen[key] = true
		if key == "oob" {
			if hasVal {
				return st, errors.New("oob takes no value")
			}
			st.oob = true
			continue
		}
		if !hasVal || val == "" {
			return st, fmt.Errorf("%s: no value", key)
		}
		switch key {
		case "rec", "cut":
			its, err := items(val)
			if err != nil {
				return st, fmt.Errorf("%s: %v", key, err)
			}
			var ps []position
			for _, it := range its {
				p, err := parsePosition(it)
				if err != nil {
					return st, fmt.Errorf("%s: %v", key, err)
				}
				if !slices.Contains(ps, p) {
					ps = append(ps, p)
				}
			}
			if key == "rec" {
				st.recs = ps
			} else {
				st.cuts = ps
			}
		case "ttl1":
			its, err := items(val)
			if err != nil {
				return st, fmt.Errorf("ttl1: %v", err)
			}
			for _, it := range its {
				n := 0
				if it != "mid" {
					if n, err = strconv.Atoi(it); err != nil || n < 1 || n > 64 || !isDigits(it) {
						return st, fmt.Errorf("ttl1: bad segment %q", it)
					}
				}
				if !slices.Contains(st.ttl1, n) {
					st.ttl1 = append(st.ttl1, n)
				}
			}
		case "tiny":
			if val != "1" && val != "2" {
				return st, errors.New("tiny: 1 or 2")
			}
			st.tiny = int(val[0] - '0')
		default:
			return st, fmt.Errorf("unknown %q", key)
		}
	}
	segmented := len(st.cuts) > 0 || st.tiny > 0
	switch {
	case len(st.cuts) > 0 && st.tiny > 0:
		return st, errors.New("cut and tiny together")
	case len(st.ttl1) > 0 && !segmented:
		return st, errors.New("ttl1 without cut or tiny")
	case st.oob && !segmented:
		return st, errors.New("oob without cut or tiny")
	}
	st.norm = st.normalize()
	return st, nil
}

func (st strategy) normalize() string {
	join := func(ps []position) string {
		s := make([]string, len(ps))
		for i, p := range ps {
			s[i] = p.String()
		}
		return strings.Join(s, ",")
	}
	var parts []string
	if len(st.recs) > 0 {
		parts = append(parts, "rec="+join(st.recs))
	}
	if len(st.cuts) > 0 {
		parts = append(parts, "cut="+join(st.cuts))
	}
	if len(st.ttl1) > 0 {
		s := make([]string, len(st.ttl1))
		for i, n := range st.ttl1 {
			s[i] = strconv.Itoa(n)
			if n == 0 {
				s[i] = "mid"
			}
		}
		parts = append(parts, "ttl1="+strings.Join(s, ","))
	}
	if st.oob {
		parts = append(parts, "oob")
	}
	if st.tiny > 0 {
		parts = append(parts, "tiny="+strconv.Itoa(st.tiny))
	}
	return strings.Join(parts, ";")
}

var strategyID = regexp.MustCompile(`^[a-z0-9-]{1,16}$`)

// parseStrategies reads yt_strategies. A broken entry is dropped and named
// in problems, the rest stay; err is for a value that is no list at all.
func parseStrategies(js string) (list []strategy, problems []string, err error) {
	var raw []json.RawMessage
	if err := json.Unmarshal([]byte(js), &raw); err != nil || raw == nil {
		return nil, nil, errors.New("not a JSON array")
	}
	list = []strategy{}
	ids := map[string]bool{}
	for i, r := range raw {
		var e struct {
			ID   *string `json:"id"`
			Spec *string `json:"spec"`
		}
		if json.Unmarshal(r, &e) != nil || e.ID == nil || e.Spec == nil {
			problems = append(problems, fmt.Sprintf("#%d: want {\"id\", \"spec\"}", i+1))
			continue
		}
		if !strategyID.MatchString(*e.ID) {
			problems = append(problems, fmt.Sprintf("#%d: bad id %q", i+1, *e.ID))
			continue
		}
		if ids[*e.ID] {
			problems = append(problems, fmt.Sprintf("%s: id twice", *e.ID))
			continue
		}
		st, err := parseSpec(*e.Spec)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", *e.ID, err))
			continue
		}
		ids[*e.ID] = true
		st.id = *e.ID
		list = append(list, st)
	}
	return list, problems, nil
}

// strategies is replaced whole by SetStrategies: a connection works with the
// list it took at its start, the picker keeps ids, never indexes.
var strategies atomic.Pointer[[]strategy]

func strategiesNow() []strategy {
	if p := strategies.Load(); p != nil {
		return *p
	}
	return nil
}

// SetStrategies sets the strategies from Remote Config's yt_strategies, a
// JSON array of {"id", "spec"} in the order to try them; an empty one turns
// direct YouTube off. Broken entries are dropped and the rest kept; the
// result names what was dropped, "" when nothing was. Call before Start.
func SetStrategies(js string) string {
	list, problems, err := parseStrategies(js)
	if err != nil {
		list, problems = nil, []string{err.Error()}
	}
	strategies.Store(&list)
	if len(problems) > 0 {
		log.Printf("desync: %d strategies, dropped: %s", len(list), strings.Join(problems, "; "))
	}
	return strings.Join(problems, "; ")
}

// tlsRecSplit cuts the single TLS record of h at each of ps (ascending,
// offsets in h) into several records.
func tlsRecSplit(h []byte, ps []int) []byte {
	out := make([]byte, 0, len(h)+5*len(ps))
	prev := 5
	for _, p := range append(ps, len(h)) {
		n := p - prev
		out = append(out, h[0], h[1], h[2], byte(n>>8), byte(n))
		out = append(out, h[prev:p]...)
		prev = p
	}
	return out
}

// plan turns a strategy into the bytes to send and their segments; h is
// one whole TLS record.
func (st strategy) plan(h []byte) (buf []byte, segs [][2]int, ttl1 map[int]bool, oobAfter int) {
	sni, off := sniAt(h)
	if off < 0 {
		return h, [][2]int{{0, len(h)}}, nil, -1
	}
	var recAt []int
	for _, p := range st.recs {
		if at := p.at(off, sni); at > 5 && at < len(h) && !slices.Contains(recAt, at) {
			recAt = append(recAt, at)
		}
	}
	slices.Sort(recAt)
	buf = h
	if len(recAt) > 0 {
		buf = tlsRecSplit(h, recAt)
	}
	// Offsets in h → in buf, past the headers inserted before them.
	shift := func(p int) int {
		n := 0
		for _, r := range recAt {
			if p >= r {
				n++
			}
		}
		return p + 5*n
	}
	var cuts []int
	for _, p := range st.cuts {
		cuts = append(cuts, shift(p.at(off, sni)))
	}
	if st.tiny > 0 {
		from, to := shift(off)-2*st.tiny, shift(off+len(sni))+st.tiny
		for p := max(from, 1); p < to && p < len(buf); p += st.tiny {
			cuts = append(cuts, p)
		}
	}
	slices.Sort(cuts)
	prev := 0
	for _, c := range cuts {
		if c <= prev || c >= len(buf) {
			continue
		}
		segs = append(segs, [2]int{prev, c})
		prev = c
	}
	segs = append(segs, [2]int{prev, len(buf)})
	mid := shift(position{kind: 'm'}.at(off, sni))
	ttl1 = map[int]bool{}
	for _, n := range st.ttl1 {
		i := n - 1
		if n == 0 {
			i = slices.IndexFunc(segs, func(s [2]int) bool { return mid >= s[0] && mid < s[1] })
		}
		// Never the last one: nothing would follow it to make the server
		// ask for the lost one again.
		if i >= 0 && i < len(segs)-1 {
			ttl1[i] = true
		}
	}
	oobAfter = -1
	if st.oob && len(segs) > 1 {
		oobAfter = 0
	}
	return
}

func sockInt(c *net.TCPConn, level, opt int) (int, error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return 0, err
	}
	var v int
	var serr error
	if err := rc.Control(func(fd uintptr) { v, serr = syscall.GetsockoptInt(int(fd), level, opt) }); err != nil {
		return 0, err
	}
	return v, serr
}

func setSockInt(c *net.TCPConn, level, opt, v int) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) { serr = syscall.SetsockoptInt(int(fd), level, opt, v) }); err != nil {
		return err
	}
	return serr
}

// sendOOB sends b with its last byte as TCP urgent data: the server's
// stack drops it from the stream, a DPI box sees it inline.
func sendOOB(c *net.TCPConn, b []byte) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var n int
	var serr error
	err = rc.Write(func(fd uintptr) bool {
		n, serr = syscall.SendmsgN(int(fd), b, nil, nil, syscall.MSG_OOB)
		return !errors.Is(serr, syscall.EAGAIN)
	})
	if err != nil {
		return err
	}
	// The rest sent again would move the urgent byte: the try fails and
	// the connection goes by relay.
	if serr == nil && n < len(b) {
		return fmt.Errorf("oob: %d of %d bytes sent", n, len(b))
	}
	return serr
}

const (
	sendQueueCap = 50 * time.Millisecond
	// Without tcp_info's notsent count (an old kernel): a pause instead,
	// usually long enough for a segment to leave.
	sendPause = 5 * time.Millisecond
	// tcp_info up to tcpi_notsent_bytes (Linux 4.6); minSdk 26 allows 4.4.
	tcpInfoNotSentEnd = 148
)

// tcpInfo reads struct tcp_info; n is what the kernel filled. The ioctl
// SIOCOUTQNSD would say the same, but SELinux denies it to apps.
func tcpInfo(c *net.TCPConn) (info [232]byte, n int, err error) {
	rc, err := c.SyscallConn()
	if err != nil {
		return info, 0, err
	}
	l := uint32(len(info))
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall6(syscall.SYS_GETSOCKOPT, fd, syscall.IPPROTO_TCP, syscall.TCP_INFO,
			uintptr(unsafe.Pointer(&info[0])), uintptr(unsafe.Pointer(&l)), 0)
	}); err != nil {
		return info, 0, err
	}
	if errno != 0 {
		return info, 0, errno
	}
	return info, int(l), nil
}

// notSent is tcpi_notsent_bytes: written, not yet on the wire.
func notSent(info []byte) (uint32, error) {
	if len(info) < tcpInfoNotSentEnd {
		return 0, fmt.Errorf("tcp_info of %d bytes, no notsent", len(info))
	}
	return binary.NativeEndian.Uint32(info[144:]), nil
}

type sendWait int

const (
	sentDrained sendWait = iota
	sentCapped           // still queued at the cap
	sentPaused           // no notsent count: a fixed pause
)

// waitSent waits for the kernel to put everything written so far on the
// wire, as ByeDPI's wait_send. TCP_NODELAY does not stop autocorking: a
// write while the last one still sits in the queue joins it, and the
// segments a strategy needs (and TTL 1 on one of them) melt into one.
// why says what stood in the way of counting, for sentPaused.
func waitSent(c *net.TCPConn) (w sendWait, why error) {
	deadline := time.Now().Add(sendQueueCap)
	for {
		info, n, err := tcpInfo(c)
		var left uint32
		if err == nil {
			left, err = notSent(info[:n])
		}
		if err != nil {
			time.Sleep(sendPause)
			return sentPaused, err
		}
		if left == 0 {
			return sentDrained, nil
		}
		if time.Now().After(deadline) {
			return sentCapped, nil
		}
		time.Sleep(time.Millisecond)
	}
}

// segsOut is tcp_info's tcpi_segs_out (Linux 4.2+): the segments the
// socket really sent, for the debug log.
func segsOut(c *net.TCPConn) (uint32, bool) {
	info, n, err := tcpInfo(c)
	if err != nil || n < 140 {
		return 0, false
	}
	return binary.NativeEndian.Uint32(info[136:]), true
}

// sendDesync writes the record h to c the way st says. Returns a
// description for the log.
func sendDesync(c *net.TCPConn, st *strategy, h []byte) (string, error) {
	c.SetNoDelay(true)
	buf, segs, ttl1, oobAfter := st.plan(h)
	if len(segs) == 1 {
		_, err := c.Write(buf)
		return "1 seg", err
	}
	ttl, err := sockInt(c, syscall.IPPROTO_IP, syscall.IP_TTL)
	if err != nil {
		return "", fmt.Errorf("get ttl: %w", err)
	}
	segs0, counted := uint32(0), false
	if verbose.Load() {
		segs0, counted = segsOut(c)
	}
	var desc []string
	capped, paused := 0, 0
	var pauseWhy error
	for i, s := range segs {
		low := ttl1[i]
		if low {
			if err := setSockInt(c, syscall.IPPROTO_IP, syscall.IP_TTL, 1); err != nil {
				return "", fmt.Errorf("set ttl: %w", err)
			}
		}
		part := buf[s[0]:s[1]]
		if i == oobAfter {
			err = sendOOB(c, append(append([]byte{}, part...), 'a'))
		} else {
			_, err = c.Write(part)
		}
		// TTL is taken when the segment leaves, not when it is written.
		if err == nil && i < len(segs)-1 {
			switch w, why := waitSent(c); w {
			case sentCapped:
				capped++
			case sentPaused:
				paused, pauseWhy = paused+1, why
			}
		}
		if low {
			if e := setSockInt(c, syscall.IPPROTO_IP, syscall.IP_TTL, ttl); e != nil && err == nil {
				err = fmt.Errorf("restore ttl: %w", e)
			}
		}
		if err != nil {
			return "", err
		}
		d := strconv.Itoa(s[1] - s[0])
		if low {
			d += "!ttl1"
		}
		if i == oobAfter {
			d += "+oob"
		}
		desc = append(desc, d)
	}
	r := fmt.Sprintf("%d segs [%s]", len(segs), strings.Join(desc, " "))
	if len(desc) > 12 {
		r = fmt.Sprintf("%d segs [%s … %s]", len(segs), strings.Join(desc[:6], " "), strings.Join(desc[len(desc)-3:], " "))
	}
	if extra := (len(buf) - len(h)) / 5; extra > 0 {
		r += fmt.Sprintf(" %d records", extra+1)
	}
	if n, ok := segsOut(c); counted && ok {
		r += fmt.Sprintf(", %d sent", n-segs0)
	}
	if capped > 0 {
		r += fmt.Sprintf(", queue not drained in %s %d×", sendQueueCap, capped)
	}
	if paused > 0 {
		r += fmt.Sprintf(", paused %s %d× (%v)", sendPause, paused, pauseWhy)
	}
	return r, nil
}

var (
	errDesyncStopped = errors.New("stopped")
	// The hello never went out: nothing to say about the strategy.
	errNoConnect = errors.New("connect")
)

// dialDesync: direct socket, desynced hello, first answer from the server,
// all within desyncWait. Only the hello's own record is cut; bytes the app
// sent after it follow as they are. On success the answer is already
// read: the caller hands it to the app. The socket is tracked, Stop
// closes it.
func dialDesync(name, dst string, hello []byte, st *strategy) (net.Conn, []byte, string, error) {
	t0 := time.Now()
	deadline := t0.Add(desyncWait)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	c, err := dialDirect(ctx, "tcp", dst)
	if err != nil {
		return nil, nil, "", fmt.Errorf("%w: %w", errNoConnect, err)
	}
	tr := &trackedConn{Conn: c}
	if !trackConn(tr) {
		c.Close()
		return nil, nil, "", errDesyncStopped
	}
	tc := c.(*net.TCPConn)
	tConn := time.Since(t0)
	tc.SetDeadline(deadline)
	rec := hello[:min(len(hello), 5+int(binary.BigEndian.Uint16(hello[3:5])))]
	desc, err := sendDesync(tc, st, rec)
	if err == nil && len(hello) > len(rec) {
		_, err = tc.Write(hello[len(rec):])
	}
	if err != nil {
		tr.Close()
		return nil, nil, "", fmt.Errorf("send: %w", err)
	}
	ans := make([]byte, 16*1024)
	n, err := tc.Read(ans)
	tFirst := time.Since(t0)
	if err != nil {
		tr.Close()
		return nil, nil, "", fmt.Errorf("%s, connect %s, %w after %s", desc, tConn.Round(time.Millisecond), err, tFirst.Round(time.Millisecond))
	}
	tc.SetDeadline(time.Time{})
	if ans[0] != 0x16 {
		tr.Close()
		kind := fmt.Sprintf("non-tls 0x%02x", ans[0])
		if ans[0] == 0x15 {
			kind = "alert"
		}
		return nil, nil, "", fmt.Errorf("%s, got %s (%d bytes) after %s", desc, kind, n, tFirst.Round(time.Millisecond))
	}
	return tr, ans[:n], fmt.Sprintf("%s, connect %s, first %d bytes after %s", desc, tConn.Round(time.Millisecond), n, tFirst.Round(time.Millisecond)), nil
}

// countConn is the direct socket past the hello: bytes both ways, the
// peak rate down and how each side ended. Read runs in one goroutine and
// CloseWrite in the other; both are read only after relay returns.
type countConn struct {
	net.Conn
	up, down   int64
	burstStart time.Time
	lastRead   time.Time
	burstBytes int64
	peak       int64 // bytes/s
	readErr    error // how the server side ended
	readEnd    time.Time
	appEnd     time.Time // the app finished sending
}

// A burst: reads with no pause over burstGap between them. YouTube fetches
// a chunk at full speed and then idles, so the rate of a burst, not of the
// whole session, is what DPI throttling would cap.
const (
	burstGap      = 500 * time.Millisecond
	burstMinTime  = 250 * time.Millisecond // shorter ones are mostly the socket buffer
	burstMinBytes = 64 << 10               // so are small ones, unless they took a while
	burstLong     = time.Second
)

// Read measures the peak: the fastest burst down.
func (c *countConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.down += int64(n)
	now := time.Now()
	if err != nil && c.readErr == nil {
		c.readErr, c.readEnd = err, now
	}
	if !c.lastRead.IsZero() && now.Sub(c.lastRead) > burstGap {
		c.closeBurst()
	}
	// The read that opens a burst arrived before it: not part of its rate.
	if c.burstStart.IsZero() {
		c.burstStart, c.burstBytes = now, 0
	} else {
		c.burstBytes += int64(n)
	}
	c.lastRead = now
	if err != nil {
		c.closeBurst()
	}
	return n, err
}

func (c *countConn) closeBurst() {
	if d := c.lastRead.Sub(c.burstStart); d >= burstLong || d >= burstMinTime && c.burstBytes >= burstMinBytes {
		if r := int64(float64(c.burstBytes) / d.Seconds()); r > c.peak {
			c.peak = r
		}
	}
	c.burstStart = time.Time{}
}

func (c *countConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	c.up += int64(n)
	return n, err
}

func (c *countConn) CloseWrite() error {
	if c.appEnd.IsZero() {
		c.appEnd = time.Now()
	}
	if hc, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return hc.CloseWrite()
	}
	return nil
}

// serverFirst: the server's side ended before the app's, and not by our
// own Close (Stop, NetworkLost).
func (c *countConn) serverFirst() bool {
	return c.readErr != nil && !errors.Is(c.readErr, net.ErrClosed) && (c.appEnd.IsZero() || c.readEnd.Before(c.appEnd))
}

// completeHello reads the rest of a hello record that came in pieces,
// until deadline. Whether it is whole now.
func completeHello(c net.Conn, b []byte, deadline time.Time) ([]byte, bool) {
	if len(b) < 5 {
		return b, false
	}
	want := 5 + int(binary.BigEndian.Uint16(b[3:5]))
	chunk := make([]byte, 2048)
	for len(b) < want {
		c.SetReadDeadline(deadline)
		n, err := c.Read(chunk)
		b = append(b, chunk[:n]...)
		if err != nil {
			break
		}
	}
	c.SetReadDeadline(time.Time{})
	return b, len(b) >= want
}

// tryDesync takes a YouTube connection direct if the picker says so.
// done: the connection was served (or the tunnel is going away);
// otherwise the caller relays it with the returned bytes, which may have
// grown by the rest of the hello.
func tryDesync(app net.Conn, first []byte, name string, dstIP net.IP, dstPort uint16, t0 time.Time) ([]byte, bool) {
	ip := dstIP.String()
	st, gen := picker.pick(ip)
	if st == nil {
		return first, false
	}
	if len(first) >= 5 && len(first) < 5+int(binary.BigEndian.Uint16(first[3:5])) {
		var whole bool
		first, whole = completeHello(app, first, t0.Add(helloWait))
		waited := time.Since(t0).Round(time.Millisecond)
		if !whole {
			// Cut where it was cut short, the hello would be broken and the
			// strategy blamed for it.
			log.Printf("sni desync: hello incomplete after %s → relay", waited)
			return first, false
		}
		log.Printf("sni desync: hello waited %s", waited)
	}
	dst := net.JoinHostPort(ip, strconv.Itoa(int(dstPort)))
	start := time.Now()
	dc, ans, desc, err := dialDesync(name, dst, first, st)
	if errors.Is(err, errDesyncStopped) || errors.Is(err, net.ErrClosed) {
		return first, true
	}
	if errors.Is(err, errNoConnect) {
		picker.unreachable(gen, ip)
	} else {
		picker.result(gen, st.id, ip, start, err == nil, fmt.Sprint(err))
	}
	if err != nil {
		if verbose.Load() {
			log.Printf("sni desync %s → %s %s FAIL: %v → relay", name, dst, st.id, err)
		}
		return first, false
	}
	if verbose.Load() {
		log.Printf("sni desync %s → %s %s ok: %s", name, dst, st.id, desc)
	}
	out := &countConn{Conn: dc}
	defer out.Close()
	app.SetWriteDeadline(time.Now().Add(handshakeTimeout))
	if _, err := app.Write(ans); err != nil {
		return first, true
	}
	app.SetWriteDeadline(time.Time{})
	relay(app, out)
	d := time.Since(start)
	down := out.down + int64(len(ans))
	if verbose.Load() {
		log.Printf("sni desync %s %s done: down %d KB, up %d KB in %s (avg %.0f KB/s, peak %d KB/s), server: %v",
			name, st.id, down/1024, out.up/1024, d.Round(100*time.Millisecond),
			float64(down)/1024/max(d.Seconds(), 0.001), out.peak/1024, out.readErr)
	}
	// The server's end, not the app's: an app may keep its side open long after.
	if out.serverFirst() {
		d = out.readEnd.Sub(start)
	}
	picker.finished(gen, st.id, ip, name, finish{
		start: start, dur: d, down: down, up: out.up, peak: out.peak,
		serverFirst: out.serverFirst(), ours: errors.Is(out.readErr, net.ErrClosed),
		why: fmt.Sprintf("%s, %v", name, out.readErr),
	})
	return first, true
}
