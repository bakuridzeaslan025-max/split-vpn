package tunnel

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Which strategy works is picked on live YouTube traffic, per network: the
// current one is tried, its failures move on to the next. Winners,
// "nothing works" and dead addresses are kept per network in
// files/desync.json; the rest is memory only. Stop and Start keep it all (a TUN rebuild is a Stop+Start
// every 10 min at most); only another network starts afresh.
const (
	needFails    = 2                // counted failures before a strategy moves on...
	needFailsWon = 3                // ...and before this network's winner does
	silentFor    = 30 * time.Minute // an address the winner does not get through to goes by relay
	deadFor      = 24 * time.Hour   // its connections refused on this network, kept in the map
	deadMax      = 16               // dead addresses kept per network
	deadRecheck  = 15 * time.Minute // a dead address the player asks for is probed again at most this often
	noneFor      = 7 * 24 * time.Hour
	addrMemory   = 64 // addresses kept per network: googlevideo's change with every video
	mapMax       = 64

	// A session that got its ServerHello and was closed by the server at
	// once with almost nothing down, after the app asked for something
	// (more than its Finished): DPI cut it after the handshake.
	earlyDeath     = 3 * time.Second
	earlyDeathDown = 4 << 10
	earlyDeathUp   = 300

	// Throttled after the handshake: slowRun videos in a row, each over
	// slowMin, none faster than slowPeak at its best (below a normal 360p,
	// above the 2024 slowdown).
	slowPeak = 40 << 10
	slowMin  = 512 << 10
	slowRun  = 3

	failLogGap = 2 * time.Second // a dead direct path fails every connection: one line per gap, the rest in the tally
)

// Verdicts for the host, about the current network.
const (
	verdictUnknown = "unknown" // off, no strategies, or no direct try yet
	verdictTesting = "testing"
	verdictWorks   = "works"
	verdictFails   = "fails"
)

var directOn atomic.Bool

// SetDirect turns direct YouTube on or off; read at Start, like SetAdBlock.
func SetDirect(on bool) { directOn.Store(on) }

// SetNetKey names the system's default network: "cell:<MCC-MNC>" (a
// carrier), "wifi@<id>" (a Wi-Fi: Go asks a public service for its
// provider and keys it "wifi:AS<n>"), anything else is a network without
// a name, picked for in memory only. The same key again changes nothing.
func SetNetKey(key string) { picker.setNetKey(key) }

// addrs remembers up to addrMemory addresses, dropping the least recently
// put.
type addrs struct {
	m    map[string]addrEntry
	tick uint64
}

type addrEntry struct {
	used  uint64
	until time.Time
}

func (a *addrs) put(ip string, until time.Time) {
	if a.m == nil {
		a.m = map[string]addrEntry{}
	}
	a.tick++
	a.m[ip] = addrEntry{a.tick, until}
	if len(a.m) <= addrMemory {
		return
	}
	oldest, at := "", ^uint64(0)
	for k, e := range a.m {
		if e.used < at {
			oldest, at = k, e.used
		}
	}
	delete(a.m, oldest)
}

func (a *addrs) get(ip string) (time.Time, bool) {
	e, ok := a.m[ip]
	return e.until, ok
}

func (a *addrs) has(ip string) bool { _, ok := a.m[ip]; return ok }
func (a *addrs) del(ip string)      { delete(a.m, ip) }
func (a *addrs) len() int           { return len(a.m) }

// addrInfo is what the picker knows of one address on this network.
type addrInfo struct {
	hello      bool      // a real direct try got its ServerHello since the relay last proved useless to it
	curOK      bool      // cur got through to it
	noHello    bool      // the last direct try got no ServerHello: its relay sessions are watched
	relayUntil time.Time // pick sends it by relay until then
	muteUntil  time.Time // the relay was silent to it with the VDS alive, direct answered: direct only
	deadUntil  time.Time // its connections refused until then
	checked    time.Time // a dead one's last probe, see recheckDue
	failed     int       // failures counted against cur, forgiven if it turns dead
	used       uint64
}

// addrLocked is ip's entry, nil if none. A dead one past its time is
// dropped here.
func (p *pickState) addrLocked(ip string) *addrInfo {
	a := p.ips[ip]
	if a != nil && !a.deadUntil.IsZero() && !p.now().Before(a.deadUntil) {
		delete(p.ips, ip)
		return nil
	}
	return a
}

// putLocked is ip's entry, a new one if none. addrMemory of them are kept
// besides the dead ones: the least recently used of those with nothing
// pending goes first, then the least recently used of the rest. The dead
// are never dropped for room.
func (p *pickState) putLocked(ip string) *addrInfo {
	if a := p.addrLocked(ip); a != nil {
		return a
	}
	if p.ips == nil {
		p.ips = map[string]*addrInfo{}
	}
	p.tick++
	a := &addrInfo{used: p.tick}
	p.ips[ip] = a
	now := p.now()
	n := 0
	for _, e := range p.ips {
		if !now.Before(e.deadUntil) {
			n++
		}
	}
	if n <= addrMemory {
		return a
	}
	drop, dropIdle := "", false
	for k, e := range p.ips {
		if k == ip || now.Before(e.deadUntil) {
			continue
		}
		idle := !now.Before(e.relayUntil) && !now.Before(e.muteUntil)
		if drop == "" || idle && !dropIdle || idle == dropIdle && e.used < p.ips[drop].used {
			drop, dropIdle = k, idle
		}
	}
	delete(p.ips, drop)
	return a
}

func (p *pickState) usedLocked(a *addrInfo) {
	p.tick++
	a.used = p.tick
}

// clearLocked forgets the failures counted against cur, and with curOK
// where it got through: a new strategy starts from nothing.
func (p *pickState) clearLocked(curOK bool) {
	for _, a := range p.ips {
		a.failed = 0
		if curOK {
			a.curOK = false
		}
	}
}

func (p *pickState) okCountLocked() int {
	n := 0
	for _, a := range p.ips {
		if a.curOK {
			n++
		}
	}
	return n
}

type netEntry struct {
	Win  string `json:"win,omitempty"`
	None bool   `json:"none,omitempty"`
	At   int64  `json:"at"`
	// Dead: ip → unix time its connections are refused until, see markDead.
	// Not part of At: a dead address does not extend a week of None.
	Dead map[string]int64 `json:"dead,omitempty"`
}

type netMapFile struct {
	V    int                 `json:"v"`
	Nets map[string]netEntry `json:"nets"`
}

type pickState struct {
	mu  sync.Mutex
	now func() time.Time

	raw      string // as SetNetKey said it
	key      string // in the map; "" for a network without a name
	gen      uint64 // results from an earlier network do not count
	asnTried bool
	asns     map[string]string // "wifi@<id>" → its ASN, for the process: a restart does not ask again

	cur       string // strategy id
	won       bool   // cur is this network's winner, from the map or found now
	passed    bool   // cur got through here at least once
	confirmed bool   // found now: got through to two addresses
	fails     int
	lastFail  time.Time // connections started before it are the same burst
	tried     int       // strategies failed in a row
	ips       map[string]*addrInfo
	tick      uint64 // addrInfo.used
	noneUntil time.Time
	slow      int
	attempts  int
	lastWin   string // any network's latest winner: where a nameless one starts

	// googlevideo clusters (videoCluster) whose new addresses race the
	// relay at once: two of their addresses silent now, none got through
	// on this network. Only another network forgets them: a strategy's
	// change says nothing about the nodes.
	clusterFirst  map[string]string // cluster → its first silent address
	clusterSilent addrs             // cluster → racing at once until
	okClusters    addrs             // clusters an address of which got through

	// What the release log says about this network's direct tries:
	// counts, not a line per connection.
	tally       tally
	summarized  tally
	okSinceFail int
	lastFailLog time.Time

	file   string
	loaded bool
	nets   map[string]netEntry

	tellMu sync.Mutex // the host hears verdicts in order
	told   string
}

type tally struct {
	ok, failed, skipped int
	empty               int    // got the ServerHello, asked, got next to nothing
	lastEmpty           string // how the latest of those ended
	// Relay legs started, sessions they served, and direct answers that
	// came after the relay took the session: those are in ok too.
	raced, relayWon, late int
}

var picker = &pickState{now: time.Now}

// deadWait: a watched address's relay session with no byte back this long
// says the relay does not reach it, see relayNoAnswer. A var for the tests.
var deadWait = 4 * time.Second

// started: Start's turn. The map is read once per process (Stop and Start
// keep the memory), the new host hears the verdict afresh.
func (p *pickState) started(file string) {
	p.mu.Lock()
	if !p.loaded || file != p.file {
		p.file, p.loaded = file, true
		p.nets = loadNetMap(file)
		p.lastWin = ""
		var at int64
		for _, e := range p.nets {
			if e.Win != "" && e.At > at {
				p.lastWin, at = e.Win, e.At
			}
		}
		p.applyLocked()
	}
	// SetStrategies before this Start may have brought a list without the
	// current one (or a list after an empty one: the map's winner waits).
	if list := strategiesNow(); len(list) > 0 && !hasID(list, p.cur) {
		p.restartLocked()
	}
	lookup, gen, raw := p.wantASNLocked(), p.gen, p.raw
	p.told = ""
	p.unlockAndTell()
	if lookup {
		go p.lookupASN(gen, raw)
	}
}

func (p *pickState) setNetKey(raw string) {
	p.mu.Lock()
	if raw == p.raw {
		p.mu.Unlock()
		return
	}
	key := ""
	if code, ok := strings.CutPrefix(raw, "cell:"); ok && code != "" {
		key = raw
	}
	if asn, ok := p.asns[raw]; ok {
		key = "wifi:AS" + asn
	}
	p.raw, p.asnTried = raw, false
	// The same network under another id (a Wi-Fi joined again): its memory stays.
	if key != "" && key == p.key {
		p.unlockAndTell()
		return
	}
	was := p.key
	p.summaryLocked()
	p.key = key
	p.resetLocked()
	p.applyLocked()
	// A nameless one after a nameless one is no news for the log.
	if directOn.Load() && len(strategiesNow()) > 0 && (key != "" || was != "") {
		log.Printf("sni desync: network %s, starting from %s%s", p.nameLocked(), p.cur, p.noneNoteLocked())
	}
	lookup, gen := p.wantASNLocked(), p.gen
	p.unlockAndTell()
	if lookup {
		go p.lookupASN(gen, raw)
	}
}

func (p *pickState) noneNoteLocked() string {
	if p.noneUntil.IsZero() {
		return ""
	}
	return " (nothing works until " + p.noneUntil.Format("2006-01-02") + ")"
}

// summaryLocked logs the tally of the network being left (or of the
// tunnel being stopped) if it changed since the last summary.
func (p *pickState) summaryLocked() {
	t := p.tally
	if t == p.summarized {
		return
	}
	p.summarized = t
	empty := ""
	if t.empty > 0 {
		empty = fmt.Sprintf(" (%d empty, last: %s)", t.empty, t.lastEmpty)
	}
	log.Printf("sni desync: on %s: %d direct ok%s, %d failed, %d straight to relay, %d raced (%d relay won, %d direct late)",
		p.nameLocked(), t.ok, empty, t.failed, t.skipped, t.raced, t.relayWon, t.late)
}

// countRace tallies a race of this network.
func (p *pickState) countRace(gen uint64, count func(t *tally)) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen == p.gen {
		count(&p.tally)
	}
}

// stopped: Stop's turn. The tally goes on (Start keeps the memory), the
// log hears it so far.
func (p *pickState) stopped() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.summaryLocked()
}

// countLocked tallies a direct try. A failure says how many got through
// since the last logged one and whether to log it: one in failLogGap.
func (p *pickState) countLocked(ok bool) (since int, logFail bool) {
	if ok {
		p.tally.ok++
		p.okSinceFail++
		return 0, false
	}
	p.tally.failed++
	if p.now().Sub(p.lastFailLog) < failLogGap {
		return 0, false
	}
	since, p.okSinceFail, p.lastFailLog = p.okSinceFail, 0, p.now()
	return since, true
}

func (p *pickState) nameLocked() string {
	if p.key == "" {
		return "without a name"
	}
	return p.key
}

func (p *pickState) resetLocked() {
	p.gen++
	p.cur, p.won, p.confirmed = "", false, false
	p.fails, p.tried, p.slow, p.attempts = 0, 0, 0, 0
	p.lastFail, p.noneUntil = time.Time{}, time.Time{}
	p.ips, p.tick, p.passed = nil, 0, false
	p.clusterFirst, p.clusterSilent, p.okClusters = nil, addrs{}, addrs{}
	p.tally, p.summarized, p.okSinceFail, p.lastFailLog = tally{}, tally{}, 0, time.Time{}
}

// applyLocked starts this network where the map says: its winner, or
// "nothing works" until a week after. A nameless one starts from the
// latest winner of any network.
func (p *pickState) applyLocked() {
	list := strategiesNow()
	p.cur, p.won = "", false
	if p.key != "" {
		if e, ok := p.nets[p.key]; ok {
			if until := time.Unix(e.At, 0).Add(noneFor); e.None && p.now().Before(until) {
				p.noneUntil = until
			}
			for ip, until := range e.Dead {
				if t := time.Unix(until, 0); p.now().Before(t) {
					p.putLocked(ip).deadUntil = t
				}
			}
			if e.Win != "" && hasID(list, e.Win) {
				p.cur, p.won = e.Win, true
			}
		}
	}
	if p.cur == "" && p.key == "" && hasID(list, p.lastWin) {
		p.cur = p.lastWin
	}
	if p.cur == "" && len(list) > 0 {
		p.cur = list[0].id
	}
}

func hasID(list []strategy, id string) bool {
	return slices.ContainsFunc(list, func(s strategy) bool { return s.id == id })
}

func (p *pickState) wantASNLocked() bool {
	if !strings.HasPrefix(p.raw, "wifi@") || p.key != "" || p.asnTried || !directOn.Load() || len(strategiesNow()) == 0 {
		return false
	}
	p.asnTried = true
	return true
}

func (p *pickState) lookupASN(gen uint64, raw string) {
	defer guard("asn")
	// The services' names resolve through the network's resolvers, and
	// those may come a moment after the network itself.
	for deadline := time.Now().Add(asnDNSWait); dnsServers() == "" && time.Now().Before(deadline); {
		time.Sleep(100 * time.Millisecond)
	}
	if dnsServers() == "" {
		p.mu.Lock()
		if p.raw == raw {
			p.asnTried = false // the next Start asks
		}
		p.mu.Unlock()
		return
	}
	asn, from := asnLookup()
	p.mu.Lock()
	if asn != "" {
		if p.asns == nil {
			p.asns = map[string]string{}
		}
		p.asns[raw] = asn
	}
	if p.gen != gen || p.raw != raw {
		p.mu.Unlock()
		return
	}
	if asn == "" {
		log.Printf("sni desync: wifi provider unknown, no name for this network")
		p.mu.Unlock()
		return
	}
	p.summaryLocked()
	p.key = "wifi:AS" + asn
	p.resetLocked()
	p.applyLocked()
	log.Printf("sni desync: network %s (%s), starting from %s%s", p.key, from, p.cur, p.noneNoteLocked())
	p.unlockAndTell()
}

// pick is the strategy for a new connection to ip named name, nil for the
// relay; gen goes back with its result. race is when a relay leg races the
// direct try: negative never, 0 at once.
func (p *pickState) pick(ip, name string) (st *strategy, gen uint64, race time.Duration) {
	list := strategiesNow()
	if !directOn.Load() || len(list) == 0 {
		return nil, 0, -1
	}
	p.mu.Lock()
	// The week's end changes the verdict.
	defer p.unlockAndTell()
	now := p.now()
	if !p.noneUntil.IsZero() {
		if now.Before(p.noneUntil) {
			return nil, 0, -1
		}
		p.noneUntil, p.tried = time.Time{}, 0
		log.Printf("sni desync: %s, a week without a strategy is over, trying from %s", p.nameLocked(), p.cur)
	}
	a := p.addrLocked(ip)
	if a != nil {
		p.usedLocked(a)
		if now.Before(a.relayUntil) {
			p.tally.skipped++
			return nil, 0, -1
		}
	}
	i := slices.IndexFunc(list, func(s strategy) bool { return s.id == p.cur })
	if i < 0 {
		p.restartLocked()
		i = slices.IndexFunc(list, func(s strategy) bool { return s.id == p.cur })
	}
	race = raceBase()
	switch {
	case race < 0:
	// It answers direct: a leg would only take the session from it.
	case a != nil && (a.hello || now.Before(a.muteUntil)):
		race = -1
	case p.clusterSilentLocked(videoCluster(name), now):
		race = 0
	case len(list[i].ttl1) > 0:
		race += raceTTL1
	}
	return &list[i], p.gen, race
}

// videoCluster is the group of googlevideo's cache nodes a name belongs
// to: rr3---sn-oj5hn5-5g.googlevideo.com → sn-oj5hn5; "" for other names.
// Not the whole node name: one group shows up under several suffixes.
func videoCluster(name string) string {
	host, ok := strings.CutSuffix(strings.ToLower(strings.TrimSuffix(name, ".")), ".googlevideo.com")
	if !ok || strings.Contains(host, ".") {
		return ""
	}
	_, node, ok := strings.Cut(host, "---")
	tok, isNode := strings.CutPrefix(node, "sn-")
	if !ok || !isNode {
		return ""
	}
	tok, _, _ = strings.Cut(tok, "-")
	if tok == "" {
		return ""
	}
	return "sn-" + tok
}

func (p *pickState) clusterSilentLocked(c string, now time.Time) bool {
	until, ok := p.clusterSilent.get(c)
	return c != "" && ok && now.Before(until)
}

// silentLocked: ip goes by relay now.
func (p *pickState) silentLocked(ip string, now time.Time) bool {
	a := p.addrLocked(ip)
	return a != nil && now.Before(a.relayUntil)
}

// clusterSilencedLocked: ip of cluster c just turned silent. The second
// address of a cluster silent at once, with none of it ever through here,
// makes the whole cluster race the relay at once; each later one extends
// that. One silent address is no sign: a live cluster has those too.
func (p *pickState) clusterSilencedLocked(c, ip string, now time.Time) {
	if c == "" || p.okClusters.has(c) {
		return
	}
	if p.clusterSilentLocked(c, now) {
		p.clusterSilent.put(c, now.Add(silentFor))
		return
	}
	if first, ok := p.clusterFirst[c]; ok && p.silentLocked(first, now) {
		if first != ip {
			p.clusterSilent.put(c, now.Add(silentFor))
			log.Printf("sni desync: cluster %s silent (2 addresses) → relay races at once", c)
		}
		return
	}
	// The first one's mark is over: this one takes its place.
	if p.clusterFirst == nil {
		p.clusterFirst = map[string]string{}
	}
	if _, in := p.clusterFirst[c]; !in && len(p.clusterFirst) >= addrMemory {
		for k := range p.clusterFirst {
			delete(p.clusterFirst, k)
			break
		}
	}
	p.clusterFirst[c] = ip
}

// silentNow: ip's relayed sessions are watched (gen goes back with
// relayNoAnswer). It goes by relay because the direct path got no
// ServerHello from it: the winner never got through to it, or the try had
// no ServerHello (also before the winner is confirmed). Or it answered
// direct: a leg that won by the VDS's 101 may serve a node the relay does
// not reach, the session would hang till the VDS times out. Not while it
// is direct only.
func (p *pickState) silentNow(ip string) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	a := p.addrLocked(ip)
	return p.gen, a != nil && (now.Before(a.relayUntil) || a.noHello || a.hello) && !now.Before(a.muteUntil)
}

func (p *pickState) mutedNow(ip string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.addrLocked(ip)
	return a != nil && p.now().Before(a.muteUntil)
}

// helloNow: a real direct try to ip got its ServerHello, see addrInfo.hello.
func (p *pickState) helloNow(ip string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.addrLocked(ip)
	return a != nil && a.hello
}

// relayNoAnswer: a watched relayed session to ip got no byte back while
// the VDS answered (eof: its relay side ended it first, before deadWait).
// One that answered direct is not dead, the relay does not reach it (an
// ISP's own cache seen from abroad): direct only for silentFor. One that
// never did is dead, unless the end was fast: one reset of a node must not
// refuse a blocked Google node the relay usually serves. Reports whether
// ip's sessions are to be closed.
func (p *pickState) relayNoAnswer(gen uint64, ip string, eof bool) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen != p.gen {
		return false
	}
	now := p.now()
	a := p.addrLocked(ip)
	switch {
	case a != nil && now.Before(a.deadUntil):
		return true
	case a != nil && now.Before(a.muteUntil):
		return false
	case a != nil && a.hello:
		log.Printf("sni desync: %s: relay does not reach it → direct only", ip)
		a.muteUntil, a.relayUntil = now.Add(silentFor), time.Time{}
		return true
	case eof:
		return false
	}
	p.markDeadLocked(ip)
	return true
}

// markDead: a silent address the relay does not reach either, such as a
// Google cache inside one ISP's network seen from another network. Its
// connections get refused for deadFor, see deadLocked. A repeated mark
// does not extend it (recheckDone does).
// Reports whether ip is dead on this network now.
func (p *pickState) markDead(gen uint64, ip string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen != p.gen {
		return false
	}
	p.markDeadLocked(ip)
	return true
}

func (p *pickState) markDeadLocked(ip string) {
	if a := p.addrLocked(ip); a != nil && p.now().Before(a.deadUntil) {
		return
	}
	log.Printf("sni desync: %s silent through the relay too → closing, refusing connections for %s", ip, deadFor)
	// The check that found it dead is as fresh as a recheck.
	p.deadLocked(ip).checked = p.now()
}

// deadLocked: ip's connections get refused for deadFor, on this network
// even after a restart: a fast error instead of the player's 30 s
// handshake timeout. Its failures are not the strategy's.
func (p *pickState) deadLocked(ip string) *addrInfo {
	now := p.now()
	until := now.Add(deadFor)
	a := p.putLocked(ip)
	a.deadUntil, a.hello, a.muteUntil, a.checked = until, false, time.Time{}, time.Time{}
	if a.failed > 0 {
		p.fails = max(p.fails-a.failed, 0)
		a.failed = 0
	}
	if p.key == "" {
		return a
	}
	e := p.nets[p.key]
	dead := map[string]int64{ip: until.Unix()}
	for k, u := range e.Dead {
		if now.Unix() < u && k != ip {
			dead[k] = u
		}
	}
	if len(dead) > deadMax {
		oldest := ""
		for k, u := range dead {
			if oldest == "" || u < dead[oldest] {
				oldest = k
			}
		}
		delete(dead, oldest)
	}
	e.Dead = dead
	if e.At == 0 {
		e.At = now.Unix()
	}
	if p.nets == nil {
		p.nets = map[string]netEntry{}
	}
	p.nets[p.key] = e
	p.writeLocked()
	return a
}

// liftLocked: dead ip answered how, it is no longer dead here nor in the
// map. An answer direct also ends its time by relay: the relay is what
// found it dead.
func (p *pickState) liftLocked(ip, how string, direct bool) {
	log.Printf("sni desync: %s answers %s → no longer dead", ip, how)
	a := p.ips[ip]
	a.deadUntil, a.checked = time.Time{}, time.Time{}
	if direct {
		a.relayUntil = time.Time{}
	}
	if e, ok := p.nets[p.key]; ok && p.key != "" {
		delete(e.Dead, ip)
		p.nets[p.key] = e
		p.writeLocked()
	}
}

func (p *pickState) deadNow(ip string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.addrLocked(ip)
	return a != nil && p.now().Before(a.deadUntil)
}

// recheckDue: the player asks for a dead address that was not probed for
// deadRecheck (or since it came from the map, or turned dead without a
// probe). True once per due time: the caller probes it and reports with
// recheckDone.
func (p *pickState) recheckDue(ip string) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	a := p.addrLocked(ip)
	if a == nil || !now.Before(a.deadUntil) || now.Sub(a.checked) < deadRecheck {
		return 0, false
	}
	a.checked = now
	return p.gen, true
}

// directProbe is the strategy a recheck tries a dead address with
// directly, nil while direct tries are off here; gen as pick's. Not a try:
// it counts for nothing, neither the strategy nor the verdict.
func (p *pickState) directProbe() (*strategy, uint64) {
	list := strategiesNow()
	if !directOn.Load() || len(list) == 0 {
		return nil, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	i := slices.IndexFunc(list, func(s strategy) bool { return s.id == p.cur })
	if i < 0 || p.now().Before(p.noneUntil) {
		return nil, 0
	}
	return &list[i], p.gen
}

// recheckDone: direct or relay, the probe that got an answer: the address
// is no longer dead. Neither (both probes sent, the VDS answering others
// meanwhile): it stays dead for deadFor from now. Both written to the map.
func (p *pickState) recheckDone(gen uint64, ip string, direct, relay bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a := p.addrLocked(ip)
	if gen != p.gen || a == nil || !p.now().Before(a.deadUntil) {
		return
	}
	switch {
	case direct:
		p.liftLocked(ip, "a direct probe", true)
		return
	case relay:
		p.liftLocked(ip, "through the relay again", false)
		return
	}
	a.deadUntil = p.now().Add(deadFor)
	if p.key == "" {
		return
	}
	if e := p.nets[p.key]; e.Dead != nil {
		e.Dead[ip] = a.deadUntil.Unix()
		p.writeLocked()
	}
}

// restartLocked: the current strategy is gone from Remote Config's list
// (or the list was empty): start this network again where the map says.
func (p *pickState) restartLocked() {
	p.won, p.confirmed, p.passed = false, false, false
	p.fails, p.tried = 0, 0
	p.clearLocked(true)
	p.applyLocked()
}

// unreachable: the direct socket did not even connect, the hello never
// left. The strategy is not to blame; the address goes by relay a while.
func (p *pickState) unreachable(gen uint64, ip string) (since int, logFail bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if gen != p.gen {
		return 0, false
	}
	now := p.now()
	switch a := p.putLocked(ip); {
	// One of a burst, after another turned it dead.
	case now.Before(a.deadUntil):
	case now.Before(a.muteUntil):
		p.directOnlyFailedLocked(ip)
	default:
		a.relayUntil = now.Add(silentFor)
	}
	return p.countLocked(false)
}

// directOnlyFailedLocked: ip, direct only, failed direct too: it is dead.
// Not probed yet: the first refused connection probes it, a single miss
// heals within desyncWait.
func (p *pickState) directOnlyFailedLocked(ip string) {
	log.Printf("sni desync: %s: direct fails too, relay never reached it → dead", ip)
	p.deadLocked(ip)
}

// result of a direct try to ip named name: did the server answer the
// hello. start is when the connection began: failures of one burst of
// parallel ones count once. since and logFail: see countLocked.
func (p *pickState) result(gen uint64, id, ip, name string, start time.Time, ok bool, why string) (since int, logFail bool) {
	p.mu.Lock()
	defer p.unlockAndTell()
	if gen != p.gen {
		return 0, false
	}
	since, logFail = p.countLocked(ok)
	c := videoCluster(name)
	now := p.now()
	a := p.putLocked(ip)
	p.usedLocked(a)
	if ok {
		a.hello, a.noHello = true, false
		if now.Before(a.deadUntil) {
			p.liftLocked(ip, "direct", true)
		}
		// Whichever strategy, and even with the relay serving the session:
		// the nodes are reachable.
		if c != "" {
			p.okClusters.put(c, time.Time{})
			p.clusterSilent.del(c)
		}
	} else {
		a.noHello = true
		if now.Before(a.muteUntil) {
			p.directOnlyFailedLocked(ip)
			return
		}
	}
	if id != p.cur || now.Before(p.noneUntil) {
		return
	}
	// A try begun before its address turned dead: not the strategy's.
	if !ok && now.Before(a.deadUntil) {
		return
	}
	p.attempts++
	if ok {
		p.fails, p.tried, p.passed = 0, 0, true
		p.clearLocked(false)
		a.curOK = true
		if p.okCountLocked() >= 2 && !p.confirmed {
			p.confirmed, p.won, p.lastWin = true, true, id
			log.Printf("sni desync: WINNER %s on %s (2 addresses)", id, p.nameLocked())
			p.saveLocked(netEntry{Win: id})
		}
		return
	}
	// Google's nodes get blocked by address too: a winner silent where it
	// never got through blames the address, not the strategy.
	if p.confirmed && !a.curOK {
		if !now.Before(a.relayUntil) {
			log.Printf("sni desync: %s silent under %s → relay for %s", ip, id, silentFor)
		}
		a.relayUntil = now.Add(silentFor)
		p.clusterSilencedLocked(c, ip, now)
		return
	}
	if !start.Before(p.lastFail) {
		a.failed++
		p.failLocked(why)
	}
	return
}

func (p *pickState) failLocked(why string) {
	p.lastFail = p.now()
	p.fails++
	need := needFails
	if p.won {
		need = needFailsWon
	}
	if p.fails < need {
		return
	}
	list := strategiesNow()
	if len(list) == 0 {
		return
	}
	prev := p.cur
	i := slices.IndexFunc(list, func(s strategy) bool { return s.id == prev })
	p.cur = list[(i+1)%len(list)].id
	p.won, p.confirmed, p.passed, p.fails = false, false, false, 0
	p.clearLocked(true)
	p.tried++
	if p.tried >= len(list) {
		p.noneLocked(fmt.Sprintf("all %d failed, last %s: %s", len(list), prev, why))
		return
	}
	log.Printf("sni desync: %s failed %d× (%s), next %s", prev, need, why, p.cur)
}

// noneLocked: YouTube goes by relay on this network for a week, no waits.
func (p *pickState) noneLocked(why string) {
	p.noneUntil, p.tried, p.slow = p.now().Add(noneFor), 0, 0
	log.Printf("sni desync: nothing works on %s (%s), relay for %s", p.nameLocked(), why, noneFor)
	p.saveLocked(netEntry{None: true})
}

type finish struct {
	start       time.Time
	dur         time.Duration
	down, up    int64
	peak        int64 // bytes/s
	serverFirst bool
	ours        bool // our Close ended it: Stop, NetworkLost
	why         string
}

// finished: a direct session that got its ServerHello is over.
func (p *pickState) finished(gen uint64, id, ip, name string, f finish) {
	p.mu.Lock()
	defer p.unlockAndTell()
	if gen != p.gen || f.ours {
		return
	}
	if f.up > earlyDeathUp && f.down < earlyDeathDown {
		p.tally.empty++
		p.tally.lastEmpty = f.why
	}
	if f.serverFirst && f.dur < earlyDeath && f.down < earlyDeathDown && f.up > earlyDeathUp {
		a := p.putLocked(ip)
		a.curOK = false
		// Not the strategy's either: three of those would drop the winner
		// on a network whose caches the relay does not reach.
		if p.now().Before(a.muteUntil) {
			return
		}
		a.relayUntil = p.now().Add(silentFor)
		log.Printf("sni desync: %s died early under %s (%s) → relay for %s", ip, id, f.why, silentFor)
		if id == p.cur && !p.now().Before(p.noneUntil) && !f.start.Before(p.lastFail) {
			p.failLocked("died early")
		}
		return
	}
	// No burst long and big enough to measure (a few small requests):
	// nothing to judge the rate by.
	if f.peak == 0 {
		return
	}
	switch {
	case f.peak > slowPeak:
		p.slow = 0
	case strings.HasSuffix(name, "googlevideo.com") && f.down > slowMin:
		if p.slow++; p.slow >= slowRun && !p.now().Before(p.noneUntil) {
			p.noneLocked(fmt.Sprintf("throttled: %d videos in a row under %d KB/s", slowRun, slowPeak>>10))
		}
	}
}

// hasWinner: this network has a strategy that gets through.
func (p *pickState) hasWinner() bool {
	if !directOn.Load() || len(strategiesNow()) == 0 {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.won && !p.now().Before(p.noneUntil)
}

func (p *pickState) verdictLocked() string {
	switch {
	case !directOn.Load() || len(strategiesNow()) == 0:
		return verdictUnknown
	case p.now().Before(p.noneUntil):
		return verdictFails
	case p.attempts == 0:
		// A router that answers DNS with its own addresses (fake-IP) keeps
		// YouTube off the TUN: no tries, nothing to say.
		return verdictUnknown
	case p.won && p.passed:
		return verdictWorks
	}
	return verdictTesting
}

// unlockAndTell releases mu and tells the host a verdict that changed.
// Off mu: the host may take its time; under tellMu: in order.
func (p *pickState) unlockAndTell() {
	v := p.verdictLocked()
	changed := v != p.told
	p.told = v
	h := protector
	p.tellMu.Lock()
	p.mu.Unlock()
	defer p.tellMu.Unlock()
	if changed && h != nil {
		h.DirectVerdict(v)
	}
}

// saveLocked writes the map with e for this network; a nameless one has
// no entry. Under mu: rare (a verdict changed) and tiny.
func (p *pickState) saveLocked(e netEntry) {
	if p.key == "" {
		return
	}
	e.At = p.now().Unix()
	if p.nets == nil {
		p.nets = map[string]netEntry{}
	}
	e.Dead = p.nets[p.key].Dead
	p.nets[p.key] = e
	p.writeLocked()
}

func (p *pickState) writeLocked() {
	trimNetMap(p.nets)
	if p.file == "" {
		return
	}
	b, _ := json.Marshal(netMapFile{V: 1, Nets: p.nets})
	tmp := p.file + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		log.Printf("desync: save: %v", err)
		return
	}
	if err := os.Rename(tmp, p.file); err != nil {
		log.Printf("desync: save: %v", err)
	}
}

func trimNetMap(m map[string]netEntry) {
	if len(m) <= mapMax {
		return
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return m[keys[i]].At < m[keys[j]].At })
	for _, k := range keys[:len(keys)-mapMax] {
		delete(m, k)
	}
}

// loadNetMap: a missing or broken file is an empty map. Unknown strategy
// ids stay in it and are ignored when the network comes up.
func loadNetMap(file string) map[string]netEntry {
	m := map[string]netEntry{}
	if file == "" {
		return m
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return m
	}
	var f netMapFile
	if err := json.Unmarshal(b, &f); err != nil || f.V != 1 {
		log.Printf("desync: %s unreadable, starting afresh", file)
		return m
	}
	for k, e := range f.Nets {
		m[k] = e
	}
	trimNetMap(m)
	return m
}

// YouTube's own names resolve through DoH as everything of ours, so the
// relay's view keeps them inside the TUN's routes. Only while the relay
// is down for sure (the breaker open: the network's answers bring
// RoutesStale and a rebuild that cuts every relay session) and a strategy gets through here, the network's resolver: then
// YouTube lives without the server. Its answers are dropped once the
// relay is back.
var directDNSUsed atomic.Bool

func directDNSWanted(name string) bool {
	return desyncByName(name) && health.refusing() && picker.hasWinner()
}

func dropDirectDNS() {
	if !directDNSUsed.Swap(false) {
		return
	}
	dnsCache.Range(func(k, _ any) bool {
		if name, _, _ := strings.Cut(k.(string), "/"); desyncByName(name) {
			dnsCache.Delete(k)
		}
		return true
	})
}
