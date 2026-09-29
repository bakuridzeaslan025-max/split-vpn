package org.duckdns.splitvpn

import android.content.Context
import android.os.Build
import com.google.firebase.FirebaseApp
import com.google.firebase.crashlytics.FirebaseCrashlytics

/**
 * Crashlytics wiring: the log lines that led to a crash, as breadcrumbs.
 *
 * Reports land on Google's servers, so everything passes [redact] first. The
 * log carries the VDS address, the cover host and the secret relay path —
 * leaking those would burn the server, not just the user's privacy. When in
 * doubt the rule is to drop, not to keep: a stack trace without an IP still
 * tells us where it crashed.
 *
 * In the main process Crashlytics installs its handler from its own
 * ContentProvider, before any of our code runs. In `:vpn` nothing is caught
 * until [init].
 */
object Crash {
    private val hex = Regex("""\b[0-9a-fA-F]{32,}\b""")
    private val ipv4 = Regex("""\b\d{1,3}(?:\.\d{1,3}){3}\b""")
    private val ipv6 = Regex("""(?<![\w:])(?:[0-9a-fA-F]{0,4}:){2,7}[0-9a-fA-F]{0,4}(?![\w:])""")
    // Go log lines that name the sites the user opened. They never leave the
    // device. Cutting them at the source beats matching hostnames in free
    // text: a TLD list is never complete (.su, .xyz, punycode .рф), and one
    // wide enough to catch them also eats java.io.* and android.net.* out of
    // every stack trace.
    private val siteLines = listOf("dns ", "sni ", "routes: ")
    // Addresses inside the TUN and on the user's own network say nothing
    // about our server and are half the value of a network-switch report.
    private val goFrame = Regex("""← (\S+)\.([^.(\s]+)\(([^:()\s]+):(\d+)\)""")
    private var scrubbing = false
    private val local = Regex("""^(?:10\.|127\.|192\.168\.|169\.254\.|172\.(?:1[6-9]|2\d|3[01])\.|0\.0\.0\.0|255\.)""")

    fun init(ctx: Context, process: String) = guard {
        Endpoints.attach(ctx)
        // Robolectric runs the real SDK on a developer machine, so a failing
        // unit test files a crash against the production app and pollutes the
        // dashboard. Seen for real: a NullPointerException out of
        // ShadowService landed among the users' crashes.
        if (Build.FINGERPRINT == "robolectric") {
            FirebaseCrashlytics.getInstance().isCrashlyticsCollectionEnabled = false
            return@guard
        }
        // FirebaseInitProvider only runs in the main process — a ContentProvider
        // lives in one process, not in every one. Without this the :vpn process,
        // where the tunnel actually crashes, reports nothing.
        FirebaseApp.initializeApp(ctx)
        FirebaseCrashlytics.getInstance().apply {
            setCustomKey("process", process)
            // Decrypting loads Go, and the UI process has no use for it.
            if (process == "vpn") setCustomKey("endpoints", Endpoints.defaults.size)
        }
        // Fatal crashes bypass [record], and their messages name the VDS just
        // as well. Wraps the Crashlytics handler, so it has to come after it.
        if (!scrubbing) {
            scrubbing = true
            scrubFatals()
        }
    }

    internal fun scrubFatals() {
        val next = Thread.getDefaultUncaughtExceptionHandler()
        Thread.setDefaultUncaughtExceptionHandler { thread, e ->
            next?.uncaughtException(thread, try { scrub(e) } catch (_: Throwable) { e })
        }
    }

    fun state(state: VpnState) = guard {
        FirebaseCrashlytics.getInstance().setCustomKey("vpn_state", state.name)
    }

    fun log(line: String) = guard {
        FirebaseCrashlytics.getInstance().log(redact(line))
    }

    /** True for the Go lines that name where the user went — kept off the wire. */
    fun namesSites(line: String) = siteLines.any(line::startsWith)

    /** A failure we handled: reported as non-fatal, so it shows up without a crash. */
    fun record(msg: String, t: Throwable?) = guard {
        val c = FirebaseCrashlytics.getInstance()
        c.log(redact(msg))
        c.recordException(if (t == null) RuntimeException(redact(msg)).apply { goFrames(msg)?.let { stackTrace = it } } else scrub(t))
    }

    /**
     * The Go frames of a recovered panic, as a stack Crashlytics can group by.
     * Without them every panic carries the same Kotlin stack of the logger
     * and they all pile into a single issue.
     */
    internal fun goFrames(line: String): Array<StackTraceElement>? =
        goFrame.findAll(line).map { m ->
            val (pkg, func, file, no) = m.destructured
            StackTraceElement(pkg, func, file, no.toInt())
        }.toList().takeIf { it.isNotEmpty() }?.toTypedArray()

    /**
     * Crashlytics puts the throwable's own message in the report title, so it
     * needs redacting as much as any log line — and it is the worse offender:
     * an error out of Go reads `relay unreachable: dial tcp 203.0.113.10:443`
     * and hands Google the server's address. Keeps the stack (class and file
     * names are not secret) and the type, which is what the report is for.
     */
    internal fun scrub(t: Throwable, depth: Int = 0): Throwable =
        RuntimeException(
            "${t.javaClass.name}: ${redact(t.message.orEmpty())}",
            t.cause?.takeIf { depth < 4 }?.let { scrub(it, depth + 1) },
        ).apply { stackTrace = t.stackTrace }

    fun redact(s: String, known: List<TunnelVpnService.Endpoint> = Endpoints.known): String {
        var out = mask(s, known)
        out = ipv6.replace(out) {
            val v = it.value
            // A full address has 7 colons, a shortened one has "::". Anything
            // else this shape is a clock (12:34:56) or a MAC.
            if (v == "::1" || ("::" !in v && v.count { c -> c == ':' } != 7)) v else "<ip6>"
        }
        return ipv4.replace(out) { if (local.containsMatchIn(it.value)) it.value else "<ip>" }
    }

    /**
     * Our servers and the credential only: what the local log file must not
     * hold either, since "share the log" sends it into some chat. The rest of
     * [redact] stays off the file: public addresses are what a log is read for.
     */
    fun mask(s: String, known: List<TunnelVpnService.Endpoint> = Endpoints.known): String {
        var out = s
        for (ep in known) {
            out = out.replace(ep.path, "<path>").replace(ep.host, "<host>", ignoreCase = true).replace(ep.ip, "<ip>")
        }
        return hex.replace(out, "<hex>")
    }

    /**
     * Debug-only check that reports arrive and that [redact] holds:
     *   adb shell am start -S -n org.newvpn/org.duckdns.splitvpn.MainActivity --es crash ui|vpn
     * The line below carries exactly what must never reach Google, so the
     * report on the dashboard is the proof. Crashing on a delay because
     * Crashlytics writes its log asynchronously — a crash 7 ms after the
     * call arrives with an empty log and proves nothing.
     */
    fun smokeTest(process: String) {
        val ep = Endpoints.known.firstOrNull() ?: TunnelVpnService.Endpoint("cover.example.org", "203.0.113.10", 443, "/path")
        AppLog.i("smoke test ($process): ${ep.addr} ${ep.host} ${ep.path} 10.255.0.2")
        AppLog.go("dns www.youtube.com./TypeA: 91 bytes in 43ms") // must not reach the dashboard
        if (process == "go") {
            // Recovered in Go, arrives as a non-fatal through AppLog.go.
            tunnel.Tunnel.smokeTestPanic(AppLog.goLogger)
            return
        }
        android.os.Handler(android.os.Looper.getMainLooper()).postDelayed({
            throw RuntimeException("Crashlytics smoke test ($process)")
        }, 2000)
    }

    /** Crashlytics must never be the thing that takes the app down. */
    private inline fun guard(body: () -> Unit) {
        try {
            body()
        } catch (_: Throwable) {
        }
    }
}
