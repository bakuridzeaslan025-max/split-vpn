// Tests for ../download.js: node:test only, no DOM and no npm packages (make -C android page-test).
import {test, mock, afterEach} from "node:test";
import assert from "node:assert/strict";
import {probe, pickUrl, attach, releaseInfo, lastResort, MIRRORS, FIXED, PROBE_FILE} from "../download.js";

const A = {base: "https://a.test/", probe: true};
const B = {base: "https://b.test/", probe: true};
const R = {base: "https://r.test/", probe: false};
const mirrors = [A, B, R];
const V = "split-vpn-1.2.3.apk";

const unhandled = [];
process.on("unhandledRejection", e => unhandled.push(e));

const flush = async () => { for (let i = 0; i < 20; i++) await new Promise(r => setImmediate(r)); };

// Mocked time forward in steps, letting streams react between them.
async function advance(ms, step = 50) {
  for (let t = 0; t < ms; t += step) {
    mock.timers.tick(Math.min(step, ms - t));
    await flush();
  }
}

const mockTime = () => mock.timers.enable({apis: ["setTimeout", "Date"]});

afterEach(async () => {
  await flush();
  mock.timers.reset();
  assert.deepEqual(unhandled.splice(0), [], "unhandled rejections");
});

// What a host does with a request. A HEAD on a body route answers 200.
const ok = (size = 32_768) => ({kind: "body", size});
const stall = (size = 16_700) => ({kind: "stall", size});
// `chunk` bytes every `every` ms, `count` times, then silence or, with reset, a failure.
const drip = (chunk, every, count = Infinity, reset = false) => ({kind: "drip", chunk, every, count, reset});
const status = code => ({kind: "status", code});
const refused = {kind: "refused"};
const hang = {kind: "hang"};

// The routes of one mirror: HEAD on both names and the probe.bin GET.
const mirror = (m, {v = status(200), f = status(200), bin = ok()} = {}) =>
  ({[m.base + V]: v, [m.base + FIXED]: f, [m.base + PROBE_FILE]: bin});

// A fetch over routes {url: behavior}; unknown URLs answer 404. Like the real one, an
// abort rejects a pending fetch and errors a body being read.
function fakeFetch(routes) {
  const calls = [];
  const fetch = (url, init) => {
    const call = {url, init, method: init.method || "GET", cancelled: false};
    calls.push(call);
    const route = routes[url] || status(404);
    const {signal} = init;
    return new Promise((resolve, reject) => {
      const aborted = () => new DOMException("This operation was aborted", "AbortError");
      if (signal.aborted) return reject(aborted());
      let body;
      signal.addEventListener("abort", () => {
        reject(aborted());
        try { body.error(aborted()); } catch {}
      });
      if (route.kind === "refused") return reject(new TypeError("fetch failed"));
      if (route.kind === "hang") return;
      if (route.kind === "status") return resolve(new Response(null, {status: route.code}));
      if (call.method === "HEAD") return resolve(new Response(null, {status: 200}));
      let sent = 0, chunks = 0;
      const stream = new ReadableStream({
        start(c) { body = c; },
        pull(c) {
          if (route.kind === "drip") {
            if (chunks >= route.count) return route.reset ? c.error(new TypeError("network error")) : new Promise(() => {});
            return new Promise(done => setTimeout(() => {
              chunks++;
              try { c.enqueue(new Uint8Array(route.chunk)); } catch {}
              done();
            }, route.every));
          }
          if (sent >= route.size) {
            if (route.kind === "stall") return new Promise(() => {});
            return c.close();
          }
          const n = Math.min(4096, route.size - sent);
          sent += n;
          c.enqueue(new Uint8Array(n));
        },
        cancel() { call.cancelled = true; },
      }, {highWaterMark: 0});
      resolve(new Response(stream, {status: 200}));
    });
  };
  return {fetch, calls};
}

function settled(promise) {
  const state = {done: false};
  promise.then(v => { state.done = true; state.value = v; }, e => { state.done = true; state.error = e; });
  return state;
}

function pick(routes, opts = {}) {
  const f = fakeFetch(routes);
  return {calls: f.calls, result: pickUrl({mirrors, fetch: f.fetch, streams: true, ...opts})};
}

const signal = () => new AbortController().signal;

test("probe: 32 KB passes, read only to 24 KB, as a GET without extra headers", async () => {
  const {fetch, calls} = fakeFetch({"https://a.test/p": ok()});
  const state = {};
  assert.equal(await probe("https://a.test/p", {fetch, signal: signal(), state}), true);
  assert.equal(state.bytes, 24_576);
  assert.deepEqual(Object.keys(calls[0].init).sort(), ["cache", "signal"]);
  assert.equal(calls[0].init.cache, "no-store");
  assert.ok(calls[0].cancelled);
});

test("probe: a complete body short of 24 KB fails, one of 24 KB passes", async () => {
  const {fetch} = fakeFetch({"https://a.test/short": ok(24_575), "https://a.test/empty": ok(0), "https://a.test/p": ok(24_576)});
  const state = {};
  assert.equal(await probe("https://a.test/short", {fetch, signal: signal(), state}), false);
  assert.equal(state.ended, true);
  assert.equal(await probe("https://a.test/empty", {fetch, signal: signal()}), false);
  assert.equal(await probe("https://a.test/p", {fetch, signal: signal()}), true);
});

test("pickUrl: a truncated probe.bin is no pass and no target", async () => {
  const {result} = pick({...mirror(A, {bin: ok(1000)}), ...mirror(B, {v: refused, f: refused, bin: refused})}, {versioned: V});
  assert.deepEqual(await result, {url: "https://r.test/" + V, checked: false});
});

test("probe: an HTTP error fails, with http set", async () => {
  const {fetch} = fakeFetch({"https://a.test/p": status(504)});
  const state = {};
  assert.equal(await probe("https://a.test/p", {fetch, signal: signal(), state}), false);
  assert.equal(state.http, true);
  assert.equal(state.ended, true);
});

test("probe: a refused connection fails, without http", async () => {
  const {fetch} = fakeFetch({"https://a.test/p": refused});
  const state = {};
  assert.equal(await probe("https://a.test/p", {fetch, signal: signal(), state}), false);
  assert.equal(state.http, false);
});

test("probe: 16 700 bytes and then a stall fails once aborted", async () => {
  const {fetch} = fakeFetch({"https://a.test/p": stall()});
  const abort = new AbortController();
  const state = {};
  const result = settled(probe("https://a.test/p", {fetch, signal: abort.signal, state}));
  await flush();
  assert.equal(result.done, false);
  assert.equal(state.bytes, 16_700);
  assert.equal(state.ended, false);
  abort.abort();
  await flush();
  assert.equal(result.value, false);
  assert.equal(state.ended, true);
});

test("pickUrl: traffic is one probe.bin GET per Pages mirror and HEADs for the names", async () => {
  const {calls, result} = pick({...mirror(A), ...mirror(B)}, {versioned: V});
  assert.deepEqual(await result, {url: "https://a.test/" + V, checked: true});
  const gets = calls.filter(c => c.method === "GET");
  assert.deepEqual(gets.map(c => c.url).sort(), ["https://a.test/probe.bin", "https://b.test/probe.bin"]);
  for (const c of gets) assert.deepEqual(Object.keys(c.init).sort(), ["cache", "signal"]);
  const heads = calls.filter(c => c.method === "HEAD");
  assert.deepEqual(heads.map(c => c.url).sort(),
    ["https://a.test/" + V, "https://a.test/" + FIXED, "https://b.test/" + V, "https://b.test/" + FIXED].sort());
  for (const c of heads) assert.equal(c.init.cache, "no-store");
  assert.ok(!calls.some(c => c.url.startsWith(R.base)));
});

test("pickUrl: the versioned name on every mirror comes before any fixed one", async () => {
  const {result} = pick({...mirror(A, {v: status(404)}), ...mirror(B)}, {versioned: V});
  assert.deepEqual(await result, {url: "https://b.test/" + V, checked: true});
});

test("pickUrl: without a version only the fixed name is asked for", async () => {
  const {calls, result} = pick({...mirror(A, {bin: refused}), ...mirror(B)});
  assert.deepEqual(await result, {url: "https://b.test/" + FIXED, checked: true});
  assert.deepEqual(calls.filter(c => c.method === "HEAD").map(c => c.url),
    ["https://a.test/" + FIXED, "https://b.test/" + FIXED]);
});

test("pickUrl: versioned HEAD 404 with a good probe gives the fixed name there", async () => {
  const {result} = pick({...mirror(A, {v: status(404)}), ...mirror(B, {bin: refused})}, {versioned: V});
  assert.deepEqual(await result, {url: "https://a.test/" + FIXED, checked: true});
});

test("pickUrl: a good probe with HEAD 404 on both names is no pass", async () => {
  const {result} = pick({...mirror(A, {v: status(404), f: status(404)}), ...mirror(B, {v: refused, f: refused, bin: refused})},
    {versioned: V});
  assert.deepEqual(await result, {url: "https://r.test/" + V, checked: false});
});

test("pickUrl: a probe stalled at 16 700 bytes waits for the deadline, then a lower mirror wins", async () => {
  mockTime();
  const {calls, result} = pick({...mirror(A, {bin: stall()}), ...mirror(B)});
  const state = settled(result);
  await advance(3999);
  assert.equal(state.done, false);
  await advance(1);
  assert.deepEqual(state.value, {url: "https://b.test/" + FIXED, checked: true});
  assert.ok(calls.every(c => c.init.signal.aborted));
});

test("pickUrl: a stalled mirror is never a target: Releases unchecked", async () => {
  mockTime();
  const state = settled(pick({...mirror(A, {bin: stall()}), ...mirror(B, {v: refused, f: refused, bin: refused})},
    {versioned: V}).result);
  await advance(4000);
  assert.deepEqual(state.value, {url: "https://r.test/" + V, checked: false});
});

test("pickUrl: a slow link opens the alive mirror with more bytes, unchecked", async () => {
  mockTime();
  const state = settled(pick({...mirror(A, {bin: drip(600, 400)}), ...mirror(B, {bin: drip(1000, 400)})},
    {versioned: V}).result);
  await advance(4000);
  assert.deepEqual(state.value, {url: "https://b.test/" + V, checked: false});
});

test("pickUrl: alive with fewer bytes beats stalled with more", async () => {
  mockTime();
  const state = settled(pick({...mirror(A, {bin: stall(16_300)}), ...mirror(B, {bin: drip(1000, 400)})}).result);
  await advance(4000);
  assert.deepEqual(state.value, {url: "https://b.test/" + FIXED, checked: false});
});

test("pickUrl: an alive mirror with versioned HEAD 404 opens the fixed name", async () => {
  mockTime();
  const state = settled(pick({...mirror(A, {v: status(404), bin: drip(1000, 400)}), ...mirror(B, {bin: refused})},
    {versioned: V}).result);
  await advance(4000);
  assert.deepEqual(state.value, {url: "https://a.test/" + FIXED, checked: false});
});

test("pickUrl: a host silent for over 1.5 s before the deadline is stalled", async () => {
  mockTime();
  const state = settled(pick({...mirror(A, {bin: drip(1000, 400, 5)}), ...mirror(B, {v: refused, f: refused, bin: refused})},
    {versioned: V}).result);
  await advance(4000);
  assert.deepEqual(state.value, {url: "https://r.test/" + V, checked: false});
});

test("pickUrl: an alive mirror with no answer to HEAD opens the fixed name", async () => {
  mockTime();
  const state = settled(pick({...mirror(A, {v: hang, f: hang, bin: drip(1000, 400)}), ...mirror(B, {bin: refused})},
    {versioned: V}).result);
  await advance(4000);
  assert.deepEqual(state.value, {url: "https://a.test/" + FIXED, checked: false});
});

test("pickUrl: a probe that failed just before the deadline is not alive", async () => {
  mockTime();
  const net = {v: refused, f: refused};
  const state = settled(pick({...mirror(A, {...net, bin: drip(1000, 400, 8, true)}), ...mirror(B, {...net, bin: hang})}).result);
  await advance(4000);
  assert.deepEqual(state.value, {url: "https://r.test/" + FIXED, checked: false});
});

test("pickUrl: an HTTP error on probe.bin alone still makes Releases the last resort", async () => {
  const net = {v: refused, f: refused};
  const {result} = pick({...mirror(A, {...net, bin: status(504)}), ...mirror(B, {...net, bin: refused})});
  assert.deepEqual(await result, {url: "https://r.test/" + FIXED, checked: false});
});

test("pickUrl: HTTP errors only make Releases the unchecked last resort", async () => {
  const {result} = pick({...mirror(A, {v: status(504), f: status(504), bin: status(504)}),
    ...mirror(B, {v: refused, f: refused, bin: refused})}, {versioned: V});
  assert.deepEqual(await result, {url: "https://r.test/" + V, checked: false});
});

test("pickUrl: all net gives no URL, at once when refused, at the deadline when hanging", async () => {
  const net = {v: refused, f: refused, bin: refused};
  assert.deepEqual(await pick({...mirror(A, net), ...mirror(B, net)}, {versioned: V}).result, {url: null, checked: false});
  mockTime();
  const state = settled(pick({...mirror(A, {v: hang, f: hang, bin: hang}), ...mirror(B, net)}, {versioned: V}).result);
  await advance(3999);
  assert.equal(state.done, false);
  await advance(1);
  assert.deepEqual(state.value, {url: null, checked: false});
});

test("pickUrl: the winner aborts the rest without waiting for the timeout", async () => {
  mockTime();
  const {calls, result} = pick({...mirror(A), ...mirror(B, {bin: stall(), v: hang, f: hang})});
  assert.deepEqual(await result, {url: "https://a.test/" + FIXED, checked: true});
  await flush();
  assert.ok(calls.every(c => c.init.signal.aborted));
  assert.ok(calls.find(c => c.url === "https://a.test/probe.bin").cancelled);
});

test("pickUrl: without response streams it throws before any request", async () => {
  const {calls, result} = pick({}, {streams: false});
  await assert.rejects(result);
  assert.equal(calls.length, 0);
});

test("releaseInfo: the versioned name and the meta line", () => {
  const rel = {
    tag_name: "v0.12.3",
    published_at: "2026-11-08T12:00:00Z",
    assets: [{name: "split-vpn-0.12.3.apk", size: 1}, {name: FIXED, size: 12_884_901}],
  };
  assert.deepEqual(releaseInfo(rel), {versioned: "split-vpn-0.12.3.apk", meta: "Версия 0.12.3 · 12,9 МБ · 8 ноября"});
  assert.equal(releaseInfo({...rel, tag_name: "v1.0"}).versioned, null);
  assert.equal(releaseInfo({...rel, assets: [{name: "a.sha256", size: 1}]}).meta, null);
  assert.match(releaseInfo({...rel, assets: [{name: "other.apk", size: 2e6}]}).meta, / 2 МБ /);
});

test("lastResort: Releases, with the versioned name when known", () => {
  assert.equal(lastResort(mirrors, null), "https://r.test/" + FIXED);
  assert.equal(lastResort(mirrors, V), "https://r.test/" + V);
  assert.deepEqual(MIRRORS.map(m => m.probe), [true, true, false]);
});

// A page of fake elements around attach(); pickUrl() answers with `answer`.
function page(answer) {
  const el = props => {
    const attrs = new Map(), on = {};
    return {
      hidden: false, href: "", textContent: "", ...props, on,
      setAttribute: (k, v) => attrs.set(k, String(v)),
      removeAttribute: k => attrs.delete(k),
      hasAttribute: k => attrs.has(k),
      addEventListener: (type, f) => (on[type] ||= []).push(f),
    };
  };
  const dl = el({textContent: "Скачать для Android", href: "https://a.test/" + FIXED});
  const fail = el({hidden: true});
  const direct = el({href: "https://r.test/" + FIXED});
  const win = {};
  const p = {dl, fail, direct, navigated: [], picks: 0};
  attach({
    dl, fail, direct,
    pickUrl: async () => { p.picks++; return typeof answer === "function" ? answer() : answer; },
    fallback: () => "https://r.test/" + V,
    navigate: url => p.navigated.push(url),
    setTimeout: (...a) => setTimeout(...a),
    clearTimeout: t => clearTimeout(t),
    addEventListener: (type, f) => (win[type] ||= []).push(f),
  });
  p.click = (mods = {}) => {
    const e = {prevented: false, preventDefault() { this.prevented = true; }, ...mods};
    return {e, done: Promise.all(dl.on.click.map(f => f(e)))};
  };
  p.pageshow = persisted => win.pageshow.forEach(f => f({persisted}));
  p.busy = () => dl.hasAttribute("aria-busy");
  return p;
}

test("button: a checked mirror opens at once, no note", async () => {
  const p = page({url: "https://a.test/" + V, checked: true});
  const {e, done} = p.click();
  assert.ok(e.prevented);
  assert.ok(p.busy());
  assert.equal(p.dl.textContent, "Секунду…");
  await done;
  assert.deepEqual(p.navigated, ["https://a.test/" + V]);
  assert.equal(p.fail.hidden, true);
  assert.equal(p.busy(), false);
  assert.equal(p.dl.textContent, "Скачать для Android");
  assert.equal(p.direct.href, "https://r.test/" + V);
});

test("button: all net shows the note at once and opens nothing", async () => {
  const p = page({url: null, checked: false});
  await p.click().done;
  assert.deepEqual(p.navigated, []);
  assert.equal(p.fail.hidden, false);
  assert.equal(p.busy(), false);
});

test("button: Releases unchecked stays busy, the note and release come after 10 s", async () => {
  mock.timers.enable({apis: ["setTimeout"]});
  const p = page({url: "https://r.test/" + V, checked: false});
  await p.click().done;
  assert.deepEqual(p.navigated, ["https://r.test/" + V]);
  assert.ok(p.busy());
  assert.equal(p.dl.textContent, "Скачиваем…");
  assert.equal(p.fail.hidden, true);
  mock.timers.tick(9999);
  assert.ok(p.busy());
  assert.equal(p.fail.hidden, true);
  mock.timers.tick(1);
  assert.equal(p.busy(), false);
  assert.equal(p.fail.hidden, false);
  assert.equal(p.dl.textContent, "Скачать для Android");
});

test("button: a second tap while busy is ignored", async () => {
  mock.timers.enable({apis: ["setTimeout"]});
  let release;
  const p = page(() => new Promise(r => { release = r; }));
  const first = p.click();
  const during = p.click();
  await during.done;
  assert.ok(during.e.prevented);
  release({url: "https://r.test/" + V, checked: false});
  await first.done;
  const after = p.click();
  await after.done;
  assert.ok(after.e.prevented);
  assert.equal(p.picks, 1);
  assert.deepEqual(p.navigated, ["https://r.test/" + V]);
});

test("button: a modified click is left to the browser", async () => {
  const p = page({url: "https://a.test/" + V, checked: true});
  for (const mod of ["ctrlKey", "metaKey", "shiftKey", "altKey"]) {
    const {e, done} = p.click({[mod]: true});
    await done;
    assert.equal(e.prevented, false);
  }
  assert.equal(p.picks, 0);
  assert.equal(p.busy(), false);
});

test("button: an old engine falls back to the plain link", async () => {
  const p = page(() => pickUrl({mirrors, fetch: fakeFetch({}).fetch, streams: false}));
  await p.click().done;
  assert.deepEqual(p.navigated, [p.dl.href]);
  assert.equal(p.busy(), false);
  assert.equal(p.fail.hidden, true);
});

test("button: a new tap hides the previous note", async () => {
  let answer = {url: null, checked: false};
  const p = page(() => answer);
  await p.click().done;
  assert.equal(p.fail.hidden, false);
  answer = {url: "https://a.test/" + V, checked: true};
  await p.click().done;
  assert.equal(p.fail.hidden, true);
});

test("button: back from the error page shows the note at once", async () => {
  mock.timers.enable({apis: ["setTimeout"]});
  const p = page({url: "https://r.test/" + V, checked: false});
  p.pageshow(true);
  assert.equal(p.fail.hidden, true);
  await p.click().done;
  p.pageshow(false);
  assert.ok(p.busy());
  p.pageshow(true);
  assert.equal(p.busy(), false);
  assert.equal(p.fail.hidden, false);
  p.fail.hidden = true;
  mock.timers.tick(10_000);
  assert.equal(p.fail.hidden, true);
});
