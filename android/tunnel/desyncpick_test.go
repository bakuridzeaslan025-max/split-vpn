package tunnel

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// freshPicker swaps the package picker for one on a hand-driven clock.
func freshPicker(t *testing.T) *pickState {
	t.Helper()
	old := picker
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	p := &pickState{}
	p.now = func() time.Time { return now }
	picker = p
	t.Cleanup(func() { picker = old })
	return p
}

func advance(p *pickState, d time.Duration) {
	t := p.now().Add(d)
	p.now = func() time.Time { return t }
}

// picking: plan's list, direct on, a fresh picker with its map in a temp
// dir, on a network with a name.
func picking(t *testing.T) (*pickState, *fakeProtector, string) {
	t.Helper()
	captureLog(t)
	p := freshPicker(t)
	SetStrategies(planStrategies)
	directOn.Store(true)
	fp := &fakeProtector{ok: true}
	protector = fp
	t.Cleanup(func() { strategies.Store(nil); directOn.Store(false); protector = nil })
	file := filepath.Join(t.TempDir(), "desync.json")
	p.started(file)
	p.setNetKey("cell:25001")
	return p, fp, file
}

func (p *pickState) curID() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cur
}

func (p *pickState) verdict() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.verdictLocked()
}

// try is one connection to ip: picked, then its result, a moment later.
func try(p *pickState, ip string, ok bool) string {
	st, gen := p.pick(ip)
	if st == nil {
		return "relay"
	}
	advance(p, time.Second)
	p.result(gen, st.id, ip, p.now().Add(-time.Second), ok, "silence")
	return st.id
}

func readMap(t *testing.T, file string) map[string]netEntry {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var f netMapFile
	if err := json.Unmarshal(b, &f); err != nil || f.V != 1 {
		t.Fatal(string(b), err)
	}
	return f.Nets
}

// A page opens 5–10 connections at once: their failures are one.
func TestPick_BurstCountsOnce(t *testing.T) {
	p, _, _ := picking(t)
	type conn struct {
		id  string
		gen uint64
	}
	var burst []conn
	start := p.now()
	for range 6 {
		st, gen := p.pick("1.1.1.1")
		burst = append(burst, conn{st.id, gen})
	}
	advance(p, 3*time.Second)
	for _, c := range burst {
		p.result(c.gen, c.id, "1.1.1.1", start, false, "x")
	}
	if p.curID() != "rec2" {
		t.Fatal("one burst moved the strategy on")
	}
	if try(p, "1.1.1.1", false); p.curID() != "rec-mid-dis" {
		t.Fatalf("second burst: on %s", p.curID())
	}
}

// One success is not a winner: tiny once won with 1 ok out of 8.
func TestPick_WinnerNeedsTwoAddresses(t *testing.T) {
	p, fp, file := picking(t)
	try(p, "1.1.1.1", true)
	if v := p.verdict(); v != verdictTesting {
		t.Fatalf("one address: %s", v)
	}
	try(p, "5.5.5.5", false)
	try(p, "6.6.6.6", false)
	if p.curID() == "rec2" {
		t.Fatal("unconfirmed strategy kept after 2 failures")
	}
	cur := p.curID()
	try(p, "1.1.1.1", true)
	try(p, "1.1.1.1", true)
	if p.verdict() != verdictTesting {
		t.Fatal("the same address twice is one")
	}
	try(p, "2.2.2.2", true)
	if v := p.verdict(); v != verdictWorks {
		t.Fatalf("two addresses: %s", v)
	}
	if e := readMap(t, file)["cell:25001"]; e.Win != cur || e.None {
		t.Fatalf("map %+v", e)
	}
	if got := fp.verdicts(); !reflect.DeepEqual(got, []string{verdictUnknown, verdictTesting, verdictWorks}) {
		t.Fatalf("host heard %v", got)
	}
}

// A node blocked by address must not cost the strategy that works
// everywhere else.
func TestPick_SilentAddress(t *testing.T) {
	p, _, _ := picking(t)
	try(p, "1.1.1.1", true)
	try(p, "2.2.2.2", true)
	for range 5 {
		try(p, "3.3.3.3", false)
	}
	if got := try(p, "3.3.3.3", true); got != "relay" {
		t.Fatalf("silent address: %s", got)
	}
	if p.curID() != "rec2" {
		t.Fatal("strategy dropped over one address")
	}
	advance(p, silentFor)
	if got := try(p, "3.3.3.3", true); got != "rec2" {
		t.Fatalf("after %s: %s", silentFor, got)
	}
	// Where it used to work: three in a row and it is over.
	try(p, "1.1.1.1", false)
	try(p, "1.1.1.1", false)
	if p.curID() != "rec2" {
		t.Fatal("a winner went after 2")
	}
	try(p, "1.1.1.1", false)
	if p.curID() == "rec2" {
		t.Fatal("a winner failing where it worked stayed")
	}
}

func TestPick_NothingWorks(t *testing.T) {
	p, fp, file := picking(t)
	for range 2 * len(strategiesNow()) {
		try(p, "1.1.1.1", false)
	}
	if got := try(p, "1.1.1.1", true); got != "relay" {
		t.Fatalf("nothing works: %s", got)
	}
	if v := p.verdict(); v != verdictFails {
		t.Fatalf("verdict %s", v)
	}
	if e := readMap(t, file)["cell:25001"]; !e.None {
		t.Fatalf("map %+v", e)
	}
	if v := fp.verdicts(); v[len(v)-1] != verdictFails {
		t.Fatalf("host heard %v", v)
	}
	advance(p, noneFor)
	if got := try(p, "1.1.1.1", true); got == "relay" {
		t.Fatal("still off after a week")
	}
}

func TestPick_EarlyDeathOnlyWhenTheServerClosedFirst(t *testing.T) {
	p, _, _ := picking(t)
	try(p, "1.1.1.1", true)
	try(p, "2.2.2.2", true)
	st, gen := p.pick("1.1.1.1")
	died := finish{start: p.now(), dur: time.Second, down: 2 << 10, up: 600, serverFirst: true, why: "x"}

	short := died
	short.serverFirst = false // a beacon, a 204: the app was done first
	p.finished(gen, st.id, "1.1.1.1", "www.youtube.com", short)
	ours := died
	ours.ours = true // Stop, NetworkLost
	p.finished(gen, st.id, "1.1.1.1", "www.youtube.com", ours)
	p.finished(gen-1, st.id, "1.1.1.1", "www.youtube.com", died)
	if got := try(p, "1.1.1.1", true); got != "rec2" {
		t.Fatalf("not an early death, yet %s", got)
	}

	advance(p, time.Second)
	p.finished(gen, st.id, "1.1.1.1", "www.youtube.com", finish{start: p.now(), dur: time.Second, down: 2 << 10, up: 600, serverFirst: true})
	if got := try(p, "1.1.1.1", true); got != "relay" {
		t.Fatalf("died early, yet %s", got)
	}
	p.mu.Lock()
	fails := p.fails
	p.mu.Unlock()
	if fails != 1 {
		t.Fatalf("an early death is a failure of the strategy: fails %d", fails)
	}
	// Both addresses it got through died early: still the winner, and the
	// card must not flip to "testing" and back.
	st, gen = p.pick("2.2.2.2")
	p.finished(gen, st.id, "2.2.2.2", "www.youtube.com", finish{start: p.now(), dur: time.Second, down: 2 << 10, up: 600, serverFirst: true})
	if v := p.verdict(); v != verdictWorks {
		t.Fatalf("verdict %s", v)
	}
}

func TestPick_Throttled(t *testing.T) {
	p, _, _ := picking(t)
	try(p, "1.1.1.1", true)
	try(p, "2.2.2.2", true)
	video := func(down, peak int64) {
		st, gen := p.pick("1.1.1.1")
		p.finished(gen, st.id, "1.1.1.1", "rr1---sn-x.googlevideo.com", finish{start: p.now(), dur: time.Minute, down: down, up: 4 << 10, peak: peak})
	}
	// A fast network: chunks of 1–2 MB in under a second each, pauses
	// between them, no whole second to measure. Not throttled.
	for range slowRun + 1 {
		video(2<<20, 0)
	}
	if v := p.verdict(); v != verdictWorks {
		t.Fatalf("fast chunks judged: %s", v)
	}
	video(1<<20, 30<<10)
	video(1<<20, 30<<10)
	video(1<<20, 50<<10) // a fast one starts the count again
	video(1<<20, 30<<10)
	video(100<<10, 1<<10) // too small to judge
	video(1<<20, 30<<10)
	if p.verdict() != verdictWorks {
		t.Fatal("throttled after 2")
	}
	video(1<<20, slowPeak)
	if v := p.verdict(); v != verdictFails {
		t.Fatalf("3 slow videos in a row: %s", v)
	}
}

// The host calls SetNetKey on every callback with the same network.
func TestSetNetKey_SameKeyChangesNothing(t *testing.T) {
	p, _, _ := picking(t)
	try(p, "1.1.1.1", false)
	gen := p.gen
	p.setNetKey("cell:25001")
	if p.gen != gen || p.fails != 1 {
		t.Fatal("same network reset the picker")
	}
	p.setNetKey("cell:25002")
	if p.gen == gen || p.fails != 0 {
		t.Fatal("new network kept the old one's count")
	}
}

// A TUN rebuild is Stop+Start every 10 min at most: picking from scratch
// each time would never find anything.
func TestPick_StopStartKeepsIt(t *testing.T) {
	p, _, file := picking(t)
	try(p, "1.1.1.1", true)
	try(p, "5.5.5.5", false)
	Stop()
	p.started(file)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.fails != 1 || p.ok.len() != 1 || p.raw != "cell:25001" {
		t.Fatalf("lost on restart: fails %d, ok %d, key %q", p.fails, p.ok.len(), p.raw)
	}
}

// Results of connections from the network before do not count.
func TestPick_OldNetworkDoesNotCount(t *testing.T) {
	p, _, _ := picking(t)
	st, gen := p.pick("1.1.1.1")
	p.setNetKey("cell:25002")
	p.result(gen, st.id, "1.1.1.1", p.now(), false, "x")
	p.result(gen, st.id, "2.2.2.2", p.now().Add(time.Second), false, "x")
	p.result(gen, st.id, "2.2.2.2", p.now().Add(time.Second), true, "")
	if p.fails != 0 || p.attempts != 0 || p.ok.len() != 0 {
		t.Fatal("an old network's result counted")
	}
}

func TestAddrs_KeepsTheLatest(t *testing.T) {
	var a addrs
	for i := range addrMemory + 10 {
		a.put(fmt.Sprintf("10.0.0.%d", i), time.Time{})
	}
	if a.len() != addrMemory || a.has("10.0.0.0") || !a.has(fmt.Sprintf("10.0.0.%d", addrMemory+9)) {
		t.Fatalf("len %d", a.len())
	}
}

func TestNetMap(t *testing.T) {
	p, _, file := picking(t)
	nets := map[string]netEntry{
		"cell:25002":  {Win: "dis-mid", At: p.now().Unix()},
		"cell:25003":  {Win: "gone-from-rc", At: p.now().Add(-time.Minute).Unix()},
		"wifi:AS8359": {None: true, At: p.now().Add(-time.Hour).Unix()},
		"cell:25004":  {None: true, At: p.now().Add(-noneFor - time.Hour).Unix()},
	}
	for i := range mapMax {
		nets[fmt.Sprintf("cell:9%04d", i)] = netEntry{Win: "rec2", At: int64(i)}
	}
	b, _ := json.Marshal(netMapFile{V: 1, Nets: nets})
	os.WriteFile(file, b, 0o600)

	q := freshPicker(t)
	q.now = p.now
	q.started(file)
	if len(q.nets) != mapMax || q.lastWin != "dis-mid" {
		t.Fatalf("%d entries, last winner %s", len(q.nets), q.lastWin)
	}
	q.setNetKey("cell:25002")
	if q.curID() != "dis-mid" || !q.won {
		t.Fatalf("winner from the map: %s", q.curID())
	}
	q.setNetKey("cell:25003")
	if q.curID() != "rec2" || q.won {
		t.Fatalf("unknown id: %s", q.curID())
	}
	q.setNetKey("cell:25004")
	if got := try(q, "1.1.1.1", false); got == "relay" {
		t.Fatal("a none older than a week still holds")
	}
	q.raw = "wifi@1"
	q.key = "wifi:AS8359"
	q.resetLocked()
	q.applyLocked()
	if got := try(q, "1.1.1.1", true); got != "relay" {
		t.Fatalf("none from the map: %s", got)
	}

	// A network without a name starts from the latest winner and keeps
	// its verdict to itself.
	q.setNetKey("")
	if q.curID() != "dis-mid" {
		t.Fatalf("nameless starts from %s", q.curID())
	}
	before, _ := os.ReadFile(file)
	for range 2 * len(strategiesNow()) {
		try(q, "1.1.1.1", false)
	}
	if q.verdict() != verdictFails {
		t.Fatal("nameless: nothing works, not told")
	}
	if after, _ := os.ReadFile(file); string(after) != string(before) {
		t.Fatal("nameless network written to the map")
	}

	os.WriteFile(file, []byte("{broken"), 0o600)
	r := freshPicker(t)
	r.started(file)
	if len(r.nets) != 0 {
		t.Fatal("broken file read")
	}
}

// Off or without strategies: always the relay, nothing to say.
func TestPick_OffIsRelay(t *testing.T) {
	p, _, _ := picking(t)
	directOn.Store(false)
	if got := try(p, "1.1.1.1", true); got != "relay" || p.verdict() != verdictUnknown {
		t.Fatalf("off: %s, %s", got, p.verdict())
	}
	directOn.Store(true)
	SetStrategies("[]")
	if got := try(p, "1.1.1.1", true); got != "relay" || p.verdict() != verdictUnknown {
		t.Fatalf("[]: %s, %s", got, p.verdict())
	}
}

// Every line about direct YouTube stays on the device.
func TestPick_LinesStartWithSni(t *testing.T) {
	p, _, _ := picking(t)
	out := captureLog(t)
	try(p, "1.1.1.1", true)
	try(p, "2.2.2.2", true)
	for range 5 {
		try(p, "3.3.3.3", false)
	}
	for _, l := range out.lines {
		if !strings.HasPrefix(l, "sni ") {
			t.Errorf("line %q", l)
		}
	}
	if len(out.lines) < 2 {
		t.Fatalf("lines %v", out.lines)
	}
}
