//go:build heavy

package tunnel

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
)

// Heavy check of the daily quota counter against real traffic: an "app"
// gVisor stack stands in for the phone's apps, its packets reach the
// tunnel's own stack (newStack) over linked channel endpoints, handleTCP
// dials the fake relay, the relay sinks, sources or echoes.
//
//	go test -tags heavy -run TestHeavy_QuotaMatchesTraffic -v ./...
//
// HEAVY_MB (default 100): the traffic both ways together.
// HEAVY_STEP_MB (default: quotaStep as is): QuotaProgress step.

const (
	heavyUpload = iota
	heavyDownload
	heavyEcho

	heavyCtlSize = 16
	heavyQueue   = 1024
	heavyMTU     = 1500
	// io.Copy's buffer in relay(): what the tunnel may have written to the
	// relay before it counts it.
	heavyCopyBuf = 32 << 10
)

var heavyChunks = []int{1, 7, 512, 1400, 1460, 4096, 16384, 65536, 262144}

type heavyCounters struct {
	appSent, appRecv     atomic.Int64 // sent: before Write, recv: after Read
	relaySent, relayRecv atomic.Int64 // the same order on the relay side
	hdrs                 atomic.Int64
}

type heavyResult struct {
	mu   sync.Mutex
	sums map[byte][32]byte // download: what the relay sent
}

func envInt(t *testing.T, name string, def int64) int64 {
	v := os.Getenv(name)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		t.Fatalf("%s=%q", name, v)
	}
	return n
}

func chunk(rng *rand.Rand, left int64) int {
	n := int64(heavyChunks[rng.IntN(len(heavyChunks))])
	return int(min(n, left))
}

// heavyRelay serves every session: header, a 16-byte control from the app
// ([0] mode, [1] id, [8:16] size), then the mode's traffic, then close.
func heavyRelay(t *testing.T, cnt *heavyCounters, res *heavyResult) {
	ln := fakeTLSListener(t)
	withEndpoint(t, ln.Addr().String(), "relay.test", "/app/x")
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go heavyServe(c, cnt, res)
		}
	}()
}

func heavyServe(c net.Conn, cnt *heavyCounters, res *heavyResult) {
	defer c.Close()
	br := bufio.NewReader(c)
	if _, err := http.ReadRequest(br); err != nil {
		return
	}
	io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	var hdr [7]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return
	}
	cnt.hdrs.Add(1)
	var ctl [heavyCtlSize]byte
	if _, err := io.ReadFull(br, ctl[:]); err != nil {
		return
	}
	cnt.relayRecv.Add(heavyCtlSize)
	mode, id, size := ctl[0], ctl[1], int64(binary.BigEndian.Uint64(ctl[8:]))
	rng := rand.New(rand.NewPCG(uint64(id), 2))
	write := func(p []byte) error {
		cnt.relaySent.Add(int64(len(p)))
		_, err := c.Write(p)
		return err
	}
	buf := make([]byte, heavyChunks[len(heavyChunks)-1])
	switch mode {
	case heavyUpload:
		h := sha256.New()
		for left := size; left > 0; {
			n, err := br.Read(buf[:chunk(rng, left)])
			cnt.relayRecv.Add(int64(n))
			h.Write(buf[:n])
			left -= int64(n)
			if err != nil {
				return
			}
		}
		write(h.Sum(nil))
	case heavyDownload:
		h := sha256.New()
		src := rand.NewChaCha8([32]byte{id})
		for left := size; left > 0; {
			p := buf[:chunk(rng, left)]
			src.Read(p)
			h.Write(p)
			if write(p) != nil {
				return
			}
			left -= int64(len(p))
		}
		res.mu.Lock()
		res.sums[id] = [32]byte(h.Sum(nil))
		res.mu.Unlock()
	case heavyEcho:
		for left := size; left > 0; {
			n, err := br.Read(buf[:chunk(rng, left)])
			cnt.relayRecv.Add(int64(n))
			if n > 0 && write(buf[:n]) != nil {
				return
			}
			left -= int64(n)
			if err != nil {
				return
			}
		}
	}
}

// heavyLink moves packets one way between the two stacks' endpoints.
func heavyLink(ctx context.Context, from, to *channel.Endpoint) {
	for {
		pkt := from.ReadContext(ctx)
		if pkt == nil {
			return
		}
		v := pkt.ToView()
		inject(to, v.AsSlice())
		v.Release()
		pkt.DecRef()
	}
}

func heavyAppStack(t *testing.T, ep *channel.Endpoint) *stack.Stack {
	ns := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol},
	})
	if err := ns.CreateNIC(1, ep); err != nil {
		t.Fatalf("app nic: %v", err)
	}
	if err := ns.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tunIP.WithPrefix()}, stack.AddressProperties{}); err != nil {
		t.Fatalf("app addr: %v", err)
	}
	ns.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	return ns
}

type heavyConn struct {
	mode byte
	size int64 // one way
}

// heavyApp runs one app session and returns the hash check's verdict.
func heavyApp(ctx context.Context, ns *stack.Stack, id byte, hc heavyConn, cnt *heavyCounters, res *heavyResult) error {
	c, err := gonet.DialContextTCP(ctx, ns, tcpip.FullAddress{NIC: 1, Addr: tgIP, Port: 443 + uint16(id)}, ipv4.ProtocolNumber)
	if err != nil {
		return fmt.Errorf("dial: %v", err)
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	rng := rand.New(rand.NewPCG(uint64(id), 1))
	write := func(p []byte) error {
		cnt.appSent.Add(int64(len(p)))
		_, err := c.Write(p)
		return err
	}
	var ctl [heavyCtlSize]byte
	ctl[0], ctl[1] = hc.mode, id
	binary.BigEndian.PutUint64(ctl[8:], uint64(hc.size))
	if err := write(ctl[:]); err != nil {
		return fmt.Errorf("control: %v", err)
	}

	sent := sha256.New()
	send := func() error {
		src := rand.NewChaCha8([32]byte{id, 1})
		buf := make([]byte, heavyChunks[len(heavyChunks)-1])
		for left := hc.size; left > 0; {
			p := buf[:chunk(rng, left)]
			src.Read(p)
			sent.Write(p)
			if err := write(p); err != nil {
				return fmt.Errorf("write: %v", err)
			}
			left -= int64(len(p))
		}
		return nil
	}
	// Reads to EOF with its own sizes: the relay closes when it is done.
	recv := func(h io.Writer) (int64, error) {
		rr := rand.New(rand.NewPCG(uint64(id), 3))
		buf := make([]byte, heavyChunks[len(heavyChunks)-1])
		var total int64
		for {
			n, err := c.Read(buf[:heavyChunks[rr.IntN(len(heavyChunks))]])
			cnt.appRecv.Add(int64(n))
			h.Write(buf[:n])
			total += int64(n)
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			if err != nil {
				return total, fmt.Errorf("read: %v", err)
			}
		}
	}

	got := sha256.New()
	switch hc.mode {
	case heavyUpload:
		if err := send(); err != nil {
			return err
		}
		// The relay answers with its hash of what it received.
		var reply bytes.Buffer
		if _, err := recv(&reply); err != nil {
			return err
		}
		if !bytes.Equal(reply.Bytes(), sent.Sum(nil)) {
			return fmt.Errorf("upload: relay hash %x, sent %x", reply.Bytes(), sent.Sum(nil))
		}
		return nil
	case heavyDownload:
		n, err := recv(got)
		if err != nil {
			return err
		}
		if n != hc.size {
			return fmt.Errorf("download: %d of %d bytes", n, hc.size)
		}
		res.mu.Lock()
		want := res.sums[id]
		res.mu.Unlock()
		if [32]byte(got.Sum(nil)) != want {
			return errors.New("download: hash mismatch")
		}
		return nil
	default:
		errc := make(chan error, 1)
		go func() { errc <- send() }()
		n, err := recv(got)
		if werr := <-errc; werr != nil {
			return werr
		}
		if err != nil {
			return err
		}
		if n != hc.size || !bytes.Equal(got.Sum(nil), sent.Sum(nil)) {
			return fmt.Errorf("echo: %d of %d bytes back, hash equal %v", n, hc.size, bytes.Equal(got.Sum(nil), sent.Sum(nil)))
		}
		return nil
	}
}

type heavyRow struct {
	app, usage, lo, hi int64
}

func mb(n int64) string { return fmt.Sprintf("%.2f", float64(n)/(1<<20)) }

func TestHeavy_QuotaMatchesTraffic(t *testing.T) {
	total := envInt(t, "HEAVY_MB", 100) << 20
	if step := envInt(t, "HEAVY_STEP_MB", 0); step > 0 {
		old := quotaStep
		quotaStep = step << 20
		t.Cleanup(func() { quotaStep = old; SetQuota(0, 0) })
	}
	// 30% up, 30% down, two echoes of 10% each way.
	conns := []heavyConn{
		{heavyUpload, total * 3 / 10},
		{heavyDownload, total * 3 / 10},
		{heavyEcho, total / 10},
		{heavyEcho, total / 10},
	}

	cnt := &heavyCounters{}
	res := &heavyResult{sums: map[byte][32]byte{}}
	heavyRelay(t, cnt, res)
	withConns(t)
	fp := withQuota(t, 0, 0)

	tunEP := channel.New(heavyQueue, heavyMTU, "")
	ns, err := newStack(tunEP, testCred)
	if err != nil {
		t.Fatal(err)
	}
	appEP := channel.New(heavyQueue, heavyMTU, "")
	app := heavyAppStack(t, appEP)
	linkCtx, stopLink := context.WithCancel(context.Background())
	go heavyLink(linkCtx, appEP, tunEP)
	go heavyLink(linkCtx, tunEP, appEP)
	t.Cleanup(func() {
		stopLink()
		app.Close()
		ns.Close()
		appEP.Close()
		tunEP.Close()
	})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	// Checkpoints every 5 MB of app traffic. lo..hi is where Usage must
	// be at that moment: at least what the relay has got from the tunnel
	// and what the app has got from it, at most what the app has handed
	// to its stack and what the relay has sent. The gap between them is
	// data in flight: socket buffers of both stacks, the link queues and
	// io.Copy's buffer. lo gets one io.Copy buffer per session of slack:
	// the relay may read a write before the tunnel's Write returns and counts.
	var rows []heavyRow
	done := make(chan struct{})
	var sampled sync.WaitGroup
	sampled.Add(1)
	go func() {
		defer sampled.Done()
		next := int64(5 << 20)
		for {
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
			}
			if cnt.appSent.Load()+cnt.appRecv.Load() < next {
				continue
			}
			lo := 7*cnt.hdrs.Load() + cnt.relayRecv.Load() + cnt.appRecv.Load()
			u := Usage()
			appSent, appRecv := cnt.appSent.Load(), cnt.appRecv.Load()
			hi := 7*int64(len(conns)) + appSent + cnt.relaySent.Load()
			rows = append(rows, heavyRow{appSent + appRecv, u, lo, hi})
			for next <= appSent+appRecv {
				next += 5 << 20
			}
		}
	}()

	start := time.Now()
	errs := make([]error, len(conns))
	var wg sync.WaitGroup
	for i, hc := range conns {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = heavyApp(ctx, app, byte(i), hc, cnt, res)
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(done)
	sampled.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("session %d (mode %d): %v", i, conns[i].mode, err)
		}
	}

	// handleTCP counts the last bytes after the app has them: wait for it.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		connsMu.Lock()
		left := len(activeConns)
		connsMu.Unlock()
		if left == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	slack := int64(len(conns)) * heavyCopyBuf
	// However much the sandwich lets through, in flight cannot exceed both
	// stacks' socket buffers (autotuned up to tcp.MaxBufferSize), io.Copy's
	// buffer and the link queues.
	inflightCap := int64(len(conns))*(2*tcp.MaxBufferSize+heavyCopyBuf) + 2*heavyQueue*heavyMTU
	var worstAbs int64
	t.Logf("%4s %10s %10s %10s %8s %10s %10s", "#", "app MB", "usage MB", "diff B", "diff %", "lo MB", "hi MB")
	var worst float64
	for i, r := range rows {
		d := r.usage - r.app
		pct := 100 * float64(d) / float64(r.app)
		worst = max(worst, abs(pct))
		worstAbs = max(worstAbs, d, -d)
		if d > inflightCap || -d > inflightCap {
			t.Errorf("checkpoint %d: usage off by %d, more than can be in flight (%d)", i+1, d, inflightCap)
		}
		t.Logf("%4d %10s %10s %10d %+8.3f %10s %10s", i+1, mb(r.app), mb(r.usage), d, pct, mb(r.lo), mb(r.hi))
		if r.usage < r.lo-slack || r.usage > r.hi {
			t.Errorf("checkpoint %d: usage %d outside [%d-%d, %d]", i+1, r.usage, r.lo, slack, r.hi)
		}
	}

	appData := cnt.appSent.Load() + cnt.appRecv.Load()
	u := Usage()
	want := appData + 7*int64(len(conns))
	t.Logf("final: app %d B (sent %d, recv %d), usage %d B, want app+7×%d = %d, diff %d B (%+.6f%%)",
		appData, cnt.appSent.Load(), cnt.appRecv.Load(), u, len(conns), want, u-want, 100*float64(u-appData)/float64(appData))
	t.Logf("relay side: got %d B, sent %d B, headers %d", cnt.relayRecv.Load(), cnt.relaySent.Load(), cnt.hdrs.Load())
	if u != want {
		t.Errorf("final usage %d, want %d", u, want)
	}
	if cnt.relayRecv.Load() != cnt.appSent.Load() || cnt.relaySent.Load() != cnt.appRecv.Load() {
		t.Errorf("relay got %d/sent %d, app sent %d/got %d", cnt.relayRecv.Load(), cnt.relaySent.Load(), cnt.appSent.Load(), cnt.appRecv.Load())
	}

	wantSteps := int32(u / quotaStep)
	deadline = time.Now().Add(2 * time.Second)
	for fp.steps.Load() < wantSteps && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	t.Logf("QuotaProgress: %d calls, usage/step = %d/%d = %d", fp.steps.Load(), u, quotaStep, wantSteps)
	if fp.steps.Load() != wantSteps {
		t.Errorf("QuotaProgress %d times, want %d", fp.steps.Load(), wantSteps)
	}
	if fp.quota.Load() != 0 {
		t.Errorf("QuotaExceeded with no limit")
	}
	t.Logf("checkpoints %d, worst diff %.3f%% / %s MB (in-flight cap %s MB), time %s, %.1f MB/s of app traffic",
		len(rows), worst, mb(worstAbs), mb(inflightCap), elapsed.Round(time.Millisecond), float64(appData)/(1<<20)/elapsed.Seconds())
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
