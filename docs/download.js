// The download button of index.html. An ES module: an engine without modules never runs
// it, and the button stays a plain link. Logic takes its globals as arguments, so
// test/download.test.mjs runs it in node without a DOM; start() wires it to the page.

// Download mirrors in priority order; each has split-vpn.apk and split-vpn-<version>.apk,
// the Pages ones also probe.bin (android/Makefile, PAGES_FILES).
// probe: false where a page cannot read the answer (Releases: the 302 to another host
// fails CORS, and no-cors hides a 504), so such a mirror is only the unchecked last resort.
export const MIRRORS = [
  {base: "https://bakuridzeaslan025-max.github.io/split-vpn-dl/", probe: true},
  {base: "https://split-vpn-dl-547fcb.gitlab.io/", probe: true},
  {base: "https://github.com/bakuridzeaslan025-max/split-vpn/releases/latest/download/", probe: false},
];
export const FIXED = "split-vpn.apk";
export const PROBE_FILE = "probe.bin";
export const PROBE_BYTES = 24576;
export const PROBE_TIMEOUT_MS = 4000;
export const ALIVE_MS = 1500;
export const FAIL_DELAY_MS = 10000;

// The versioned file name and the meta line from the API's latest release.
export function releaseInfo(rel) {
  const version = rel.tag_name.replace(/^v/, "");
  const versioned = /^\d+\.\d+\.\d+$/.test(version) ? `split-vpn-${version}.apk` : null;
  const apk = rel.assets.find(a => a.name === FIXED) || rel.assets.find(a => a.name.endsWith(".apk"));
  if (!apk) return {versioned, meta: null};
  const mb = (apk.size / 1e6).toLocaleString("ru-RU", {maximumFractionDigits: 1});
  const date = new Date(rel.published_at).toLocaleDateString("ru-RU", {day: "numeric", month: "long"});
  return {versioned, meta: `Версия ${version} · ${mb} МБ · ${date}`};
}

// Releases' latest/download/split-vpn.apk is always the newest release, with no
// cache in the way as on Pages: the fixed name, whatever the version.
export const lastResort = mirrors => mirrors.find(m => !m.probe).base + FIXED;

// probe.bin on a mirror: 32 KB of random bytes. Throttled mobile networks let ~16 KB
// of a host through and then stall, so an answer to HEAD alone proves nothing; the
// whole APK is too much (on a fast link parallel GETs stream megabytes before the abort
// lands). No Range header: it would turn the GET into a preflighted CORS request.
// Resolves to true once `bytes` arrived; false on an HTTP error, a failure, the abort
// or a body that ends short (probe.bin is always 32 KB, so that one is broken). Fills `state`: http (an HTTP answer came), bytes, last (the
// time of the last chunk), ended.
export function probe(url, {fetch, signal, bytes = PROBE_BYTES, now = () => Date.now(), state = {}}) {
  Object.assign(state, {http: false, bytes: 0, last: null, ended: false});
  return fetch(url, {cache: "no-store", signal}).then(async r => {
    state.http = true;
    if (r.status !== 200) return false;
    const reader = r.body.getReader();
    try {
      while (state.bytes < bytes) {
        const {done, value} = await reader.read();
        if (done) return false;
        state.bytes += value.length;
        state.last = now();
      }
      return true;
    } finally {
      reader.cancel().catch(() => {});
    }
  }).catch(() => false).then(ok => {
    state.ended = true;
    return ok;
  });
}

// The choice over what the probes have told so far; undefined while it still waits
// for one of them. probes: one per mirror with probe, in MIRRORS order: heads {name:
// HTTP status or "net"}, passed (true or false once ended), http, bytes, alive. The
// app's self-update (Updater.decide) ports this as it is: test/pick-vectors.json is
// run by both.
// A name passes on a mirror with HEAD 200 there and a passed probe.bin. The first
// passing one in order wins, checked: the versioned name on every mirror before any
// fixed one, since a mirror still deploying the new release serves the old APK under
// the fixed name; mirrors in MIRRORS order within each name.
// With no pass, the deadline sorts out probe.bin downloads still running: alive if a
// chunk came within the last aliveMs (a slow link), stalled otherwise (a throttled
// host goes silent after its first ~16 KB). The alive mirror with the most bytes is
// opened unchecked; a stalled one never is.
export function decide({mirrors = MIRRORS, versioned = null, probes}) {
  const names = versioned ? [versioned, FIXED] : [FIXED];
  const probed = mirrors.filter(m => m.probe);
  for (const n of names) {
    for (const [i, p] of probes.entries()) {
      if (p.heads[n] == null) return undefined;
      if (p.heads[n] !== 200) continue;
      if (p.passed == null) return undefined;
      if (p.passed) return {url: probed[i].base + n, checked: true};
    }
  }
  if (probes.some(p => p.passed == null)) return undefined;
  let target = null;
  probes.forEach((p, i) => {
    if (!p.passed && !p.alive) return;
    const name = names.find(n => p.heads[n] === 200) || (p.heads[FIXED] === "net" ? FIXED : null);
    if (name && (!target || p.bytes > target.bytes)) target = {url: probed[i].base + name, bytes: p.bytes};
  });
  if (target) return {url: target.url, checked: false};
  // Nothing answered at all: GitHub is likely unreachable, and opening Releases would
  // only swap this page for the browser's error page.
  const reachable = probes.some(p => p.http || names.some(n => p.heads[n] !== "net"));
  return {url: reachable ? lastResort(mirrors) : null, checked: false};
}

// All requests at once under one 4 s timeout: per mirror a HEAD on each APK name and
// one probe.bin. Opening the APK needs no user activation, but past ~5 s after the tap
// Chrome asks before a repeat download from the page. The deadline aborts whatever
// still runs, so decide() has every answer by then. Returns the URL to open and
// whether it was checked.
export async function pickUrl({mirrors = MIRRORS, versioned = null, fetch, streams,
    timeoutMs = PROBE_TIMEOUT_MS, bytes = PROBE_BYTES, aliveMs = ALIVE_MS, now = () => Date.now()}) {
  // Without body streams every probe would fail; throwing takes the click handler's
  // fallback to the plain link instead.
  if (!streams) throw new Error("no response streams");
  const abort = new AbortController();
  const {signal} = abort;
  const names = versioned ? [versioned, FIXED] : [FIXED];
  let settle;
  const decided = new Promise(r => { settle = r; });
  const check = () => {
    const d = decide({mirrors, versioned, probes: probed.map(p =>
      ({heads: p.heads, passed: p.passed, http: p.state.http, bytes: p.state.bytes, alive: p.alive}))});
    if (d) settle(d);
  };
  const probed = mirrors.filter(m => m.probe).map(m => {
    const p = {heads: {}, passed: undefined, state: {}, alive: false};
    for (const n of names) {
      fetch(m.base + n, {method: "HEAD", cache: "no-store", signal}).then(r => r.status, () => "net")
        .then(s => { p.heads[n] = s; check(); });
    }
    probe(m.base + PROBE_FILE, {fetch, signal, bytes, now, state: p.state}).then(ok => { p.passed = ok; check(); });
    return p;
  });
  const timer = setTimeout(() => {
    for (const p of probed) p.alive = !p.state.ended && p.state.last !== null && now() - p.state.last <= aliveMs;
    abort.abort();
  }, timeoutMs);
  // No mirror to probe: nothing would call it.
  check();
  try {
    return await decided;
  } finally {
    clearTimeout(timer);
    abort.abort();
  }
}

// The button's behavior. pickUrl() reads the current versioned name; the timer and
// event functions are called unbound, as the browser's globals allow.
export function attach({dl, fail, pickUrl, navigate,
    setTimeout, clearTimeout, addEventListener}) {
  const label = dl.textContent;
  let pending = null;

  function settle(showFail) {
    clearTimeout(pending);
    pending = null;
    fail.hidden = !showFail;
    dl.removeAttribute("aria-busy");
    dl.textContent = label;
  }

  dl.addEventListener("click", async e => {
    if (e.ctrlKey || e.metaKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    if (dl.hasAttribute("aria-busy")) return;
    dl.setAttribute("aria-busy", "true");
    dl.textContent = "Секунду…";
    fail.hidden = true;
    let url, checked;
    try {
      ({url, checked} = await pickUrl());
    } catch {
      // Engines without response streams (pickUrl throws): fall back to the plain link
      // the click was kept from opening.
      settle(false);
      navigate(dl.href);
      return;
    }
    if (!url) return settle(true);
    navigate(url);
    if (checked) return settle(false);
    // Releases is slow: Chrome shows the download prompt 4–15 s after the navigation, and a
    // started download leaves this page in place with no event to tell it from a failure.
    // So the button stays busy against a second tap, and the fallback links come only later.
    dl.textContent = "Скачиваем…";
    pending = setTimeout(settle, FAIL_DELAY_MS, true);
  });

  // Back from the browser's error page for Releases: the download did not start.
  addEventListener("pageshow", e => { if (e.persisted && pending) settle(true); });
}

function start() {
  // Set once the API tells the version: split-vpn.apk on Pages is cached for 10 minutes.
  let versioned = null;
  fetch("https://api.github.com/repos/bakuridzeaslan025-max/split-vpn/releases/latest")
    .then(r => r.ok ? r.json() : Promise.reject())
    .then(rel => {
      // Without the API (60 requests an hour per IP, and mobile NAT shares one) only the fixed name is tried.
      const info = releaseInfo(rel);
      versioned = info.versioned;
      if (info.meta) document.getElementById("meta").textContent = info.meta;
    })
    .catch(() => {});

  attach({
    dl: document.getElementById("dl"),
    fail: document.getElementById("fail"),
    pickUrl: () => pickUrl({versioned, fetch, streams: "body" in Response.prototype}),
    navigate: url => { location.href = url; },
    setTimeout, clearTimeout, addEventListener,
  });
}

if (typeof document === "object") start();
