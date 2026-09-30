package tunnel

import (
	"net"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/link/fdbased"
)

// One processor per channel, as on a single-CPU phone: fdbased's reader
// delivers packets itself, and a packet that needs a route (an ICMP reply to
// stray UDP) takes the stack's lock that RemoveNIC waits for the reader under.
func TestStop_WhileTunDelivers(t *testing.T) {
	resetDNS()
	t.Cleanup(resetDNS)
	withEndpoint(t, "127.0.0.1:1", "relay.test", "/app/x")
	for i := 0; i < 50; i++ {
		if !stopUnderFlood(t) {
			buf := make([]byte, 1<<20)
			t.Fatalf("run %d: Stop still blocked after 5 s\n%s", i, filterStacks(string(buf[:runtime.Stack(buf, true)])))
		}
	}
}

func stopUnderFlood(t *testing.T) bool {
	// A socket gets fdbased's recvmmsg dispatcher, a phone's TUN readv; both stop the same way (eventfd + ppoll).
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET, 0)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := fdbased.New(&fdbased.Options{FDs: []int{fds[0]}, MTU: 1500, ProcessorsPerChannel: 1})
	if err != nil {
		t.Fatal(err)
	}
	ns, err := newStack(ep, testCred)
	if err != nil {
		t.Fatal(err)
	}
	// For -race only: newStack installs its handlers after CreateNIC started
	// the reader; the stack's lock, taken per packet, orders them.
	ns.RegisteredEndpoints()
	mu.Lock()
	setConns(make(map[net.Conn]struct{}))
	tunFile = os.NewFile(uintptr(fds[0]), "tun")
	s = ns
	linkEP = ep
	mu.Unlock()

	// No listener for it: the stack answers ICMP.
	quic := udpPkt(tunIP, tcpip.AddrFrom4([4]byte{142, 250, 1, 1}), 40000, 443, make([]byte, 1200))
	var stop atomic.Bool
	flooding := make(chan struct{})
	go func() {
		defer close(flooding)
		for !stop.Load() {
			if _, err := syscall.Write(fds[1], quic); err != nil {
				return
			}
		}
	}()
	time.Sleep(20 * time.Millisecond)

	done := make(chan struct{})
	go func() { Stop(); close(done) }()
	// Not for good: the reader only checks for stop once the TUN is drained.
	time.Sleep(50 * time.Millisecond)
	stop.Store(true)
	var ok bool
	select {
	case <-done:
		ok = true
		<-flooding
		syscall.Close(fds[1])
	case <-time.After(5 * time.Second):
	}
	return ok
}

// filterStacks keeps the goroutines of the stop path and the TUN reader.
func filterStacks(all string) string {
	var keep []string
	for _, g := range strings.Split(all, "\n\n") {
		if strings.Contains(g, "tunnel.Stop") || strings.Contains(g, "fdbased") {
			keep = append(keep, g)
		}
	}
	return strings.Join(keep, "\n\n")
}
