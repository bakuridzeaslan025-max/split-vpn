package tunnel

import (
	"context"
	"errors"
	"log"
	"net"
	"strings"
	"sync"
	"time"
)

// Relay health. An outage is one story in the log instead of thousands of
// identical lines that push its start out of the rotated file, and once it
// is certain, apps get an instant refusal instead of a 10 s dial that keeps
// the radio awake. Memory only: losing the state (process death) costs at
// most one more full error line.
const (
	breakerAfter  = 5                // dials failed in a row on this network before apps are refused...
	breakerDelay  = 15 * time.Second // ...and for this long: a burst of parallel dials in a short blip is not an outage
	probeInterval = 30 * time.Second // while refusing, one real dial per interval probes the relay
	probeMax      = 5 * time.Minute  // the prober's own dials back off from probeInterval up to this
	summaryEvery  = 5 * time.Minute
	verdictAfter  = 3 // probes failed in a row before a new endpoint is called down: a lost hello is common on phones
)

var verdictRetry = 5 * time.Second // between those probes; a var for tests

var errRelayDown = errors.New("relay down")

type relayHealth struct {
	mu        sync.Mutex
	now       func() time.Time
	gen       uint64 // bumped on a new network: dials from the gone one don't count
	fails     int    // dials failed since the last success
	refused   int
	since     time.Time
	netFails  int             // as fails and since, but on this network: a fresh one gets
	netSince  time.Time       // its own chance before refusals start again
	kinds     map[string]bool // logged in full since the last new network
	open      bool
	pending   bool // something happened since loggedAt
	loggedAt  time.Time
	nextProbe time.Time
	host      Host
	known     bool // host heard RelayDown since setHost...
	told      bool // ...with this value
	probeSoon bool
	wake      chan struct{} // nudges this tunnel's prober: state changed or probeSoon
	newFails  int           // probes failed while the host waits for a verdict (!known)
}

var health = relayHealth{now: time.Now}

func (h *relayHealth) generation() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gen
}

func (h *relayHealth) down() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.fails > 0
}

func (h *relayHealth) allow() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.open {
		return nil
	}
	t := h.now()
	if t.Before(h.nextProbe) {
		h.refused++
		h.pending = true
		h.summaryLocked(t)
		return errRelayDown
	}
	h.nextProbe = t.Add(probeInterval)
	return nil
}

// failed records a dial to the VDS that did not get through; line is the
// full error, written the first time its kind shows up on this network.
func (h *relayHealth) failed(gen uint64, stage, line string, err error) {
	if errors.Is(err, context.Canceled) {
		return // the caller gave up, the relay said nothing
	}
	if gen == notCounted {
		log.Print(line)
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if gen != h.gen {
		return
	}
	t := h.now()
	h.fails++
	if h.fails == 1 {
		h.since = t
	}
	h.netFails++
	if h.netFails == 1 {
		h.netSince = t
	}
	if kind := stage + ": " + errKind(err); !h.kinds[kind] {
		if h.kinds == nil {
			h.kinds = map[string]bool{}
		}
		h.kinds[kind] = true
		log.Print(line)
		h.loggedAt = t
	} else {
		h.pending = true
	}
	if !h.open && h.netFails >= breakerAfter && t.Sub(h.netSince) >= breakerDelay {
		h.open = true
		h.nextProbe = t.Add(probeInterval)
		log.Printf("relay: %d dials failed in %s, refusing app conns, probe every %s", h.netFails, t.Sub(h.netSince).Round(time.Second), probeInterval)
		h.tellLocked(true)
	}
	h.summaryLocked(t)
}

// ok records a session the relay accepted.
func (h *relayHealth) ok(gen uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if gen != h.gen {
		return
	}
	// open with no failure: a suspect endpoint that answered at once, nothing came back.
	if h.fails > 1 || h.open && h.fails > 0 {
		log.Printf("relay: back after %s, %d dials failed, %d app conns refused", h.now().Sub(h.since).Round(time.Second), h.fails, h.refused)
	}
	h.tellLocked(false)
	h.fails, h.refused, h.pending = 0, 0, false
	h.netFails, h.kinds, h.open = 0, nil, false
}

// unreachableAtStart: the tunnel comes up anyway and heals itself, like
// after any outage, instead of failing the start and waiting for a tap.
func (h *relayHealth) unreachableAtStart() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unreachableLocked("at start, tunnel up anyway")
}

// unreachable is the prober's failed probe of a relay the host has heard
// nothing about yet (a new endpoint); verdictAfter of them make the verdict.
// Once the host knows, failures are only counted.
func (h *relayHealth) unreachable(gen uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if gen != h.gen || h.known {
		return
	}
	if h.newFails++; h.newFails >= verdictAfter {
		h.unreachableLocked("")
	}
}

func (h *relayHealth) unreachableLocked(when string) {
	t := h.now()
	if h.fails == 0 {
		h.fails, h.since = 1, t
	}
	h.open = true
	h.nextProbe = t.Add(probeInterval)
	if when != "" {
		when = " " + when
	}
	log.Printf("relay: unreachable%s, refusing app conns, probe every %s", when, probeInterval)
	h.tellLocked(true)
}

// setHost starts a tunnel's reports afresh: its first verdict always
// reaches the host, which may still hold the previous tunnel's.
// It returns the new tunnel's wake channel: one per prober, so a stopped
// one still finishing its probe cannot take a nudge meant for the next.
func (h *relayHealth) setHost(host Host) <-chan struct{} {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.host, h.known, h.newFails = host, false, 0
	h.wake = make(chan struct{}, 1)
	return h.wake
}

func (h *relayHealth) tellLocked(down bool) {
	if h.known && h.told == down {
		return
	}
	h.known, h.told = true, down
	if h.host != nil {
		h.host.RelayDown(down)
	}
	h.wakeLocked()
}

func (h *relayHealth) wakeLocked() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

// prober dials the relay itself while it is down: apps may have nothing to
// send (phone in a pocket), and the warning would stay up after the
// network is back. Right away on a new network, then backing off.
func (h *relayHealth) prober(stop, wake <-chan struct{}, probe func() error) {
	defer guard("prober")
	delay := probeInterval
	timer := time.NewTimer(delay)
	timer.Stop()
	for {
		select {
		case <-stop:
			return
		default:
		}
		h.mu.Lock()
		// !known: a new endpoint, or a verdict dropped by a network change;
		// the host waits for one either way.
		down, retry := h.known && h.told, !h.known && h.newFails > 0
		soon := h.probeSoon || !h.known && !retry
		h.probeSoon = false
		h.mu.Unlock()
		switch {
		case soon:
			delay = probeInterval
		case !down && !retry:
			delay = probeInterval
			select {
			case <-stop:
				return
			case <-wake:
			}
			continue
		default:
			wait := delay
			if retry {
				wait = verdictRetry
			}
			timer.Reset(wait)
			select {
			case <-stop:
				timer.Stop()
				return
			case <-wake:
				timer.Stop()
				continue
			case <-timer.C:
			}
			if !retry {
				delay = min(2*delay, probeMax)
			}
		}
		gen := h.generation()
		// A 404 means the relay is back and only the credential is not:
		// apps will see that 404 themselves, and the service re-registers.
		if err := probe(); err == nil || errors.Is(err, ErrCredRejected) {
			h.ok(gen)
		} else {
			h.unreachable(gen)
		}
	}
}

// newNetwork: dials from the gone network stop counting and error kinds are
// logged afresh. The outage itself goes on (start and counts stay until the
// relay answers or the tunnel stops), and so do the refusals: the prober
// tries the new network at once, and a network that does not help would
// otherwise cost every app a 10 s dial.
func (h *relayHealth) newNetwork() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.newNetworkLocked()
}

func (h *relayHealth) newNetworkLocked() {
	h.gen++
	h.netFails, h.kinds, h.newFails = 0, nil, 0
	if h.open {
		h.probeSoon = true
		h.wakeLocked()
	}
}

func (h *relayHealth) stopped() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resetLocked("tunnel stopped")
	h.host = nil
}

// newEndpoint drops the old relay's story: its dials in flight stop
// counting, the breaker closes, and the prober (woken here) gets the new
// relay a verdict of its own for the host. A suspect one keeps the breaker
// open until the prober's first success: its probes do not ask allow().
func (h *relayHealth) newEndpoint(suspect bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.resetLocked("endpoint switched")
	if suspect {
		h.open, h.since = true, h.now()
		h.nextProbe = h.since.Add(probeInterval)
	}
	h.wakeLocked()
}

func (h *relayHealth) resetLocked(why string) {
	if h.fails > 1 {
		log.Printf("relay: down for %s, %d dials failed, %d app conns refused, %s", h.now().Sub(h.since).Round(time.Second), h.fails, h.refused, why)
	}
	h.known, h.probeSoon, h.newFails = false, false, 0
	h.fails, h.refused, h.pending = 0, 0, false
	h.gen++
	h.netFails, h.kinds, h.open = 0, nil, false
}

func (h *relayHealth) summaryLocked(t time.Time) {
	if !h.pending || t.Sub(h.loggedAt) < summaryEvery {
		return
	}
	log.Printf("relay: down for %s, %d dials failed, %d app conns refused", t.Sub(h.since).Round(time.Second), h.fails, h.refused)
	h.loggedAt = t
	h.pending = false
}

// errKind drops addresses and ports, and folds every timeout into one:
// "read tcp a:1->b:2: i/o timeout", "dial tcp b:2: i/o timeout" and
// "context deadline exceeded" are the same silence.
func errKind(err error) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	s := err.Error()
	if i := strings.LastIndex(s, ": "); i >= 0 {
		s = s[i+2:]
	}
	return s
}
