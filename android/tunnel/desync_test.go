package tunnel

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// unrecord joins the bodies of consecutive TLS records.
func unrecord(b []byte) []byte {
	var out []byte
	for len(b) >= 5 {
		n := int(b[3])<<8 | int(b[4])
		if len(b) < 5+n {
			break
		}
		out = append(out, b[5:5+n]...)
		b = b[5+n:]
	}
	return out
}

// records splits b into its TLS records' bodies.
func records(b []byte) [][]byte {
	var out [][]byte
	for len(b) >= 5 {
		n := int(b[3])<<8 | int(b[4])
		if len(b) < 5+n {
			break
		}
		out = append(out, b[5:5+n])
		b = b[5+n:]
	}
	return out
}

// The list the plan starts with, as rc/config.json has it.
const planStrategies = `[
	{"id": "rec2", "spec": "rec=host+1,midsld;cut=host+1,midsld"},
	{"id": "rec-mid-dis", "spec": "rec=midsld;cut=1;ttl1=1"},
	{"id": "dis-mid", "spec": "cut=host+1,midsld;ttl1=2"},
	{"id": "oob-mid", "spec": "cut=host+1,midsld;ttl1=2;oob"},
	{"id": "tiny", "spec": "tiny=1;ttl1=mid"},
	{"id": "none", "spec": ""}
]`

func mustStrategies(t *testing.T, js string) []strategy {
	t.Helper()
	list, problems, err := parseStrategies(js)
	if err != nil || len(problems) > 0 {
		t.Fatal(err, problems)
	}
	return list
}

func byID(t *testing.T, list []strategy, id string) *strategy {
	t.Helper()
	for i := range list {
		if list[i].id == id {
			return &list[i]
		}
	}
	t.Fatalf("no strategy %s", id)
	return nil
}

func TestSniAt(t *testing.T) {
	h := clientHello(t, "rr1---sn-abc.googlevideo.com")
	name, off := sniAt(h)
	if name != "rr1---sn-abc.googlevideo.com" || string(h[off:off+len(name)]) != name {
		t.Fatalf("got %q at %d", name, off)
	}
	if _, off := sniAt(h[:20]); off != -1 {
		t.Fatal("truncated hello yielded an offset")
	}
}

// host+1 breaks "youtubei", midsld "youtube" and "googlevideo": the words
// the DPI looks for.
func TestPositions(t *testing.T) {
	for _, c := range []struct{ name, pos, before, after string }{
		{"rr1---sn-abc.googlevideo.com", "midsld", "rr1---sn-abc.googl", "evideo.com"},
		{"www.youtube.com", "midsld", "www.you", "tube.com"},
		{"youtu.be", "midsld", "yo", "utu.be"},
		{"youtubei.googleapis.com", "host+1", "y", "outubei.googleapis.com"},
		{"localhost", "midsld", "loca", "lhost"},
		{"i.ytimg.com", "hostend", "i.ytimg.co", "m"},
		{"i.ytimg.com", "host+0", "", "i.ytimg.com"},
	} {
		h := clientHello(t, c.name)
		_, off := sniAt(h)
		p, err := parsePosition(c.pos)
		if err != nil {
			t.Fatal(err)
		}
		at := p.at(off, c.name)
		if got := string(h[off:at]) + "|" + string(h[at:off+len(c.name)]); got != c.before+"|"+c.after {
			t.Errorf("%s %s: %s", c.name, c.pos, got)
		}
	}
}

// testdata/strategies.json is shared with rc/tool (make tunnel-test
// compares the copies): the app takes what the tool lets through.
func TestParseStrategies_SharedVector(t *testing.T) {
	b, err := os.ReadFile("testdata/strategies.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name    string          `json:"name"`
		In      json.RawMessage `json:"in"`
		Keep    [][2]string     `json:"keep"`
		Dropped int             `json:"dropped"`
		Error   bool            `json:"error"`
	}
	if err := json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		list, problems, err := parseStrategies(string(c.In))
		if (err != nil) != c.Error {
			t.Errorf("%s: err %v", c.Name, err)
			continue
		}
		got := [][2]string{}
		for _, s := range list {
			got = append(got, [2]string{s.id, s.norm})
		}
		if !c.Error && (!reflect.DeepEqual(got, append([][2]string{}, c.Keep...)) || len(problems) != c.Dropped) {
			t.Errorf("%s: kept %v, dropped %d: %v", c.Name, got, len(problems), problems)
		}
	}
}

// A normalized spec reads back as itself.
func TestParseSpec_NormIsStable(t *testing.T) {
	for _, s := range mustStrategies(t, planStrategies) {
		again, err := parseSpec(s.norm)
		if err != nil || again.norm != s.norm {
			t.Errorf("%s: %q → %q, %v", s.id, s.norm, again.norm, err)
		}
	}
}

func TestSetStrategies_KeepsTheGoodOnes(t *testing.T) {
	t.Cleanup(func() { strategies.Store(nil) })
	if p := SetStrategies(`[{"id":"a","spec":""},{"id":"b","spec":"cut=x"}]`); !strings.Contains(p, "b:") {
		t.Fatalf("problems %q", p)
	}
	if l := strategiesNow(); len(l) != 1 || l[0].id != "a" {
		t.Fatalf("list %v", l)
	}
	if p := SetStrategies("garbage"); p == "" || len(strategiesNow()) != 0 {
		t.Fatalf("garbage: %q, %v", p, strategiesNow())
	}
	if p := SetStrategies("[]"); p != "" || len(strategiesNow()) != 0 {
		t.Fatal("[] is no strategies, quietly")
	}
}

func TestPlan_WhatGoesWhere(t *testing.T) {
	h := clientHello(t, "www.youtube.com")
	list := mustStrategies(t, planStrategies)

	buf, segs, ttl1, oob := byID(t, list, "rec2").plan(h)
	if rs := records(buf); len(rs) != 3 {
		t.Fatalf("rec2: %d records", len(rs))
	}
	for _, r := range records(buf) {
		if bytes.Contains(r, []byte("youtube")) {
			t.Fatal("rec2: a record holds the whole word")
		}
	}
	if len(segs) != 3 || len(ttl1) != 0 || oob != -1 {
		t.Fatalf("rec2: segs %v ttl1 %v oob %d", segs, ttl1, oob)
	}

	_, segs, ttl1, _ = byID(t, list, "rec-mid-dis").plan(h)
	if segs[0] != [2]int{0, 1} || !ttl1[0] || len(ttl1) != 1 {
		t.Fatalf("rec-mid-dis: segs %v ttl1 %v", segs, ttl1)
	}
	_, segs, ttl1, oob = byID(t, list, "oob-mid").plan(h)
	if len(segs) != 3 || !ttl1[1] || len(ttl1) != 1 || oob != 0 {
		t.Fatalf("oob-mid: segs %v ttl1 %v oob %d", segs, ttl1, oob)
	}

	_, off := sniAt(h)
	mid := off + len("www.you")
	_, segs, ttl1, _ = byID(t, list, "tiny").plan(h)
	if len(ttl1) != 1 {
		t.Fatalf("tiny: ttl1 %v", ttl1)
	}
	for i := range ttl1 {
		if segs[i] != [2]int{mid, mid + 1} {
			t.Fatalf("tiny: TTL 1 on %v, want the byte at %d", segs[i], mid)
		}
	}

	// The last segment never gets TTL 1: nothing would come after it.
	st, _ := parseSpec("cut=host+1;ttl1=1,2")
	if _, segs, ttl1, _ := st.plan(h); len(segs) != 2 || ttl1[1] || !ttl1[0] {
		t.Fatalf("segs %v ttl1 %v", segs, ttl1)
	}
	// Positions past the hello are dropped, not a crash.
	st, _ = parseSpec("rec=16000;cut=16000")
	if buf, segs, _, _ := st.plan(h); !bytes.Equal(buf, h) || len(segs) != 1 {
		t.Fatalf("far positions: %d segs", len(segs))
	}
}

// Every strategy over loopback: the server reads the same handshake bytes,
// the OOB byte and the TTL games leave no trace in the stream.
func TestSendDesync_ServerSeesTheHello(t *testing.T) {
	h := clientHello(t, "rr4---sn-abcdef.googlevideo.com")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	list := mustStrategies(t, planStrategies)
	list = append(list, mustStrategies(t, `[{"id":"tiny2","spec":"tiny=2;oob;rec=1,host+3"}]`)...)
	for i := range list {
		st := &list[i]
		t.Run(st.id, func(t *testing.T) {
			got := make(chan []byte, 1)
			go func() {
				c, err := ln.Accept()
				if err != nil {
					got <- nil
					return
				}
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				b, _ := io.ReadAll(c)
				got <- b
			}()
			c, err := net.Dial("tcp", ln.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			desc, err := sendDesync(c.(*net.TCPConn), st, h)
			if err != nil {
				t.Fatal(err)
			}
			c.(*net.TCPConn).CloseWrite()
			b := <-got
			c.Close()
			if !bytes.Equal(unrecord(b), unrecord(h)) {
				t.Fatalf("%s: server got %d bytes, want handshake of %d", desc, len(b), len(h))
			}
			t.Logf("%s", desc)
		})
	}
}

// helloServer accepts once, reads a hello of want handshake bytes and
// then tail, answers with a ServerHello-like record and down more bytes.
func helloServer(t *testing.T, ip string, want int, tail string, down int) (addr string, port uint16, got <-chan []byte) {
	t.Helper()
	ln, err := net.Listen("tcp", net.JoinHostPort(ip, "0"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	ch := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(5 * time.Second))
		var b []byte
		buf := make([]byte, 4096)
		for len(unrecord(b)) < want || !bytes.HasSuffix(b, []byte(tail)) {
			n, err := c.Read(buf)
			b = append(b, buf[:n]...)
			if err != nil {
				break
			}
		}
		ch <- b
		c.Write([]byte{0x16, 3, 3, 0, 2, 2, 0})
		c.Write(make([]byte, down))
	}()
	a := ln.Addr().(*net.TCPAddr)
	return a.String(), uint16(a.Port), ch
}

// Only the hello's own record is cut; what the app sent after it in the
// same peek goes as it was.
func TestDialDesync_TailAfterTheRecord(t *testing.T) {
	withConns(t)
	h := clientHello(t, "www.youtube.com")
	addr, _, got := helloServer(t, "127.0.0.1", len(unrecord(h)), "tail-END", 0)
	st := byID(t, mustStrategies(t, planStrategies), "rec2")
	c, ans, _, err := dialDesync("www.youtube.com", addr, append(append([]byte{}, h...), "tail-END"...), st)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b := <-got
	if ans[0] != 0x16 || !bytes.HasSuffix(b, []byte("tail-END")) || !bytes.Equal(unrecord(b[:len(b)-8]), unrecord(h)) {
		t.Fatalf("server got %q", b)
	}
}

// A server that never answers costs desyncWait, then the caller relays.
func TestDialDesync_SilenceIsAFailure(t *testing.T) {
	withConns(t)
	defer func(w time.Duration) { desyncWait = w }(desyncWait)
	desyncWait = 200 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			io.Copy(io.Discard, c)
		}
	}()
	st := byID(t, mustStrategies(t, planStrategies), "none")
	t0 := time.Now()
	if _, _, _, err := dialDesync("www.youtube.com", ln.Addr().String(), clientHello(t, "www.youtube.com"), st); err == nil {
		t.Fatal("silence passed")
	}
	if d := time.Since(t0); d > time.Second {
		t.Fatalf("waited %s", d)
	}
}

// One strategy, direct on, a fresh picker: what tryDesync is driven with.
func directWith(t *testing.T, js string) *pickState {
	t.Helper()
	p := freshPicker(t)
	p.now = time.Now // tryDesync's start times are real ones
	SetStrategies(js)
	directOn.Store(true)
	t.Cleanup(func() { strategies.Store(nil); directOn.Store(false) })
	p.setNetKey("cell:25099")
	return p
}

// The second segment of a hello arrives late: up to helloWait, then the
// strategy is tried. Not at all within it: relay, no try, nothing blamed.
func TestTryDesync_WaitsForTheHello(t *testing.T) {
	withConns(t)
	captureLog(t)
	p := directWith(t, `[{"id":"rec2","spec":"rec=host+1,midsld;cut=host+1,midsld"}]`)
	h := clientHello(t, "www.youtube.com")
	ip := net.IPv4(127, 0, 0, 1)

	app, peer := net.Pipe()
	defer app.Close()
	defer peer.Close()
	go func() {
		time.Sleep(50 * time.Millisecond)
		peer.Write(h[60:])
		io.ReadFull(peer, make([]byte, 7))
		peer.Close()
	}()
	_, port, got := helloServer(t, "127.0.0.1", len(unrecord(h)), "", 0)
	if _, done := tryDesync(app, h[:60], "www.youtube.com", ip, port, time.Now()); !done {
		t.Fatal("whole hello not tried")
	}
	if b := <-got; !bytes.Equal(unrecord(b), unrecord(h)) {
		t.Fatal("server did not get the hello")
	}

	defer func(w time.Duration) { helloWait = w }(helloWait)
	helloWait = 100 * time.Millisecond
	app2, peer2 := net.Pipe()
	defer app2.Close()
	defer peer2.Close()
	first, done := tryDesync(app2, h[:60], "www.youtube.com", ip, 1, time.Now())
	if done || len(first) != 60 {
		t.Fatalf("incomplete hello: done %v, %d bytes", done, len(first))
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attempts != 1 || p.fails != 0 {
		t.Fatalf("attempts %d fails %d: the cut short one must not count", p.attempts, p.fails)
	}
}

// No connection at all says nothing about the strategy: the hello never
// went out. The address goes by relay for a while.
func TestTryDesync_ConnectFailureIsNotTheStrategys(t *testing.T) {
	withConns(t)
	p := directWith(t, `[{"id":"rec2","spec":"rec=host+1,midsld;cut=host+1,midsld"}]`)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := uint16(ln.Addr().(*net.TCPAddr).Port)
	ln.Close() // refused from now on
	h := clientHello(t, "www.youtube.com")
	for range 3 {
		app, peer := net.Pipe()
		if _, done := tryDesync(app, h, "www.youtube.com", net.IPv4(127, 0, 0, 1), port, time.Now()); done {
			t.Fatal("no connection, yet served")
		}
		app.Close()
		peer.Close()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fails != 0 || p.attempts != 0 || p.cur != "rec2" || !p.silent.has("127.0.0.1") {
		t.Fatalf("fails %d, attempts %d, on %s, silent %v", p.fails, p.attempts, p.cur, p.silent.has("127.0.0.1"))
	}
}

// Stop or a lost network while the hello waits for its answer: our own
// Close, not the strategy's failure.
func TestTryDesync_ClosedWhileWaitingIsNotCounted(t *testing.T) {
	withConns(t)
	p := directWith(t, `[{"id":"rec2","spec":"rec=host+1,midsld;cut=host+1,midsld"}]`)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			io.Copy(io.Discard, c) // never answers
			c.Close()
		}
	}()
	h := clientHello(t, "www.youtube.com")
	app, peer := net.Pipe()
	defer peer.Close()
	done := make(chan bool, 1)
	go func() {
		_, d := tryDesync(app, h, "www.youtube.com", net.IPv4(127, 0, 0, 1), uint16(ln.Addr().(*net.TCPAddr).Port), time.Now())
		done <- d
	}()
	time.Sleep(200 * time.Millisecond)
	NetworkLost()
	select {
	case d := <-done:
		if !d {
			t.Fatal("went on to the relay after our own Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still waiting")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fails != 0 || p.attempts != 0 {
		t.Fatalf("counted: fails %d, attempts %d", p.fails, p.attempts)
	}
}

// Direct bytes cost the VDS nothing: the quota is the relay's.
func TestTryDesync_NotCountedInQuota(t *testing.T) {
	withConns(t)
	directWith(t, `[{"id":"none","spec":""}]`)
	SetQuota(0, 0)
	t.Cleanup(func() { SetQuota(0, 0) })
	h := clientHello(t, "rr1---sn-x.googlevideo.com")
	_, port, _ := helloServer(t, "127.0.0.1", len(unrecord(h)), "", 200<<10)
	app, peer := net.Pipe()
	defer peer.Close()
	go func() {
		io.ReadFull(peer, make([]byte, 7+200<<10))
		peer.Close()
	}()
	if _, done := tryDesync(app, h, "rr1---sn-x.googlevideo.com", net.IPv4(127, 0, 0, 1), port, time.Now()); !done {
		t.Fatal("not direct")
	}
	if u := Usage(); u != 0 {
		t.Fatalf("quota counted %d direct bytes", u)
	}
}

// The list may change under connections in flight (SetStrategies before a
// rebuild's Start): theirs stays theirs, the picker goes by id, not index.
func TestSetStrategies_SwapUnderAConnection(t *testing.T) {
	p := directWith(t, planStrategies)
	st, gen := p.pick("1.1.1.1")
	if st.id != "rec2" {
		t.Fatalf("picked %s", st.id)
	}
	SetStrategies(`[{"id":"new","spec":"cut=1"}]`)
	if st.id != "rec2" || st.norm != "rec=host+1,midsld;cut=host+1,midsld" {
		t.Fatal("the connection's strategy changed under it")
	}
	p.result(gen, st.id, "1.1.1.1", p.now(), false, "x")
	if st2, _ := p.pick("1.1.1.1"); st2 == nil || st2.id != "new" {
		t.Fatalf("after the swap: %v", st2)
	}
}

// The detector's two sides: a throttled stream is steady, so its rate is
// measured; fast chunks with pauses give no whole second, so no rate.
func TestCountConn_PeakOfThrottledAndOfFast(t *testing.T) {
	read := func(write func(a net.Conn)) *countConn {
		a, b := net.Pipe()
		c := &countConn{Conn: b}
		go func() {
			write(a)
			a.Close()
		}()
		buf := make([]byte, 64<<10)
		for {
			if _, err := c.Read(buf); err != nil {
				return c
			}
		}
	}
	slow := read(func(a net.Conn) {
		for range 15 {
			a.Write(make([]byte, 3<<10))
			time.Sleep(100 * time.Millisecond)
		}
	})
	if slow.peak == 0 || slow.peak > slowPeak {
		t.Fatalf("throttled at ~30 KB/s: peak %d KB/s", slow.peak>>10)
	}
	// A 1 MB chunk in ~0.4 s, then a pause: the burst's own rate, ~2.5 MB/s.
	fast := read(func(a net.Conn) {
		for range 2 {
			for range 16 {
				a.Write(make([]byte, 64<<10))
				time.Sleep(25 * time.Millisecond)
			}
			time.Sleep(600 * time.Millisecond)
		}
	})
	if fast.peak < 256<<10 || fast.peak > 4<<20 {
		t.Fatalf("1 MB in 0.4 s: peak %d KB/s", fast.peak>>10)
	}
	// All at once: the socket buffer, not the network.
	instant := read(func(a net.Conn) { a.Write(make([]byte, 1<<20)) })
	if instant.peak != 0 {
		t.Fatalf("one instant burst: peak %d KB/s", instant.peak>>10)
	}
}

// SIOCOUTQNSD is denied to apps: the send queue is read from tcp_info,
// which an old kernel (before 4.6) fills too short to hold it.
func TestWaitSent_TCPInfo(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			io.Copy(io.Discard, c)
			c.Close()
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("hello"))
	if w, why := waitSent(c.(*net.TCPConn)); w != sentDrained {
		t.Fatalf("loopback: %v, %v", w, why)
	}
	var info [232]byte
	info[144] = 7
	if n, err := notSent(info[:]); n != 7 || err != nil {
		t.Fatalf("%d, %v", n, err)
	}
	if _, err := notSent(info[:144]); err == nil {
		t.Fatal("a 4.4 kernel's tcp_info read as having notsent")
	}
}

func TestCountConn_Peak(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	c := &countConn{Conn: b}
	go func() {
		// 1.2 s at ~100 KB/s, a pause, then a short burst that is too short to count.
		for i := 0; i < 12; i++ {
			a.Write(make([]byte, 10<<10))
			time.Sleep(100 * time.Millisecond)
		}
		time.Sleep(700 * time.Millisecond)
		a.Write(make([]byte, 500<<10))
		a.Close()
	}()
	buf := make([]byte, 64<<10)
	for {
		if _, err := c.Read(buf); err != nil {
			break
		}
	}
	if c.peak < 60<<10 || c.peak > 130<<10 {
		t.Fatalf("peak %d KB/s", c.peak>>10)
	}
	if c.down != 620<<10 || !c.serverFirst() {
		t.Fatalf("down %d, server first %v", c.down, c.serverFirst())
	}
}
