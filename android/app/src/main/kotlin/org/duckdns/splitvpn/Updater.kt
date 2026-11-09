package org.duckdns.splitvpn

import android.app.ActivityManager
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInfo
import android.content.pm.PackageInstaller
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.provider.Settings
import android.util.JsonReader
import android.util.JsonToken
import androidx.core.content.IntentCompat
import java.io.File
import java.io.IOException
import java.io.StringReader
import java.net.HttpURLConnection
import java.net.SocketTimeoutException
import java.net.URL
import java.security.MessageDigest
import java.util.TreeMap
import java.util.concurrent.atomic.AtomicBoolean
import java.util.concurrent.TimeUnit
import java.util.concurrent.locks.ReentrantLock
import kotlin.concurrent.withLock
import kotlin.concurrent.thread
import org.json.JSONArray
import org.json.JSONException
import org.json.JSONObject

/**
 * Self-update: pick a mirror of Remote Config's `apk_urls` the way the
 * download page does (docs/download.js), download the APK, check it, hand it
 * to PackageInstaller. Without `apk_urls`, the one APK of `apk_url`. Runs in
 * the UI process: the confirmation dialog is an activity, and :vpn has no
 * window to start it from.
 */
object Updater {
    const val RELEASES = "https://github.com/bakuridzeaslan025-max/split-vpn/releases/"
    private const val MAX_BYTES = 64L shl 20
    private const val FILE = "update.apk"
    private const val CHANNEL_ID = "update"
    // 1 and 2 are TunnelVpnService's.
    private const val NOTIFICATION_ID = 3
    // Our committed session, in the UI process's prefs: the installer's
    // answer may come to a new process (from the notification, after the
    // install). Only its ABORTED is the user's cancel.
    internal const val KEY_SESSION = "update.session"
    // Hidden API: CONFIRM_INSTALL since Android 10, CONFIRM_PERMISSIONS before.
    private val confirmActions = setOf("android.content.pm.action.CONFIRM_INSTALL", "android.content.pm.action.CONFIRM_PERMISSIONS")

    enum class Result { STARTED, NEED_PERMISSION, UP_TO_DATE, DISABLED, BUSY, BAD_URL, FAILED }

    /** Grouped on the dashboard by kind; the details go into the message. */
    enum class Kind(val tag: String) { TIMEOUT("timeout"), HTTP("http"), BROKEN("broken"), CHECK("check"), INSTALL("install"), PROBE("probe") }

    internal class Failure(val kind: Kind, val detail: String, val bytes: Long = 0, val ms: Long = 0)

    internal class Pkg(val name: String, val versionCode: Long, val certs: Set<String>)

    internal val busy = AtomicBoolean()
    // The installer's dialog that came while the app was off screen: the
    // next onResume starts it, from the activity, where nothing blocks it.
    @Volatile
    private var pending: Pair<Int, Intent>? = null
    internal var visible: (Context) -> Boolean = {
        ActivityManager.RunningAppProcessInfo().also(ActivityManager::getMyMemoryState).importance ==
            ActivityManager.RunningAppProcessInfo.IMPORTANCE_FOREGROUND
    }

    fun canInstall(ctx: Context) = ctx.packageManager.canRequestPackageInstalls()

    /** For [Result.NEED_PERMISSION]: the system screen that grants it. */
    fun permissionIntent(ctx: Context) =
        Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES, Uri.parse("package:${ctx.packageName}"))

    /** «Обновить» downloads at all: else it opens the download page. `[]` in apk_urls turns it off. */
    fun enabled(v: Versions): Boolean {
        val m = v.apks.takeIf { it.isNotBlank() }?.let(::parseMirrors) ?: return v.apk.isNotEmpty()
        return m.list.isNotEmpty() || m.dropped.isNotEmpty()
    }

    /**
     * Blocks for the probes and the download: never on the main thread.
     * [Result.STARTED] means the system took the APK; the confirmation pops
     * up by itself, or waits for [takePending] and in a notification when the
     * app is no longer on screen. [Result.BUSY]: another call is still
     * downloading. Whatever else, the UI's way out is the download page,
     * never a mirror itself.
     */
    fun update(ctx: Context, v: Versions, progress: (Int) -> Unit = {}): Result {
        // A debug build shares the package and the key with release: updating it
        // would put release over debug without anyone asking.
        if (Versions.debug) return Result.DISABLED
        if (v.latest <= BuildConfig.VERSION_CODE) return Result.UP_TO_DATE
        // RemoteConfig.apply keeps apk_urls only when it is a list.
        val mirrors = v.apks.takeIf { it.isNotBlank() }?.let(::parseMirrors)
        if (mirrors == null && !urlOk(v.apk)) {
            report(Failure(Kind.CHECK, "url"))
            return Result.BAD_URL
        }
        if (mirrors != null) {
            if (mirrors.dropped.isNotEmpty()) report(Failure(Kind.CHECK, "url: apk_urls entries ${mirrors.dropped.joinToString(",")}"))
            if (mirrors.list.isEmpty()) return if (mirrors.dropped.isEmpty()) Result.DISABLED else Result.BAD_URL
        }
        if (!canInstall(ctx)) return Result.NEED_PERMISSION
        if (!busy.compareAndSet(false, true)) return Result.BUSY
        try {
            val source = if (mirrors == null) Source.Url(v.apk) else Source.Mirrored(mirrors, versioned(v.name))
            return run(ctx, v.latest, source, progress)
        } finally {
            busy.set(false)
        }
    }

    internal sealed class Source {
        /** apk_url: the one APK, as before apk_urls. */
        class Url(val url: String) : Source()
        class Mirrored(val mirrors: Mirrors, val versioned: String?) : Source()
    }

    /** The UI's [update] after its checks: probes, downloads, verifies, installs. */
    internal fun run(ctx: Context, latest: Long, source: Source, progress: (Int) -> Unit = {}, t: Timing = timing): Result {
        val apk = File(ctx.cacheDir, FILE)
        lastFailure = null
        lastKind = null
        lastMirror = null
        lastDetail = null
        // self_update's detail: why each mirror (its place in apk_urls) failed.
        val tags = TreeMap<Int, String>()
        val t0 = System.nanoTime()
        var probes: Probes? = null
        var current: Candidate? = null
        try {
            val queue = ArrayDeque<Candidate>()
            when (source) {
                is Source.Url -> queue += Candidate(source.url, 0)
                is Source.Mirrored -> {
                    lastMirror = 0
                    source.mirrors.dropped.forEach { tags[it] = "bad" }
                    probes = Probes(source.mirrors.list, source.versioned, t)
                    queue += probes.decision.candidates
                }
            }
            if (queue.isEmpty()) {
                report(Failure(Kind.PROBE, "no mirror", 0, (System.nanoTime() - t0) / 1_000_000))
                return Result.FAILED
            }
            val tried = HashSet<Int>()
            var failure: Failure? = null
            var backups = probes != null
            while (true) {
                if (queue.isEmpty() && backups) {
                    backups = false
                    queue += probes!!.backups().filter { it.mirror !in tried }
                }
                val c = queue.removeFirstOrNull() ?: break
                current = c
                if (failure != null) {
                    // Only the last one is a non-fatal: the others are in self_update's detail.
                    AppLog.i("update: ${failure(failure).message}, next mirror")
                    // A verify failure leaves the file behind.
                    apk.delete()
                    progress(-1)
                }
                tried += c.mirror
                val f = when {
                    source is Source.Url -> download(c.url, apk, progress = progress)
                    else -> download(c.url, apk, t.connectMs, if (c.checked) t.readMs else t.uncheckedReadMs, progress)
                } ?: checkApk(ctx, apk, latest)?.let { Failure(Kind.CHECK, it, apk.length(), (System.nanoTime() - t0) / 1_000_000) }
                if (f == null) {
                    if (source is Source.Mirrored) lastMirror = c.mirror
                    installer(ctx, apk)
                    return Result.STARTED
                }
                if (c.mirror > 0) tags[c.mirror] = tag(f)
                failure = f
            }
            report(failure!!)
            return Result.FAILED
        } catch (e: Exception) {
            current?.mirror?.takeIf { it > 0 }?.let { tags[it] = "install" }
            report(Failure(Kind.INSTALL, e.javaClass.simpleName, apk.length(), (System.nanoTime() - t0) / 1_000_000))
            return Result.FAILED
        } finally {
            // PackageInstaller has its own copy once the session is written.
            apk.delete()
            if (source is Source.Mirrored) lastDetail = detail(TreeMap(probes?.tags().orEmpty()).apply { putAll(tags) })
        }
    }

    // Swapped in unit tests: a test server serves no APK signed with our key.
    internal var checkApk: (Context, File, Long) -> String? = { ctx, apk, latest -> verify(self(ctx), archive(ctx, apk), latest) }
    internal var installer: (Context, File) -> Unit = { ctx, apk -> install(ctx, apk) }

    /** Download failures and [verify]'s reasons as self_update's detail spells them. */
    private fun tag(f: Failure) = when (f.kind) {
        Kind.CHECK -> when (f.detail) {
            "too big" -> "big"
            "not an apk" -> "notapk"
            "certificate" -> "cert"
            "not newer", "older than latest" -> "old"
            else -> f.detail
        }
        Kind.HTTP -> "get-http" + f.detail.removePrefix("HTTP ")
        Kind.TIMEOUT -> "get-" + f.detail
        else -> "get-" + f.kind.tag
    }

    /** "1:stall,2:http504": in mirror order, whole entries within Firebase's 100 characters. */
    internal fun detail(tags: Map<Int, String>): String? {
        val b = StringBuilder()
        for ((i, t) in tags) {
            val e = (if (b.isEmpty()) "" else ",") + "$i:$t"
            if (b.length + e.length > 100) break
            b.append(e)
        }
        return b.toString().ifEmpty { null }
    }

    /** Which mirror the last [update] took, its place in apk_urls from 1; 0 none, null without apk_urls. */
    @Volatile
    var lastMirror: Int? = null
        internal set

    /** Why the mirrors before it failed, or all of them, for self_update; null without apk_urls. */
    @Volatile
    var lastDetail: String? = null
        internal set

    // Mirrors (apk_urls), as docs/download.js. Each has FIXED and
    // split-vpn-<version>.apk, the Pages ones also probe.bin, 32 KB of random
    // bytes (android/Makefile, PAGES_FILES).
    internal const val FIXED = "split-vpn.apk"
    private const val PROBE_FILE = "probe.bin"
    /** A HEAD that got no HTTP answer: download.js's "net". */
    internal const val NET = -1

    private val pagesBases = setOf(
        "https://bakuridzeaslan025-max.github.io/split-vpn-dl/",
        "https://split-vpn-dl-547fcb.gitlab.io/",
    )
    private val releasesBase = Regex("""(?:download/[A-Za-z0-9_.+-]+|latest/download)/""")
    private val versionName = Regex("""[0-9]+\.[0-9]+\.[0-9]+""")

    /** An entry of apk_urls; [index] is its place there, from 1, as self_update counts mirrors. */
    internal data class Mirror(val base: String, val probe: Boolean, val index: Int)

    internal class Mirrors(val list: List<Mirror>, val dropped: List<Int>)

    /**
     * A second line behind the certificate check, against a swapped RC: our
     * Pages mirrors, Releases of our repo only without probe (it has no
     * probe.bin). The file names are ours, see [versioned].
     */
    internal fun mirrorOk(base: String, probe: Boolean) = ".." !in base &&
        (base in pagesBases || !probe && base.startsWith(RELEASES) && releasesBase.matches(base.removePrefix(RELEASES)))

    /**
     * apk_urls: the good entries in order and the places of the dropped ones;
     * null when it is no JSON array, or the good ones have no probed mirror. rc/tool/testdata/apk_urls.json is shared
     * with rc/tool's check, which refuses what this drops.
     */
    internal fun parseMirrors(raw: String): Mirrors? {
        if (!strictJson(raw)) return null
        val arr = try { JSONArray(raw.trim()) } catch (_: JSONException) { return null }
        val list = mutableListOf<Mirror>()
        val dropped = mutableListOf<Int>()
        for (i in 0 until arr.length()) {
            val o = arr.opt(i) as? JSONObject
            val base = o?.opt("base") as? String
            val probe = when (val p = o?.opt("probe")) {
                null, JSONObject.NULL -> false
                else -> p as? Boolean
            }
            if (base != null && probe != null && mirrorOk(base, probe)) list += Mirror(base, probe, i + 1) else dropped += i + 1
        }
        // Releases alone is only ever an unchecked last resort, and without a
        // probe nothing answers that would make it one: as no list at all.
        if (list.isNotEmpty() && list.none { it.probe }) return null
        return Mirrors(list, dropped)
    }

    // org.json takes single quotes, bare words, comments and anything after
    // the value; rc/tool's Go parser none of it.
    private fun strictJson(raw: String) = try {
        JsonReader(StringReader(raw)).use { r ->
            r.skipValue()
            r.peek() == JsonToken.END_DOCUMENT
        }
    } catch (_: Exception) {
        false
    }

    /** latest_name's file, null when RC has none: then only [FIXED] is asked for. */
    internal fun versioned(name: String) = name.takeIf { versionName.matches(it) }?.let { "split-vpn-$it.apk" }

    /** Shrunk in unit tests. probeMs is the one deadline of all probes, as download.js PROBE_TIMEOUT_MS. */
    internal class Timing(
        val probeMs: Long = 4_000,
        val aliveMs: Long = 1_500,
        val probeBytes: Int = 24_576,
        val connectMs: Int = 10_000,
        val readMs: Int = 5_000,
        // Releases redirects to its CDN, slow to the first byte.
        val uncheckedReadMs: Int = 15_000,
    )

    internal var timing = Timing()

    /** [checked]: it passed the probes; Releases as the last resort and an alive mirror did not. */
    internal data class Candidate(val url: String, val mirror: Int, val checked: Boolean = true)

    /**
     * What a probed mirror has told: [heads] by file name, an HTTP status or
     * [NET], absent while unanswered; [passed] null while probe.bin runs.
     */
    internal class Probed(val heads: Map<String, Int>, val passed: Boolean?, val http: Boolean, val bytes: Long, val alive: Boolean)

    /** [candidates]: the download's order, [url] first; past it only mirrors that passed by now, one name each. */
    internal class Decision(val candidates: List<Candidate>) {
        val url get() = candidates.firstOrNull()?.url
        val checked get() = candidates.firstOrNull()?.checked == true
    }

    /**
     * download.js decide(), line for line: null while it waits for an answer.
     * The vectors in docs/test/pick-vectors.json run against both.
     */
    internal fun decide(mirrors: List<Mirror>, versioned: String?, probes: List<Probed>): Decision? {
        val names = listOfNotNull(versioned, FIXED)
        val probed = mirrors.filter { it.probe }
        for (n in names) for ((i, p) in probes.withIndex()) {
            val h = p.heads[n] ?: return null
            if (h != 200) continue
            val passed = p.passed ?: return null
            if (passed) return Decision(passing(names, probed, probes))
        }
        if (probes.any { it.passed == null }) return null
        var target: Candidate? = null
        var most = 0L
        for ((i, p) in probes.withIndex()) {
            if (p.passed != true && !p.alive) continue
            val name = names.firstOrNull { p.heads[it] == 200 } ?: FIXED.takeIf { p.heads[FIXED] == NET } ?: continue
            if (target == null || p.bytes > most) {
                target = Candidate(probed[i].base + name, probed[i].index, checked = false)
                most = p.bytes
            }
        }
        if (target != null) return Decision(listOf(target))
        // Nothing answered at all: no network, Releases would fail as well.
        val reachable = probes.any { p -> p.http || names.any { p.heads[it] != NET } }
        val last = mirrors.firstOrNull { !it.probe }?.takeIf { reachable }
            ?: return Decision(emptyList())
        val c = Candidate(last.base + (versioned ?: FIXED), last.index, checked = false)
        return Decision(listOf(c))
    }

    // A mirror that failed the download fails it under its other name too.
    private fun passing(names: List<String>, probed: List<Mirror>, probes: List<Probed>): List<Candidate> {
        val seen = HashSet<Int>()
        return names.flatMap { n ->
            probes.indices.filter { probes[it].heads[n] == 200 && probes[it].passed == true && seen.add(it) }
                .map { Candidate(probed[it].base + n, probed[it].index) }
        }
    }

    // One probed mirror while its requests run; guarded by its Probes' lock.
    private class Live {
        val heads = HashMap<String, Int>()
        var passed: Boolean? = null
        var code = 0 // probe.bin's HTTP status
        var bytes = 0L
        var last = -1L
        var short = false
        var alive = false
        fun now() = Probed(HashMap(heads), passed, code != 0, bytes, alive)
        // As the page's abort: what still runs failed.
        fun aborted(names: List<String>) = Probed(names.associateWith { heads[it] ?: NET }, passed ?: false, code != 0, bytes, alive)
    }

    private fun millis() = System.nanoTime() / 1_000_000

    /**
     * The page's pickUrl: per probed mirror a HEAD on each name and one GET
     * of probe.bin, all at once under one deadline; [decide] after each
     * answer. Throttled mobile networks let ~16 KB of a host through and then
     * stall, so a HEAD alone proves nothing. [decision] is ready when the
     * constructor returns. Past a checked winner the other probes go on, so
     * that a download that fails has [backups]; at the deadline, or once all
     * have answered, every connection is dropped.
     */
    internal class Probes(private val mirrors: List<Mirror>, private val versioned: String?, private val t: Timing = timing) {
        private val names = listOfNotNull(versioned, FIXED)
        private val probed = mirrors.filter { it.probe }
        private val lock = ReentrantLock()
        private val changed = lock.newCondition()
        private val live = probed.map { Live() }
        private val open = mutableListOf<HttpURLConnection>()
        private var over = false
        private var atDeadline = false
        private val deadline = millis() + t.probeMs
        val decision: Decision

        init {
            for ((i, m) in probed.withIndex()) {
                for (n in names) thread(isDaemon = true, name = "Update probe") { head(m.base + n, live[i], n) }
                thread(isDaemon = true, name = "Update probe") { probe(m.base + PROBE_FILE, live[i]) }
            }
            decision = lock.withLock {
                var d = decide(mirrors, versioned, live.map { it.now() })
                while (d == null && !over) {
                    val left = deadline - millis()
                    if (left <= 0) finish(atDeadline = true) else changed.await(left, TimeUnit.MILLISECONDS)
                    d = decide(mirrors, versioned, live.map { if (over) it.aborted(names) else it.now() })
                }
                if (!over && settled()) finish(atDeadline = false)
                d!!
            }
            if (!over) thread(isDaemon = true, name = "Update probes") { lock.withLock { awaitEnd() } }
        }

        // Under the lock. Both the watcher and backups() end the probes, so
        // neither can be left waiting on the other.
        private fun awaitEnd() {
            while (!over) {
                val left = deadline - millis()
                when {
                    settled() -> finish(atDeadline = false)
                    left <= 0 -> finish(atDeadline = true)
                    else -> changed.await(left, TimeUnit.MILLISECONDS)
                }
            }
        }

        /**
         * Waits for the probes to end, the deadline at the latest: the
         * mirrors that passed, in the decision's order, one name each.
         */
        fun backups(): List<Candidate> = lock.withLock {
            awaitEnd()
            passing(names, probed, live.map { it.aborted(names) })
        }

        /** Why the probed mirrors failed, as far as known now; the chosen one aside. */
        fun tags(): Map<Int, String> = lock.withLock {
            val chosen = decision.candidates.firstOrNull()?.mirror
            probed.indices.filter { probed[it].index != chosen }
                .mapNotNull { i -> probeTag(live[i], names, atDeadline)?.let { probed[i].index to it } }.toMap()
        }

        private fun settled() = live.all { l -> l.passed != null && names.all { it in l.heads } }

        // Under the lock.
        private fun finish(atDeadline: Boolean) {
            if (atDeadline) {
                val at = millis()
                for (l in live) l.alive = l.passed == null && l.last >= 0 && at - l.last <= t.aliveMs
            }
            this.atDeadline = atDeadline
            over = true
            changed.signalAll()
            // Each on a thread of its own: a disconnect may wait for a connect
            // or, on the JVM, a read still running.
            for (c in open) thread(isDaemon = true, name = "Update probe") { runCatching { c.disconnect() } }
        }

        // Null once the probes are over: nothing more to ask.
        private fun connect(url: String): HttpURLConnection? = lock.withLock {
            if (over) return null
            (URL(url).openConnection() as HttpURLConnection).apply {
                // Past the deadline: only so that no thread outlives it by much.
                connectTimeout = t.probeMs.toInt() + 1_000
                readTimeout = t.probeMs.toInt() + 1_000
                useCaches = false
                // Else OkHttp pools it, and disconnect() leaves it open for minutes.
                setRequestProperty("Connection", "close")
                open += this
            }
        }

        private fun record(body: () -> Unit) = lock.withLock {
            if (!over) {
                body()
                changed.signalAll()
            }
        }

        private fun head(url: String, l: Live, name: String) {
            var c: HttpURLConnection? = null
            val code = try {
                c = connect(url) ?: return
                c.requestMethod = "HEAD"
                c.responseCode
            } catch (_: Exception) {
                NET
            } finally {
                c?.disconnect()
            }
            record { l.heads[name] = code }
        }

        private fun probe(url: String, l: Live) {
            var c: HttpURLConnection? = null
            val ok = try {
                c = connect(url) ?: return
                val code = c.responseCode
                record { l.code = code }
                code == 200 && c.inputStream.let { inp ->
                    val buf = ByteArray(8 shl 10)
                    var got = 0L
                    while (got < t.probeBytes) {
                        val n = inp.read(buf)
                        if (n < 0) {
                            // probe.bin is always 32 KB: a body ending short is a broken one.
                            record { l.short = true }
                            break
                        }
                        got += n
                        record {
                            l.bytes = got
                            l.last = millis()
                        }
                    }
                    got >= t.probeBytes
                }
            } catch (_: Exception) {
                false
            }
            record { l.passed = ok }
            c?.disconnect()
        }
    }

    // Null: passed, or still running, nothing known against it.
    private fun probeTag(l: Live, names: List<String>, atDeadline: Boolean): String? = when (l.passed) {
        null -> when {
            !atDeadline -> null
            l.alive -> "slow"
            l.code != 0 -> "stall"
            else -> "timeout"
        }
        false -> when {
            l.code != 0 && l.code != 200 -> "http${l.code}"
            l.short -> "short"
            // The body broke off: a reset mid-way, as DPI does, not a host out of reach.
            l.code == 200 -> "reset"
            else -> "net"
        }
        // probe.bin passed, the APK's HEAD did not.
        true -> names.map { l.heads[it] }.let { hs ->
            when {
                200 in hs -> null
                null !in hs -> "head" + hs.last().let { if (it == NET) "net" else "$it" }
                atDeadline -> "headtimeout"
                else -> null
            }
        }
    }

    // Charset excludes '/', '?', '#', '%', '@': exactly an asset of a tag, or
    // of the latest release, of our repo.
    private val asset = Regex("""(?:download/[A-Za-z0-9_.+-]+|latest/download)/[A-Za-z0-9_.+-]+\.apk""")

    internal fun urlOk(url: String) =
        url.startsWith(RELEASES) && asset.matches(url.removePrefix(RELEASES)) && ".." !in url

    /** Null when [apk] may replace [self]; otherwise which check failed. */
    internal fun verify(self: Pkg, apk: Pkg?, latestVersion: Long): String? = when {
        apk == null -> "not an apk"
        apk.name != self.name -> "package"
        // The archive may carry a rotation history (apksigner --lineage);
        // our current key has to be in it.
        self.certs.isEmpty() || !apk.certs.containsAll(self.certs) -> "certificate"
        apk.versionCode <= self.versionCode -> "not newer"
        apk.versionCode < latestVersion -> "older than latest"
        else -> null
    }

    /**
     * Null on success; on failure [dest] is gone. Follows GitHub's redirect to
     * its CDN. [progress]: percent, on each change, when the size is known.
     * [readMs] is the stall detector: no data in the body for that long is a
     * throttled host, TIMEOUT "stall". It bounds the wait for the answer as
     * well ("timeout"), so a 504 that comes late counts as none. The defaults
     * are apk_url's, as before apk_urls.
     */
    internal fun download(url: String, dest: File, connectMs: Int = 15_000, readMs: Int = 30_000, progress: (Int) -> Unit = {}): Failure? {
        val t0 = System.nanoTime()
        var bytes = 0L
        var body = false
        fun fail(kind: Kind, detail: String): Failure {
            dest.delete()
            return Failure(kind, detail, bytes, (System.nanoTime() - t0) / 1_000_000)
        }
        try {
            val c = URL(url).openConnection() as HttpURLConnection
            try {
                c.connectTimeout = connectMs
                // Set before the connection: on one already open it need not reach the stream.
                c.readTimeout = readMs
                c.instanceFollowRedirects = true
                // A body read to the end returns the socket to OkHttp's pool, open for minutes.
                c.setRequestProperty("Connection", "close")
                val code = c.responseCode
                if (code != HttpURLConnection.HTTP_OK) return fail(Kind.HTTP, "HTTP $code")
                val len = c.contentLengthLong
                if (len > MAX_BYTES) return fail(Kind.CHECK, "too big")
                body = true
                c.inputStream.use { inp ->
                    dest.outputStream().use { out ->
                        val buf = ByteArray(64 shl 10)
                        var shown = -1
                        while (true) {
                            val n = inp.read(buf)
                            if (n < 0) break
                            out.write(buf, 0, n)
                            bytes += n
                            if (bytes > MAX_BYTES) return fail(Kind.CHECK, "too big")
                            if (len > 0) {
                                val pct = (bytes * 100 / len).toInt().coerceAtMost(100)
                                if (pct != shown) progress(pct.also { shown = it })
                            }
                        }
                    }
                }
                if (len >= 0 && bytes != len) return fail(Kind.BROKEN, "short body")
            } finally {
                c.disconnect()
            }
        } catch (_: SocketTimeoutException) {
            return fail(Kind.TIMEOUT, if (body) "stall" else "timeout")
        } catch (e: IOException) {
            // The class only: messages of network errors name the host.
            return fail(Kind.BROKEN, e.javaClass.simpleName)
        }
        return null
    }

    /** Non-fatal with a frame of its own per kind, so each kind is its own issue. */
    internal fun failure(f: Failure) =
        RuntimeException("update ${f.kind.tag}: ${f.detail}, ${f.bytes} bytes in ${f.ms} ms").apply {
            stackTrace = arrayOf(StackTraceElement(Updater::class.java.name, f.kind.tag, "Updater.kt", 0))
        }

    /** The last [update]'s failure, for the user's log: the UI process has no file log of its own. */
    @Volatile
    var lastFailure: String? = null
        private set

    /** And its kind, for self_update's detail. */
    @Volatile
    var lastKind: Kind? = null
        private set

    private fun report(f: Failure) {
        val e = failure(f)
        lastFailure = e.message
        lastKind = f.kind
        AppLog.e("update failed", e)
    }

    /**
     * self_update: the funnel from the tap to the installed version. [mirror]:
     * the one taken, from 1 in apk_urls order, 0 none; [detail] after a
     * download, why the mirrors failed ("1:stall,3:get-http504").
     */
    internal fun event(ctx: Context, step: String, detail: String? = null, mirror: Int? = null) =
        Analytics.event(ctx, "self_update", buildMap {
            put("step", step)
            detail?.let { put("detail", it) }
            mirror?.let { put("mirror", it) }
        })

    private fun prefs(ctx: Context) = ctx.getSharedPreferences(Services.PREFS, Context.MODE_PRIVATE)

    @Suppress("DEPRECATION")
    private val signFlags =
        if (Build.VERSION.SDK_INT >= 28) PackageManager.GET_SIGNING_CERTIFICATES else PackageManager.GET_SIGNATURES

    private fun self(ctx: Context) =
        pkg(ctx.packageManager.getPackageInfo(ctx.packageName, signFlags), history = false)

    private fun archive(ctx: Context, apk: File) =
        ctx.packageManager.getPackageArchiveInfo(apk.path, signFlags)?.let { pkg(it, history = true) }

    @Suppress("DEPRECATION")
    private fun pkg(p: PackageInfo, history: Boolean): Pkg {
        val sigs = if (Build.VERSION.SDK_INT >= 28) {
            p.signingInfo?.let { s ->
                if (history && !s.hasMultipleSigners()) s.signingCertificateHistory else s.apkContentsSigners
            }
        } else {
            p.signatures
        }
        val sha = MessageDigest.getInstance("SHA-256")
        val certs = sigs.orEmpty().map { sig -> sha.digest(sig.toByteArray()).joinToString("") { "%02x".format(it) } }
        val code = if (Build.VERSION.SDK_INT >= 28) p.longVersionCode else p.versionCode.toLong()
        return Pkg(p.packageName, code, certs.toSet())
    }

    private fun install(ctx: Context, apk: File) {
        val installer = ctx.packageManager.packageInstaller
        val params = PackageInstaller.SessionParams(PackageInstaller.SessionParams.MODE_FULL_INSTALL).apply {
            setAppPackageName(ctx.packageName)
            setSize(apk.length())
            // Only once we are the installer of record: the first update still asks.
            if (Build.VERSION.SDK_INT >= 31) setRequireUserAction(PackageInstaller.SessionParams.USER_ACTION_NOT_REQUIRED)
        }
        // One left over from an unconfirmed attempt holds a copy of an older APK.
        // Their status comes back as STATUS_FAILURE_ABORTED, which is not reported:
        // first forget the one committed, or its ABORTED would be the user's cancel.
        prefs(ctx).edit().remove(KEY_SESSION).apply()
        for (old in installer.mySessions) runCatching { installer.abandonSession(old.sessionId) }
        pending = null
        ctx.getSystemService(NotificationManager::class.java).cancel(NOTIFICATION_ID)
        val id = installer.createSession(params)
        installer.openSession(id).use { s ->
            try {
                s.openWrite("base.apk", 0, apk.length()).use { out ->
                    apk.inputStream().use { it.copyTo(out) }
                    s.fsync(out)
                }
                // Mutable: the system puts the status and the confirmation intent in.
                val flags = PendingIntent.FLAG_UPDATE_CURRENT or (if (Build.VERSION.SDK_INT >= 31) PendingIntent.FLAG_MUTABLE else 0)
                val status = PendingIntent.getBroadcast(ctx, id, Intent(ctx, InstallReceiver::class.java), flags)
                s.commit(status.intentSender)
                // After the commit: a session abandoned below must not
                // come back as the user's cancel on top of the failure.
                prefs(ctx).edit().putInt(KEY_SESSION, id).apply()
            } catch (e: Exception) {
                s.abandon()
                throw e
            }
        }
        AppLog.i("update: session committed")
    }

    internal fun onStatus(ctx: Context, intent: Intent) {
        val st = intent.getIntExtra(PackageInstaller.EXTRA_STATUS, PackageInstaller.STATUS_FAILURE)
        val session = intent.getIntExtra(PackageInstaller.EXTRA_SESSION_ID, -1)
        // Answered from the notification: coming back to the app must not ask
        // again. Not by an abandoned session's ABORTED, though.
        if (st != PackageInstaller.STATUS_PENDING_USER_ACTION && pending?.first == session) pending = null
        val ours = session != -1 && prefs(ctx).getInt(KEY_SESSION, -1) == session
        // A final answer about ours: nothing more will come for it.
        if (ours && st != PackageInstaller.STATUS_PENDING_USER_ACTION) prefs(ctx).edit().remove(KEY_SESSION).apply()
        when (st) {
            PackageInstaller.STATUS_PENDING_USER_ACTION ->
                confirm(ctx, session, IntentCompat.getParcelableExtra(intent, Intent.EXTRA_INTENT, Intent::class.java))
            PackageInstaller.STATUS_SUCCESS -> {
                AppLog.i("update: installed")
                event(ctx, "installed")
                installed(ctx)
            }
            PackageInstaller.STATUS_FAILURE_ABORTED -> {
                AppLog.i("update: cancelled by the user")
                // Others are the sessions install() abandoned itself.
                if (ours) event(ctx, "cancelled")
            }
            else -> {
                report(Failure(Kind.INSTALL, "status $st"))
                event(ctx, "failed", Kind.INSTALL.tag)
            }
        }
    }

    private fun confirm(ctx: Context, session: Int, confirm: Intent?) {
        // It arrives through a mutable PendingIntent: start only the system's own dialog.
        if (confirm == null || confirm.action !in confirmActions) {
            report(Failure(Kind.INSTALL, "confirm action ${confirm?.action}"))
            event(ctx, "failed", "confirm")
            return
        }
        confirm.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        // Android 10+ drops activity starts from the background without a word.
        if (visible(ctx)) {
            ctx.startActivity(confirm)
            return
        }
        pending = session to confirm
        // Without POST_NOTIFICATIONS this shows nothing; coming back to the app still does.
        notify(ctx, "Обновление готово", "Нажмите, чтобы установить", confirm)
    }

    // The install killed the app, and Android starts no activity from the
    // background: this status comes to a fresh process, the tap reopens the app.
    private fun installed(ctx: Context) {
        if (visible(ctx)) return
        // As the launcher's: getLaunchIntentForPackage sets the package, and on
        // Android 8-10 a task started by it makes the icon stack a second activity.
        val open = Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER)
            .setClass(ctx, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        notify(ctx, "Split VPN обновлён до ${BuildConfig.VERSION_NAME}", "Нажмите, чтобы открыть", open)
    }

    private fun notify(ctx: Context, title: String, text: String, open: Intent) {
        val nm = ctx.getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(NotificationChannel(CHANNEL_ID, "Обновления", NotificationManager.IMPORTANCE_DEFAULT))
        val pi = PendingIntent.getActivity(ctx, 0, open, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        nm.notify(
            NOTIFICATION_ID,
            Notification.Builder(ctx, CHANNEL_ID)
                .setContentTitle(title)
                .setContentText(text)
                .setSmallIcon(android.R.drawable.stat_sys_download_done)
                .setContentIntent(pi)
                .setAutoCancel(true)
                .build(),
        )
    }

    /**
     * For the app on screen: the dialog [confirm] put off, once. The update
     * notification goes either way, its job is done.
     */
    fun takePending(ctx: Context): Intent? {
        ctx.getSystemService(NotificationManager::class.java).cancel(NOTIFICATION_ID)
        val (_, i) = pending ?: return null
        pending = null
        return i
    }
}

/** PackageInstaller's answer to a commit from [Updater]. */
class InstallReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) = Updater.onStatus(ctx, intent)
}
