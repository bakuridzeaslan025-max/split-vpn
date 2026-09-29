package org.duckdns.splitvpn

import com.google.firebase.FirebaseApp
import com.google.firebase.crashlytics.FirebaseCrashlytics
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment

/**
 * What [Crash.redact] lets through goes to Google. The VDS address, the cover
 * host and the relay path must not be in it.
 */
@RunWith(RobolectricTestRunner::class)
class CrashTest {

    @org.junit.Before
    fun setUp() = Endpoints.resetForTests()

    /**
     * Unit tests must never reach the production dashboard. They did once: a
     * NullPointerException from ShadowService showed up among the users'
     * crashes, because the real SDK picks up the real google-services.json.
     */
    @Test
    fun staysDisarmedUnderRobolectric() {
        val app = RuntimeEnvironment.getApplication()
        Crash.init(app, "vpn")
        Crash.record("boom", IllegalStateException("bad"))
        assertTrue(
            "Crashlytics armed in a unit test",
            FirebaseApp.getApps(app).isEmpty() ||
                !FirebaseCrashlytics.getInstance().isCrashlyticsCollectionEnabled,
        )
    }

    /**
     * The throwable's message becomes the report title. A Go error names the
     * VDS, so an unscrubbed one would put the server's address at the top of
     * the dashboard.
     */
    @Test
    fun scrubsTheAddressOutOfAThrowable() {
        val go = IllegalStateException("relay unreachable: dial tcp 203.0.113.10:443: refused")
        val scrubbed = Crash.scrub(RuntimeException("register failed", go))

        assertFalse("address leaked through the cause", scrubbed.toString().contains("203.0.113"))
        assertFalse("address leaked through the cause", scrubbed.cause!!.message!!.contains("203.0.113"))
        assertTrue("the original type is what makes a report readable", scrubbed.message!!.contains("RuntimeException"))
        assertTrue(scrubbed.cause!!.message!!.contains("IllegalStateException"))
        // Frames carry class and file names only, and the report is useless without them.
        assertEquals(go.stackTrace.size, scrubbed.cause!!.stackTrace.size)
    }

    /** A fatal crash never passes [Crash.record], so the handler is its only filter. */
    @Test
    fun scrubsFatalCrashesOnTheirWayToCrashlytics() {
        val before = Thread.getDefaultUncaughtExceptionHandler()
        try {
            var seen: Throwable? = null
            Thread.setDefaultUncaughtExceptionHandler { _, e -> seen = e } // stands in for Crashlytics
            Crash.scrubFatals()
            val crash = IllegalStateException("dial tcp 203.0.113.10:443: refused")
            Thread.getDefaultUncaughtExceptionHandler()!!.uncaughtException(Thread.currentThread(), crash)

            assertFalse("address reached the crash handler", seen.toString().contains("203.0.113"))
            assertTrue(seen!!.message!!.contains("IllegalStateException"))
            assertEquals(crash.stackTrace.size, seen!!.stackTrace.size)
        } finally {
            Thread.setDefaultUncaughtExceptionHandler(before)
        }
    }

    /**
     * Only what the UI already explains stays off the dashboard. A catch-all
     * here once hid every bug on the way up behind "no VPN permission".
     */
    @Test
    fun onlyEverydayFailuresAreExpected() {
        for (m in listOf(
            "relay unreachable: dial tcp 203.0.113.10:443: i/o timeout",
            "registration rejected",
            "credential rejected",
            "stopped",
            "VPN establish failed",
            TunnelVpnService.ERR_NEED_CODE,
        )) assertTrue(m, TunnelVpnService.expected(Exception(m)))

        for (e in listOf(
            NullPointerException(),
            UnsatisfiedLinkError("dlopen failed: libgojni.so"),
            Exception("fdbased: bad file descriptor"),
            Exception("register: http 502"),
            Exception("already running"),
        )) assertFalse(e.toString(), TunnelVpnService.expected(e))
    }

    @Test
    fun scrubStopsOnALoopedCauseChain() {
        val a = RuntimeException("a")
        val b = RuntimeException("b", a)
        a.initCause(b) // Throwable allows this; without a depth limit it would recurse forever
        Crash.scrub(a) // must return
    }

    @Test
    fun hidesTheVdsAddress() {
        val out = Crash.redact("dial 203.0.113.10:443 failed")
        assertFalse(out.contains("203.0.113"))
        assertEquals("dial <ip>:443 failed", out)
    }

    private val ours = listOf(
        TunnelVpnService.Endpoint("cover.example.org", "203.0.113.10", 443, "/app/s3cr3t"),
        TunnelVpnService.Endpoint("backup.example.net", "2001:db8::10", 8443, "/other"),
    )

    @Test
    fun hidesTheCoverHost() {
        assertEquals("TLS to <host> ok", Crash.redact("TLS to Cover.Example.org ok", ours))
        assertEquals("TLS to <host> ok", Crash.redact("TLS to backup.example.net ok", ours))
    }

    @Test
    fun hidesTheRelayPath() {
        assertEquals("POST <path>: http 404", Crash.redact("POST /app/s3cr3t: http 404", ours))
        assertEquals("GET <path>", Crash.redact("GET /other", ours))
    }

    /** Only our own hosts: a site name in an error is not worth a TLD list. */
    @Test
    fun keepsHostsThatAreNotOurs() {
        assertEquals("TLS to example.com ok", Crash.redact("TLS to example.com ok", ours))
    }

    @Test
    fun hidesCredentials() {
        val cred = "a".repeat(154)
        assertFalse(Crash.redact("Sec-WebSocket-Protocol: $cred").contains(cred))
    }

    @Test
    fun keepsAddressesInsideTheTun() {
        val line = "dns query via 10.255.0.2 from 192.168.1.5"
        assertEquals(line, Crash.redact(line))
    }

    @Test
    fun keepsDestinationsUnredactedOnlyWhenLocal() {
        // A public destination is stripped too: it is not worth the risk of
        // a rule that has to tell "our server" from "Telegram".
        assertEquals("route to <ip>", Crash.redact("route to 149.154.167.51"))
    }

    /**
     * Where the user went never reaches Crashlytics at all. Matching hostnames
     * in free text was tried and dropped: a TLD list missed .su, .info and
     * punycode, and matched WWW.YouTube.COM not at all.
     */
    @Test
    fun keepsTheBrowsingHistoryOffTheWire() {
        assertTrue(Crash.namesSites("dns rutracker.su./TypeA: 91 bytes in 43ms"))
        assertTrue(Crash.namesSites("dns WWW.YouTube.COM./TypeA: 91 bytes"))
        assertTrue(Crash.namesSites("sni forum.example.info → 1.2.3.4:443 direct"))
        assertTrue(Crash.namesSites("routes: pbs.twimg.com → 1.2.3.4 outside routes"))
    }

    @Test
    fun keepsGoDiagnosticsThatNameNoSite() {
        assertFalse(Crash.namesSites("tls: chrome hello to host ok in 124ms"))
        assertFalse(Crash.namesSites("go panic in handleTCP: boom"))
        assertFalse(Crash.namesSites("udp dns endpoint: closed"))
    }

    /**
     * The redaction used to eat these: a hostname pattern wide enough for the
     * real web also matches every second Java package.
     */
    @Test
    fun keepsPackageNamesIntact() {
        for (line in listOf(
            "onStartCommand action=android.net.VpnService",
            "java.io.IOException: broken pipe",
            "javax.net.ssl.SSLHandshakeException",
            "at org.duckdns.splitvpn.TunnelVpnService.onStartCommand",
        )) {
            assertEquals(line, Crash.redact(line))
        }
    }

    /**
     * A Go panic is the one report where every frame matters, and it is also
     * the line most likely to trip the redaction: Go package paths look a lot
     * like hostnames.
     */
    @Test
    fun keepsGoPanicFramesIntact() {
        val line = "go panic in handleTCP: assignment to entry in nil map" +
            " ← tunnel.handleTCP(tunnel.go:410)" +
            " ← tunnel.newStack.func1(tunnel.go:293)" +
            " ← tcp.(*Forwarder).HandlePacket(forwarder.go:143)" +
            " ← runtime.goexit(asm_amd64.s:1264)"
        assertEquals(line, Crash.redact(line))
    }

    @Test
    fun turnsGoFramesIntoAStack() {
        val frames = Crash.goFrames(
            "go panic in handleTCP: boom ← tunnel.handleTCP(tunnel.go:410)" +
                " ← tcp.(*Forwarder).HandlePacket(forwarder.go:143)",
        )!!
        assertEquals(2, frames.size)
        assertEquals("tunnel", frames[0].className)
        assertEquals("handleTCP", frames[0].methodName)
        assertEquals("tunnel.go", frames[0].fileName)
        assertEquals(410, frames[0].lineNumber)
        assertEquals("tcp.(*Forwarder)", frames[1].className)
        assertEquals("HandlePacket", frames[1].methodName)
        assertEquals(null, Crash.goFrames("register kind=1 failed"))
    }

    @Test
    fun hidesPublicV6ButKeepsLoopback() {
        assertEquals("bind <ip6> ok", Crash.redact("bind 2a00:1450:4001:80f::200e ok"))
        assertEquals("listen ::1 ok", Crash.redact("listen ::1 ok"))
        assertEquals("gw <ip6> up", Crash.redact("gw fe80::1 up"))
        assertEquals("dial [<ip6>]:443", Crash.redact("dial [2a00:1450:4001:80f::200e]:443"))
    }

    @Test
    fun keepsClocksAndMacs() {
        for (line in listOf("at 12:34:56.789 up", "bssid 02:00:00:ab:cd:ef", "tunnel.go:410:12")) {
            assertEquals(line, Crash.redact(line))
        }
    }
}
