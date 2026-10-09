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
// current one is tried, its failures move on to the next. Winners and
// "nothing works" are kept per network in files/desync.json; the rest is
// memory only. Stop and Start keep it all (a TUN rebuild is a Stop+Start
// every 10 min at most); only another network starts afresh.
const (
	needFails    = 2                // counted failures before a strategy moves on...
	needFailsWon = 3                // ...and before this network's winner does
	silentFor    = 30 * time.Minute // an address the winner does not get through to goes by relay
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

type netEntry struct {
	Win  string `json:"win,omitempty"`
	None bool   `json:"none,omitempty"`
	At   int64  `json:"at"`
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
	ok        addrs     // where cur got through
	silent    addrs     // relay until
	noneUntil time.Time
	slow      int
	attempts  int
	lastWin   string // any network's latest winner: where a nameless one starts

	file   string
	loaded bool
	nets   map[string]netEntry

	tellMu sync.Mutex // the host hears verdicts in order
	told   string
}

var picker = &pickState{now: time.Now}

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
	p.key = key
	p.resetLocked()
	p.applyLocked()
	// A nameless one after a nameless one is no news for the log.
	if directOn.Load() && len(strategiesNow()) > 0 && (key != "" || was != "") {
		log.Printf("sni desync: network %s, starting from %s", p.nameLocked(), p.cur)
	}
	lookup, gen := p.wantASNLocked(), p.gen
	p.unlockAndTell()
	if lookup {
		go p.lookupASN(gen, raw)
	}
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
	p.ok, p.silent, p.passed = addrs{}, addrs{}, false
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
	p.key = "wifi:AS" + asn
	p.resetLocked()
	p.applyLocked()
	log.Printf("sni desync: network %s (%s), starting from %s", p.key, from, p.cur)
	p.unlockAndTell()
}

// pick is the strategy for a new connection to ip, nil for the relay; gen
// goes back with its result.
func (p *pickState) pick(ip string) (*strategy, uint64) {
	list := strategiesNow()
	if !directOn.Load() || len(list) == 0 {
		return nil, 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.now()
	if !p.noneUntil.IsZero() {
		if now.Before(p.noneUntil) {
			return nil, 0
		}
		p.noneUntil, p.tried = time.Time{}, 0
		log.Printf("sni desync: %s, a week without a strategy is over, trying from %s", p.nameLocked(), p.cur)
	}
	if until, ok := p.silent.get(ip); ok {
		if now.Before(until) {
			return nil, 0
		}
		p.silent.del(ip)
	}
	i := slices.IndexFunc(list, func(s strategy) bool { return s.id == p.cur })
	if i < 0 {
		// Gone from Remote Config's list.
		p.cur, p.won, p.confirmed, p.passed = list[0].id, false, false, false
		p.fails, p.tried, p.ok = 0, 0, addrs{}
		i = 0
	}
	return &list[i], p.gen
}

// result of a direct try: did the server answer the hello. start is when
// the connection began: failures of one burst of parallel ones count once.
func (p *pickState) result(gen uint64, id, ip string, start time.Time, ok bool, why string) {
	p.mu.Lock()
	defer p.unlockAndTell()
	if gen != p.gen || id != p.cur || p.now().Before(p.noneUntil) {
		return
	}
	p.attempts++
	if ok {
		p.fails, p.tried, p.passed = 0, 0, true
		p.ok.put(ip, time.Time{})
		if p.ok.len() >= 2 && !p.confirmed {
			p.confirmed, p.won, p.lastWin = true, true, id
			log.Printf("sni desync: WINNER %s on %s (2 addresses)", id, p.nameLocked())
			p.saveLocked(netEntry{Win: id})
		}
		return
	}
	// Google's nodes get blocked by address too: a winner silent where it
	// never got through blames the address, not the strategy.
	if p.confirmed && !p.ok.has(ip) {
		if !p.silent.has(ip) {
			log.Printf("sni desync: %s silent under %s → relay for %s", ip, id, silentFor)
		}
		p.silent.put(ip, p.now().Add(silentFor))
		return
	}
	if start.Before(p.lastFail) {
		return
	}
	p.failLocked(why)
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
	p.won, p.confirmed, p.passed, p.fails, p.ok = false, false, false, 0, addrs{}
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
	if f.serverFirst && f.dur < earlyDeath && f.down < earlyDeathDown && f.up > earlyDeathUp {
		p.ok.del(ip)
		p.silent.put(ip, p.now().Add(silentFor))
		log.Printf("sni desync: %s died early under %s (%s) → relay for %s", ip, id, f.why, silentFor)
		if id == p.cur && !p.now().Before(p.noneUntil) && !f.start.Before(p.lastFail) {
			p.failLocked("died early")
		}
		return
	}
	// No whole second of steady data (fast chunks with pauses between):
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
	p.nets[p.key] = e
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
// is down and a strategy gets through here, the network's resolver: then
// YouTube lives without the server. Its answers are dropped once the
// relay is back.
var directDNSUsed atomic.Bool

func directDNSWanted(name string) bool {
	return desyncByName(name) && health.down() && picker.hasWinner()
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
