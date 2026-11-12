package tunnel

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
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
	id, _ := tryAs(p, ip, "", ok)
	return id
}

// tryAs is try for a connection named name; race is the relay leg's delay.
func tryAs(p *pickState, ip, name string, ok bool) (id string, race time.Duration) {
	st, gen, race := p.pick(ip, name)
	if st == nil {
		return "relay", race
	}
	advance(p, time.Second)
	p.result(gen, st.id, ip, name, p.now().Add(-time.Second), ok, "silence")
	return st.id, race
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
		st, gen, _ := p.pick("1.1.1.1", "")
		burst = append(burst, conn{st.id, gen})
	}
	advance(p, 3*time.Second)
	for _, c := range burst {
		p.result(c.gen, c.id, "1.1.1.1", "", start, false, "x")
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
	st, gen, _ := p.pick("1.1.1.1", "")
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
	st, gen, _ = p.pick("2.2.2.2", "")
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
		st, gen, _ := p.pick("1.1.1.1", "")
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
	if p.fails != 1 || p.okCountLocked() != 1 || p.raw != "cell:25001" {
		t.Fatalf("lost on restart: fails %d, ok %d, key %q", p.fails, p.okCountLocked(), p.raw)
	}
}

// Results of connections from the network before do not count.
func TestPick_OldNetworkDoesNotCount(t *testing.T) {
	p, _, _ := picking(t)
	st, gen, _ := p.pick("1.1.1.1", "")
	p.setNetKey("cell:25002")
	p.result(gen, st.id, "1.1.1.1", "", p.now(), false, "x")
	p.result(gen, st.id, "2.2.2.2", "", p.now().Add(time.Second), false, "x")
	p.result(gen, st.id, "2.2.2.2", "", p.now().Add(time.Second), true, "")
	if p.fails != 0 || p.attempts != 0 || p.okCountLocked() != 0 {
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

// Remote Config's list came back after an empty one: the map's winner on
// this network counts again at the next Start, without a network change.
func TestPick_ListBackAfterEmpty(t *testing.T) {
	p, _, file := picking(t)
	p.mu.Lock()
	p.nets["cell:25009"] = netEntry{Win: "dis-mid", At: p.now().Unix()}
	p.mu.Unlock()
	SetStrategies("[]")
	p.setNetKey("cell:25009")
	SetStrategies(planStrategies)
	p.started(file)
	if p.curID() != "dis-mid" || !p.won {
		t.Fatalf("on %s, won %v", p.curID(), p.won)
	}
}

// The week of "nothing works" ends at a pick: the host hears it.
func TestPick_WeekOverIsTold(t *testing.T) {
	p, fp, _ := picking(t)
	for range 2 * len(strategiesNow()) {
		try(p, "1.1.1.1", false)
	}
	advance(p, noneFor)
	p.pick("1.1.1.1", "")
	if v := fp.verdicts(); v[len(v)-1] == verdictFails {
		t.Fatalf("host still thinks %v", v)
	}
}

// fail1 is one direct try to ip with its result at once; what the log hears.
func fail1(p *pickState, ip string, ok bool) (int, bool) {
	st, gen, _ := p.pick(ip, "")
	if st == nil {
		return 0, false
	}
	return p.result(gen, st.id, ip, "", p.now(), ok, "x")
}

func (p *pickState) tallyNow() tally {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tally
}

func TestPick_FailLinesThrottled(t *testing.T) {
	p, _, _ := picking(t)
	if since, log := fail1(p, "1.1.1.1", false); !log || since != 0 {
		t.Fatalf("first: %d %v", since, log)
	}
	advance(p, time.Second)
	if _, log := fail1(p, "1.1.1.1", false); log {
		t.Fatal("a second line within the gap")
	}
	fail1(p, "2.2.2.2", true)
	fail1(p, "3.3.3.3", true)
	advance(p, failLogGap)
	if since, log := fail1(p, "1.1.1.1", false); !log || since != 2 {
		t.Fatalf("after the gap: %d %v", since, log)
	}
	advance(p, time.Second)
	_, gen, _ := p.pick("4.4.4.4", "")
	if _, log := p.unreachable(gen, "4.4.4.4"); log {
		t.Fatal("unreachable within the gap logged")
	}
	advance(p, failLogGap)
	_, gen, _ = p.pick("5.5.5.5", "")
	if since, log := p.unreachable(gen, "5.5.5.5"); !log || since != 0 {
		t.Fatalf("unreachable after the gap: %d %v", since, log)
	}
	if got := p.tallyNow(); got != (tally{ok: 2, failed: 5}) {
		t.Fatalf("tally %+v", got)
	}
}

// The summary names the network being left and comes once per change.
func TestPick_Summary(t *testing.T) {
	p, _, _ := picking(t)
	out := captureLog(t)
	try(p, "1.1.1.1", true)
	try(p, "2.2.2.2", true)
	try(p, "3.3.3.3", false)
	try(p, "3.3.3.3", true) // silent: straight to relay
	st, gen, _ := p.pick("1.1.1.1", "")
	p.finished(gen, st.id, "1.1.1.1", "rr1.googlevideo.com", finish{dur: 10 * time.Second, up: 900, down: 100, why: "rr1.googlevideo.com, EOF"})
	p.finished(gen, st.id, "1.1.1.1", "rr1.googlevideo.com", finish{dur: 10 * time.Second, up: 0, down: 100, why: "preconnect"})
	p.countRace(gen, func(t *tally) { t.raced += 2; t.relayWon++; t.late++ })
	p.countRace(gen-1, func(t *tally) { t.raced++ })
	p.stopped()
	p.stopped()
	want := "sni desync: on cell:25001: 2 direct ok (1 empty, last: rr1.googlevideo.com, EOF), 1 failed, 1 straight to relay, 2 raced (1 relay won, 1 direct late)"
	if n := slices.Index(out.lines, want); n < 0 || slices.Index(out.lines[n+1:], want) >= 0 {
		t.Fatalf("lines %q", out.lines)
	}
	try(p, "1.1.1.1", true)
	p.setNetKey("cell:25002")
	want = "sni desync: on cell:25001: 3 direct ok (1 empty, last: rr1.googlevideo.com, EOF), 1 failed, 1 straight to relay, 2 raced (1 relay won, 1 direct late)"
	if !slices.Contains(out.lines, want) {
		t.Fatalf("lines %q", out.lines)
	}
	if got := p.tallyNow(); got != (tally{}) {
		t.Fatalf("new network kept %+v", got)
	}
	n := len(out.lines)
	p.setNetKey("cell:25003")
	for _, l := range out.lines[n:] {
		if strings.Contains(l, " on cell:25002") {
			t.Fatalf("summary of nothing: %q", l)
		}
	}
	for _, l := range out.lines {
		if !strings.HasPrefix(l, "sni ") {
			t.Errorf("line %q", l)
		}
	}
}

func TestPick_OldNetworkNotTallied(t *testing.T) {
	p, _, _ := picking(t)
	st, gen, _ := p.pick("1.1.1.1", "")
	p.setNetKey("cell:25002")
	if _, log := p.result(gen, st.id, "1.1.1.1", "", p.now(), false, "x"); log {
		t.Fatal("old network's failure logged")
	}
	if _, log := p.unreachable(gen, "1.1.1.1"); log {
		t.Fatal("old network's unreachable logged")
	}
	if got := p.tallyNow(); got != (tally{}) {
		t.Fatalf("tally %+v", got)
	}
}

// A network under "nothing works" says so when it comes back.
func TestPick_NoneShownOnNetwork(t *testing.T) {
	p, _, file := picking(t)
	for range 2 * len(strategiesNow()) {
		try(p, "1.1.1.1", false)
	}
	out := captureLog(t)
	p.setNetKey("cell:25002")
	p.setNetKey("cell:25001")
	until := time.Unix(readMap(t, file)["cell:25001"].At, 0).Add(noneFor).Format("2006-01-02")
	want := "sni desync: network cell:25001, starting from rec2 (nothing works until " + until + ")"
	if !slices.Contains(out.lines, want) {
		t.Fatalf("lines %q", out.lines)
	}
}

// A dead address stays refused for deadFor from the first mark: later
// silent sessions do not extend it, and a mark from before a network
// switch does not count.
func TestMarkDead_WindowAndNetwork(t *testing.T) {
	captureLog(t)
	p := freshPicker(t)
	if !p.markDead(p.gen, "78.37.65.14") || !p.deadNow("78.37.65.14") {
		t.Fatal("not dead after the mark")
	}
	advance(p, deadFor-time.Second)
	p.markDead(p.gen, "78.37.65.14")
	advance(p, time.Second)
	if p.deadNow("78.37.65.14") {
		t.Fatal("a later mark extended the window")
	}
	gen := p.gen
	p.mu.Lock()
	p.resetLocked()
	p.mu.Unlock()
	if p.markDead(gen, "78.37.65.13") || p.deadNow("78.37.65.13") {
		t.Fatal("dead from an earlier network")
	}
}

// A dead address is kept for the network in the map: the next time on it
// (here after a restart) its connections are refused from the start, and
// the failure it caused is not held against the strategy.
func TestMarkDead_KeptForTheNetwork(t *testing.T) {
	p, _, file := picking(t)
	try(p, "1.1.1.1", false)
	if p.fails != 1 {
		t.Fatalf("fails %d", p.fails)
	}
	if !p.markDead(p.gen, "1.1.1.1") || p.fails != 0 {
		t.Fatalf("dead: fails %d", p.fails)
	}
	p.setNetKey("cell:25002")
	if p.deadNow("1.1.1.1") {
		t.Fatal("dead on another network")
	}

	q := freshPicker(t)
	q.started(file)
	q.setNetKey("cell:25001")
	if !q.deadNow("1.1.1.1") {
		t.Fatal("not dead after a restart")
	}
	advance(q, deadFor+time.Hour) // try moves the clock too
	q.setNetKey("cell:25002")
	q.setNetKey("cell:25001")
	if q.deadNow("1.1.1.1") {
		t.Fatalf("dead after %s", deadFor)
	}
}

// Before the winner is confirmed a node without a ServerHello is watched
// all the same, and its failure is forgiven once it turns out dead.
func TestNoHello_WatchedBeforeConfirmed(t *testing.T) {
	p, _, _ := picking(t)
	try(p, "1.1.1.1", false)
	if _, watched := p.silentNow("1.1.1.1"); !watched {
		t.Fatal("not watched before confirmed")
	}
	p.markDead(p.gen, "1.1.1.1")
	if p.fails != 0 {
		t.Fatalf("fails %d after the mark", p.fails)
	}
	// A try begun before the mark, reported after it, is not the strategy's.
	st, gen, _ := p.pick("2.2.2.2", "")
	p.markDead(p.gen, "2.2.2.2")
	p.result(gen, st.id, "2.2.2.2", "", p.now(), false, "silence")
	if p.fails != 0 {
		t.Fatalf("fails %d for a dead address", p.fails)
	}
}

// A success clears the failures together with what they were for: a later
// dead address forgives only its own.
func TestFailed_ClearedBySuccess(t *testing.T) {
	p, _, _ := picking(t)
	try(p, "1.1.1.1", false)
	try(p, "2.2.2.2", true)
	try(p, "3.3.3.3", false)
	p.markDead(p.gen, "1.1.1.1")
	if p.fails != 1 {
		t.Fatalf("fails %d: another address's failure forgiven", p.fails)
	}
}

// The player's asks for a dead address bring a probe at most every
// deadRecheck; an answer lifts the mark, also from the map, silence
// extends it.
func TestRecheck(t *testing.T) {
	p, _, file := picking(t)
	p.markDead(p.gen, "1.1.1.1")
	if _, due := p.recheckDue("1.1.1.1", true); due {
		t.Fatal("due right after the mark")
	}
	advance(p, deadRecheck)
	gen, due := p.recheckDue("1.1.1.1", true)
	if !due {
		t.Fatalf("not due %s after the mark", deadRecheck)
	}
	if _, again := p.recheckDue("1.1.1.1", true); again {
		t.Fatal("due twice")
	}
	advance(p, deadFor-deadRecheck-time.Minute)
	p.recheckDone(gen, "1.1.1.1", false, false)
	advance(p, deadFor/2) // past the first mark's end
	if !p.deadNow("1.1.1.1") {
		t.Fatal("silence did not extend the mark")
	}
	gen, due = p.recheckDue("1.1.1.1", true)
	if !due {
		t.Fatalf("not due after %s", deadRecheck)
	}
	p.recheckDone(gen, "1.1.1.1", false, true)
	if p.deadNow("1.1.1.1") || readMap(t, file)["cell:25001"].Dead["1.1.1.1"] != 0 {
		t.Fatal("still dead after an answer")
	}
}

// Two bursts to a node before it turns out dead: both failures forgiven.
func TestMarkDead_ForgivesEveryFailure(t *testing.T) {
	p, _, _ := picking(t)
	p.mu.Lock()
	p.won = true // three failures before the winner moves on
	p.mu.Unlock()
	try(p, "1.1.1.1", false)
	advance(p, time.Second)
	try(p, "1.1.1.1", false)
	if p.fails != 2 {
		t.Fatalf("fails %d before the mark", p.fails)
	}
	p.markDead(p.gen, "1.1.1.1")
	if p.fails != 0 {
		t.Fatalf("fails %d after the mark", p.fails)
	}
}

// The network's winner saved after a mark keeps the dead address in the map.
func TestMarkDead_SurvivesWinner(t *testing.T) {
	p, _, file := picking(t)
	p.markDead(p.gen, "9.9.9.9")
	try(p, "1.1.1.1", true)
	try(p, "2.2.2.2", true)
	e := readMap(t, file)["cell:25001"]
	if e.Win == "" || e.Dead["9.9.9.9"] == 0 {
		t.Fatalf("map entry %+v", e)
	}
}

func TestVideoCluster(t *testing.T) {
	for name, want := range map[string]string{
		"rr3---sn-oj5hn5-5g.googlevideo.com":   "sn-oj5hn5",
		"rr1---sn-5hne6nsz.googlevideo.com":    "sn-5hne6nsz",
		"RR3---SN-OJ5HN5-5T.GoogleVideo.com.":  "sn-oj5hn5",
		"rr3---sn-oj5hn5-5g.googlevideo.com.x": "",
		"rr3---sn-oj5hn5.example.com":          "",
		"redirector.googlevideo.com":           "",
		"rr3-sn-oj5hn5.googlevideo.com":        "",
		"rr3---sn-.googlevideo.com":            "",
		"rr3---xx-oj5hn5.googlevideo.com":      "",
		"a.rr3---sn-oj5hn5.googlevideo.com":    "",
		"":                                     "",
	} {
		if got := videoCluster(name); got != want {
			t.Errorf("%q: %q, want %q", name, got, want)
		}
	}
}

// Nodes of one cluster under its three suffixes, and another cluster.
const (
	node1, node2, node3, node4 = "rr1---sn-oj5hn5-5g.googlevideo.com", "rr2---sn-oj5hn5-59.googlevideo.com",
		"rr3---sn-oj5hn5-5t.googlevideo.com", "rr4---sn-oj5hn5-5g.googlevideo.com"
	otherNode = "rr1---sn-gvnuxaxjvh-axqz.googlevideo.com"
)

// winner confirms rec2 on names outside any cluster.
func winner(p *pickState) {
	tryAs(p, "1.1.1.1", "www.youtube.com", true)
	tryAs(p, "2.2.2.2", "youtubei.googleapis.com", true)
}

func raceOf(p *pickState, ip, name string) time.Duration {
	_, _, race := p.pick(ip, name)
	return race
}

// One silent address is no sign; the second makes the cluster's new
// addresses race the relay at once, the direct try still made.
func TestCluster_TwoSilentRaceAtOnce(t *testing.T) {
	p, _, _ := picking(t)
	out := captureLog(t)
	winner(p)
	tryAs(p, "10.0.0.1", node1, false)
	if r := raceOf(p, "10.0.0.3", node3); r != raceAfter {
		t.Fatalf("one silent: race after %s", r)
	}
	tryAs(p, "10.0.0.1", node1, false) // the same one again: still one
	tryAs(p, "10.0.0.2", node2, false)
	if !slices.Contains(out.lines, "sni desync: cluster sn-oj5hn5 silent (2 addresses) → relay races at once") {
		t.Fatalf("lines %q", out.lines)
	}
	st, _, race := p.pick("10.0.0.3", node3)
	if st == nil || race != 0 {
		t.Fatalf("silent cluster: %v, race after %s", st, race)
	}
	if r := raceOf(p, "10.0.0.9", otherNode); r != raceAfter {
		t.Fatalf("another cluster: race after %s", r)
	}
	if r := raceOf(p, "10.0.0.3", "www.youtube.com"); r != raceAfter {
		t.Fatalf("no cluster: race after %s", r)
	}
}

// Silent half an hour apart: the first one's mark is over, not a cluster.
func TestCluster_SilentFarApart(t *testing.T) {
	p, _, _ := picking(t)
	winner(p)
	tryAs(p, "10.0.0.1", node1, false)
	advance(p, silentFor)
	tryAs(p, "10.0.0.2", node2, false)
	if r := raceOf(p, "10.0.0.3", node3); r != raceAfter {
		t.Fatalf("race after %s", r)
	}
	// The second one took the first one's place.
	tryAs(p, "10.0.0.3", node3, false)
	if r := raceOf(p, "10.0.0.4", node4); r != 0 {
		t.Fatalf("two silent now: race after %s", r)
	}
}

// Wi-Fi → LTE: two of the cluster's addresses (the other network's
// caches) silent before any got through; the working one still answers
// direct and lifts the mark, whichever side served it.
func TestCluster_NetworkSwitchOkLifts(t *testing.T) {
	p, _, _ := picking(t)
	p.setNetKey("cell:25020")
	winner(p)
	tryAs(p, "78.37.65.14", node1, false)
	tryAs(p, "78.37.65.15", node2, false)
	id, race := tryAs(p, "178.66.83.10", node3, true)
	if id == "relay" || race != 0 {
		t.Fatalf("working node: %s, race after %s", id, race)
	}
	if r := raceOf(p, "178.66.83.11", node4); r != raceAfter {
		t.Fatalf("after the ok: race after %s", r)
	}
	tryAs(p, "78.37.65.16", node4, false)
	if r := raceOf(p, "178.66.83.12", node1); r != raceAfter {
		t.Fatalf("an ok cluster silent again: race after %s", r)
	}
}

// An ok counts for the cluster under any strategy; a strategy's change
// keeps what is known of the nodes, another network forgets it.
func TestCluster_KeptTillNetworkChange(t *testing.T) {
	p, _, _ := picking(t)
	winner(p)
	st, gen, _ := p.pick("10.0.0.9", otherNode)
	p.mu.Lock()
	p.cur = "dis-mid"
	p.mu.Unlock()
	p.result(gen, st.id, "10.0.0.9", otherNode, p.now(), true, "")
	p.mu.Lock()
	p.cur = st.id
	ok := p.okClusters.has("sn-gvnuxaxjvh")
	p.mu.Unlock()
	if !ok {
		t.Fatal("an ok of another strategy not counted")
	}
	tryAs(p, "10.0.0.1", node1, false)
	tryAs(p, "10.0.0.2", node2, false)
	p.mu.Lock()
	p.failLocked("x")
	p.restartLocked()
	kept := p.clusterSilent.has("sn-oj5hn5") && p.okClusters.has("sn-gvnuxaxjvh") && p.clusterFirst["sn-oj5hn5"] != ""
	p.resetLocked()
	gone := p.clusterSilent.len() == 0 && p.okClusters.len() == 0 && len(p.clusterFirst) == 0
	p.mu.Unlock()
	if !kept || !gone {
		t.Fatalf("kept %v, gone %v", kept, gone)
	}
}

// yt_race_ms: none is 600 ms, TTL 1 strategies 300 more, 0 no race at all.
func TestPick_RaceDelay(t *testing.T) {
	p, _, _ := picking(t)
	t.Cleanup(func() { SetRaceMs(-1) })
	winner(p)
	tryAs(p, "10.0.0.1", node1, false)
	tryAs(p, "10.0.0.2", node2, false)
	if r := raceOf(p, "10.0.0.5", "www.youtube.com"); r != 600*time.Millisecond {
		t.Fatalf("no key: %s", r)
	}
	SetRaceMs(1000)
	if r := raceOf(p, "10.0.0.5", "www.youtube.com"); r != time.Second {
		t.Fatalf("1000: %s", r)
	}
	SetRaceMs(0)
	if r := raceOf(p, "10.0.0.5", "www.youtube.com"); r >= 0 {
		t.Fatalf("0: %s", r)
	}
	if r := raceOf(p, "10.0.0.3", node3); r >= 0 {
		t.Fatalf("0, silent cluster: %s", r)
	}
	SetRaceMs(-5)
	SetStrategies(`[{"id":"dis-mid","spec":"cut=host+1,midsld;ttl1=2"}]`)
	if r := raceOf(p, "10.0.0.5", "www.youtube.com"); r != 900*time.Millisecond {
		t.Fatalf("ttl1: %s", r)
	}
	if r := raceOf(p, "10.0.0.3", node3); r != 0 {
		t.Fatalf("ttl1, silent cluster: %s", r)
	}
}

// addr is what the picker knows of ip, zero if nothing.
func (p *pickState) addr(ip string) addrInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	if a := p.addrLocked(ip); a != nil {
		return *a
	}
	return addrInfo{}
}

// diedEarly: a direct session of cur to ip got its ServerHello and died.
func diedEarly(p *pickState, ip, name string) {
	p.finished(p.gen, p.curID(), ip, name, finish{start: p.now(), dur: time.Second, down: 2 << 10, up: 600, serverFirst: true, why: "x"})
}

// directOnly: ip answered direct, then the relay was silent to it.
func directOnly(t *testing.T, p *pickState, ip, name string) {
	t.Helper()
	tryAs(p, ip, name, true)
	gen, watched := p.silentNow(ip)
	if !watched || !p.relayNoAnswer(gen, ip, false) {
		t.Fatalf("watched %v", watched)
	}
}

// Tele2 LTE: its own cache answered direct, died early, and the relay from
// abroad does not reach it. Direct only, not dead; it races no leg and
// its early deaths stay direct.
func TestAddr_RelaySilentAfterHelloIsDirectOnly(t *testing.T) {
	p, _, file := picking(t)
	out := captureLog(t)
	winner(p)
	const ip = "178.66.83.79"
	tryAs(p, ip, node1, true)
	diedEarly(p, ip, node1)
	if got, _ := tryAs(p, ip, node1, true); got != "relay" {
		t.Fatalf("after the early death: %s", got)
	}
	gen, watched := p.silentNow(ip)
	if !watched || !p.relayNoAnswer(gen, ip, false) {
		t.Fatalf("watched %v", watched)
	}
	if p.deadNow(ip) || readMap(t, file)["cell:25001"].Dead[ip] != 0 || !p.mutedNow(ip) {
		t.Fatalf("dead %v, muted %v", p.deadNow(ip), p.mutedNow(ip))
	}
	if !slices.Contains(out.lines, "sni desync: 178.66.83.79: relay does not reach it → direct only") {
		t.Fatalf("lines %q", out.lines)
	}
	if _, watched := p.silentNow(ip); watched {
		t.Fatal("watched while direct only")
	}
	for range 2 {
		st, _, race := p.pick(ip, node1)
		if st == nil || race >= 0 {
			t.Fatalf("direct only: %v, race after %s", st, race)
		}
		diedEarly(p, ip, node1)
	}
}

// One that never answered direct, the relay silent with the VDS alive:
// dead, as before; a fast end of its session is not enough for that.
func TestAddr_RelaySilentWithoutHelloIsDead(t *testing.T) {
	p, _, _ := picking(t)
	tryAs(p, "1.1.1.1", node1, false)
	gen, watched := p.silentNow("1.1.1.1")
	if !watched {
		t.Fatal("not watched")
	}
	if p.relayNoAnswer(gen, "1.1.1.1", true) || p.deadNow("1.1.1.1") {
		t.Fatal("dead by a fast end")
	}
	if !p.relayNoAnswer(gen, "1.1.1.1", false) || !p.deadNow("1.1.1.1") || p.mutedNow("1.1.1.1") {
		t.Fatal("not dead")
	}
}

// Direct only and the direct try fails too (no ServerHello, or no TCP):
// dead at once, not the strategy's, and the first refused connection
// probes it: one miss heals at once.
func TestAddr_DirectOnlyFailsIsDead(t *testing.T) {
	p, _, file := picking(t)
	out := captureLog(t)
	winner(p)
	directOnly(t, p, "3.3.3.3", node1)
	fails, attempts := p.fails, p.attempts
	tryAs(p, "3.3.3.3", node1, false)
	if !p.deadNow("3.3.3.3") || readMap(t, file)["cell:25001"].Dead["3.3.3.3"] == 0 {
		t.Fatal("not dead")
	}
	if p.fails != fails || p.attempts != attempts {
		t.Fatalf("fails %d, attempts %d: counted", p.fails, p.attempts)
	}
	if !slices.Contains(out.lines, "sni desync: 3.3.3.3: direct fails too, relay never reached it → dead") {
		t.Fatalf("lines %q", out.lines)
	}
	gen, due := p.recheckDue("3.3.3.3", true)
	if !due {
		t.Fatal("no recheck at once")
	}
	p.recheckDone(gen, "3.3.3.3", true, false)
	if p.deadNow("3.3.3.3") || readMap(t, file)["cell:25001"].Dead["3.3.3.3"] != 0 || p.addr("3.3.3.3").hello {
		t.Fatalf("after the recheck: %+v", p.addr("3.3.3.3"))
	}

	directOnly(t, p, "4.4.4.4", node2)
	_, gen, _ = p.pick("4.4.4.4", node2)
	p.unreachable(gen, "4.4.4.4")
	if !p.deadNow("4.4.4.4") {
		t.Fatal("no TCP: not dead")
	}
}

// A network whose caches the relay does not reach: their early deaths are
// no strategy's, the winner stays.
func TestAddr_DirectOnlyEarlyDeaths(t *testing.T) {
	p, _, _ := picking(t)
	winner(p)
	for _, ip := range []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"} {
		directOnly(t, p, ip, node1)
		advance(p, time.Second)
		diedEarly(p, ip, node1)
		if got, _ := tryAs(p, ip, node1, true); got == "relay" {
			t.Fatalf("%s by relay", ip)
		}
	}
	if p.fails != 0 || p.curID() != "rec2" || p.verdict() != verdictWorks || p.tallyNow().empty != 3 {
		t.Fatalf("fails %d, on %s, %s, %+v", p.fails, p.curID(), p.verdict(), p.tallyNow())
	}
}

// Direct only lasts silentFor: then an early death goes by relay again.
func TestAddr_DirectOnlyEnds(t *testing.T) {
	p, _, _ := picking(t)
	winner(p)
	directOnly(t, p, "3.3.3.3", node1)
	advance(p, silentFor)
	if _, watched := p.silentNow("3.3.3.3"); !watched || p.mutedNow("3.3.3.3") {
		t.Fatal("still direct only")
	}
	diedEarly(p, "3.3.3.3", node1)
	if got, _ := tryAs(p, "3.3.3.3", node1, true); got != "relay" {
		t.Fatalf("early death: %s", got)
	}
}

// One that answers direct races no leg, even in a silent cluster; others do.
func TestAddr_HelloNoRace(t *testing.T) {
	p, _, _ := picking(t)
	winner(p)
	tryAs(p, "10.0.0.3", node3, true)
	p.mu.Lock()
	p.clusterSilent.put("sn-oj5hn5", p.now().Add(silentFor))
	p.mu.Unlock()
	if r := raceOf(p, "10.0.0.3", node3); r >= 0 {
		t.Fatalf("hello, silent cluster: race after %s", r)
	}
	if r := raceOf(p, "10.0.0.1", node1); r != 0 {
		t.Fatalf("silent cluster: race after %s", r)
	}
	tryAs(p, "10.0.0.4", "www.youtube.com", true)
	if r := raceOf(p, "10.0.0.4", "www.youtube.com"); r >= 0 {
		t.Fatalf("hello: race after %s", r)
	}
	if r := raceOf(p, "10.0.0.5", "www.youtube.com"); r != raceAfter {
		t.Fatalf("another: race after %s", r)
	}
}

// Another strategy forgets what cur did; what is known of the addresses
// stays. Another network forgets it all but its dead from the map.
func TestAddr_StrategyAndNetworkChange(t *testing.T) {
	p, _, _ := picking(t)
	winner(p)
	directOnly(t, p, "3.3.3.3", node1)
	tryAs(p, "4.4.4.4", "", false)
	p.markDead(p.gen, "5.5.5.5")
	p.mu.Lock()
	p.ips["1.1.1.1"].failed = 1
	p.fails = needFailsWon - 1
	p.failLocked("x")
	p.mu.Unlock()
	if p.curID() == "rec2" {
		t.Fatal("strategy kept")
	}
	if a := p.addr("1.1.1.1"); a.curOK || a.failed != 0 || !a.hello {
		t.Fatalf("1.1.1.1 %+v", a)
	}
	if !p.mutedNow("3.3.3.3") || !p.addr("3.3.3.3").hello || !p.now().Before(p.addr("4.4.4.4").relayUntil) || !p.deadNow("5.5.5.5") {
		t.Fatal("lost on a strategy change")
	}
	p.setNetKey("cell:25002")
	for _, ip := range []string{"1.1.1.1", "3.3.3.3", "4.4.4.4", "5.5.5.5"} {
		if a := p.addr(ip); a != (addrInfo{}) {
			t.Fatalf("%s on a new network: %+v", ip, a)
		}
	}
	p.setNetKey("cell:25001")
	if !p.deadNow("5.5.5.5") || p.addr("3.3.3.3") != (addrInfo{}) {
		t.Fatal("back: not as in the map")
	}
}

// What is pending outlives addrMemory addresses that got through.
func TestAddr_EvictionKeepsPending(t *testing.T) {
	p, _, _ := picking(t)
	winner(p)
	tryAs(p, "9.0.0.1", "", false)
	directOnly(t, p, "9.0.0.2", "")
	p.markDead(p.gen, "9.0.0.3")
	for i := range addrMemory {
		tryAs(p, fmt.Sprintf("10.9.0.%d", i), "", true)
	}
	if !p.now().Before(p.addr("9.0.0.1").relayUntil) || !p.mutedNow("9.0.0.2") || !p.deadNow("9.0.0.3") {
		t.Fatal("dropped")
	}
	p.mu.Lock()
	n := len(p.ips)
	p.mu.Unlock()
	if n != addrMemory+1 || p.addr("1.1.1.1") != (addrInfo{}) {
		t.Fatalf("%d kept", n)
	}
}

// A burst of connections to a direct only address that all fail to
// connect: dead by the first, the rest only counted; a direct answer to
// the recheck brings it back direct.
func TestAddr_UnreachableBurstUnderMute(t *testing.T) {
	p, _, _ := picking(t)
	winner(p)
	directOnly(t, p, "4.4.4.4", node2)
	_, gen, _ := p.pick("4.4.4.4", node2)
	p.unreachable(gen, "4.4.4.4")
	p.unreachable(gen, "4.4.4.4")
	g, due := p.recheckDue("4.4.4.4", true)
	if !due {
		t.Fatal("not due")
	}
	p.recheckDone(g, "4.4.4.4", true, false)
	if st, _, _ := p.pick("4.4.4.4", node2); st == nil {
		t.Fatalf("lifted by a direct probe, yet by relay: %+v", p.addr("4.4.4.4"))
	}
}

// Never answered direct: one miss under the winner sends it by relay, the
// relay is silent too, dead. A direct answer to the recheck ends its time
// by relay with the mark.
func TestAddr_DirectLiftEndsRelay(t *testing.T) {
	p, _, _ := picking(t)
	winner(p)
	tryAs(p, "7.7.7.7", node1, false)
	gen, _ := p.silentNow("7.7.7.7")
	p.relayNoAnswer(gen, "7.7.7.7", false)
	advance(p, deadRecheck)
	g, due := p.recheckDue("7.7.7.7", true)
	if !due {
		t.Fatal("not due")
	}
	p.recheckDone(g, "7.7.7.7", true, false)
	if st, _, _ := p.pick("7.7.7.7", node1); st == nil {
		t.Fatalf("lifted by a direct probe, yet by relay: %+v", p.addr("7.7.7.7"))
	}
}

// Offline (no route out): a direct-only address's failed dial is this
// side's, not the address's. It stays direct only, not dead.
func TestAddr_OfflineKeepsDirectOnly(t *testing.T) {
	p, _, file := picking(t)
	winner(p)
	directOnly(t, p, "3.3.3.3", node1)
	fails, attempts := p.fails, p.attempts
	err := fmt.Errorf("%w: %w", errNoConnect, &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ENETUNREACH)})
	desyncVerdict(p.gen, &strategy{id: p.cur}, "3.3.3.3", node1, "3.3.3.3:443", p.now(), err)
	if p.deadNow("3.3.3.3") || !p.mutedNow("3.3.3.3") || readMap(t, file)["cell:25001"].Dead["3.3.3.3"] != 0 {
		t.Fatalf("after an offline dial: %+v", p.addr("3.3.3.3"))
	}
	if p.fails != fails || p.attempts != attempts {
		t.Fatalf("fails %d, attempts %d: counted", p.fails, p.attempts)
	}
}

// A recheck while the relay cannot take a probe is direct only: it does
// not use up the turn, the relay's return brings one at once.
func TestRecheck_RelayDownKeepsTurn(t *testing.T) {
	p, _, _ := picking(t)
	p.markDead(p.gen, "1.1.1.1")
	advance(p, deadRecheck)
	if _, due := p.recheckDue("1.1.1.1", false); !due {
		t.Fatal("no direct-only recheck")
	}
	if _, due := p.recheckDue("1.1.1.1", false); due {
		t.Fatal("direct-only recheck twice")
	}
	if _, due := p.recheckDue("1.1.1.1", true); !due {
		t.Fatal("no recheck once the relay is back")
	}
	if _, due := p.recheckDue("1.1.1.1", true); due {
		t.Fatal("due twice")
	}
}

// The relay looked up at the SYN but could not take the probe after all:
// the turn is not used up, the next SYN with the relay probes.
func TestRecheck_RelayProbeLostKeepsTurn(t *testing.T) {
	p, _, _ := picking(t)
	p.markDead(p.gen, "1.1.1.1")
	advance(p, deadRecheck)
	gen, due := p.recheckDue("1.1.1.1", true)
	if !due {
		t.Fatal("not due")
	}
	p.recheckNoRelay(gen, "1.1.1.1")
	if _, due := p.recheckDue("1.1.1.1", false); due {
		t.Fatal("direct-only recheck right after")
	}
	if _, due := p.recheckDue("1.1.1.1", true); !due {
		t.Fatal("no recheck with the relay")
	}
	if _, due := p.recheckDue("1.1.1.1", true); due {
		t.Fatal("due twice")
	}
}
