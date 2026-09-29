package org.duckdns.splitvpn

import android.content.Context
import android.content.Intent
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.Shadows.shadowOf
import org.robolectric.android.controller.ServiceController
import org.robolectric.shadows.ShadowLooper
import org.robolectric.shadows.ShadowNetwork
import org.robolectric.shadows.ShadowSystemClock
import java.io.File

/** Remote Config in :vpn: when it is fetched, what is kept, what gets masked. */
@RunWith(RobolectricTestRunner::class)
class RemoteConfigTest {

    private lateinit var controller: ServiceController<TunnelVpnService>
    private val app get() = RuntimeEnvironment.getApplication()
    private val svc get() = controller.get()
    private val be = FakeBackend()

    private val baked = TunnelVpnService.Endpoint("cover.example.org", "203.0.113.10", 443, "/app/baked1")
    private val fromRc = TunnelVpnService.Endpoint("fresh.example.net", "198.51.100.7", 8443, "/app/fresh22")
    private fun blob(vararg eps: TunnelVpnService.Endpoint) = "blob:" + eps.joinToString(",", "[", "]") {
        """{"host":"${it.host}","ip":"${it.ip}","port":${it.port},"path":"${it.path}"}"""
    }

    @Before
    fun setUp() {
        TunnelState.set(VpnState.DISCONNECTED)
        TunnelState.clearLog()
        ShadowLooper.idleMainLooper()
        AppLog.dir(app).deleteRecursively()
        Endpoints.resetForTests()
        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
        svc.backend = be.apply { endpoints = listOf(baked) }
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400))
        ShadowLooper.idleMainLooper()
    }

    @After
    fun tearDown() {
        be.release()
        controller.destroy()
        ShadowLooper.idleMainLooper()
    }

    private fun start() {
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START).putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram")), 0, 1)
    }

    private fun awaitState(want: VpnState) {
        val deadline = System.currentTimeMillis() + 10_000
        while (TunnelState.state != want && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, java.util.concurrent.TimeUnit.MILLISECONDS)
        }
        assertEquals(TunnelState.lastError ?: "", want, TunnelState.state)
    }

    // The fetch thread clears its flag right after the fetch returns.
    private fun awaitFetches(n: Int) {
        val deadline = System.currentTimeMillis() + 5_000
        while (be.fetches.get() < n && System.currentTimeMillis() < deadline) Thread.sleep(10)
        assertEquals(n, be.fetches.get())
        Thread.sleep(200)
    }

    private fun logLines() = AppLog.files(app).reversed().flatMap { it.readLines() }

    @Test
    fun startDoesNotWaitForTheFetch() {
        be.blockConfig = true
        start()
        awaitState(VpnState.CONNECTED)
        assertEquals(1, be.fetches.get())
    }

    @Test
    fun cacheBeatsTheBakedInList() {
        Endpoints.cache(app, listOf(fromRc))
        start()
        awaitState(VpnState.CONNECTED)
        assertEquals(fromRc.host, be.startedSni)
    }

    @Test
    fun fetchedListBecomesTheCache() {
        be.config = RcValues(blob(fromRc), 110, 120, "https://example.org/app.apk")
        start()
        awaitFetches(1)
        assertEquals(listOf(fromRc), Endpoints.cached(app))
        val prefs = app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)
        assertEquals(110L, prefs.getLong(RemoteConfig.KEY_MIN_VERSION, 0))
        assertEquals(120L, prefs.getLong(RemoteConfig.KEY_LATEST_VERSION, 0))
        assertEquals("https://example.org/app.apk", prefs.getString(RemoteConfig.KEY_UPDATE_URL, null))
        assertTrue(fromRc in Endpoints.known)
    }

    @Test
    fun brokenBlobLeavesTheCache() {
        Endpoints.cache(app, listOf(fromRc))
        for (bad in listOf("garbage", "blob:[]", "blob:{}")) {
            RemoteConfig.apply(app, RcValues(bad, 0, 0, ""), be::decryptEndpoints)
            assertEquals(bad, listOf(fromRc), Endpoints.cached(app))
        }
        assertEquals(3, logLines().count { " E remote config: endpoints dropped" in it })
    }

    @Test
    fun noEndpointsKeyLeavesTheCacheQuietly() {
        Endpoints.cache(app, listOf(fromRc))
        RemoteConfig.apply(app, RcValues("", 0, 0, ""), be::decryptEndpoints)
        assertEquals(listOf(fromRc), Endpoints.cached(app))
        assertFalse(logLines().any { "remote config" in it })
    }

    // "Server unavailable" fetches, a bare RelayDown (no network) does not.
    @Test
    fun fetchesOnNoServerOnly() {
        start()
        awaitState(VpnState.CONNECTED)
        awaitFetches(1)
        val cm = app.getSystemService(android.net.ConnectivityManager::class.java)
        val cbs = shadowOf(cm).networkCallbacks.toList()
        @Suppress("DEPRECATION")
        cm.allNetworks.forEach { n -> cbs.forEach { it.onLost(n) } }
        be.startedHost!!.relayDown(true)
        ShadowLooper.idleMainLooper()
        advance(5_000)
        assertEquals(Waiting.NO_NETWORK, TunnelState.waiting)
        assertEquals(1, be.fetches.get())

        cbs.forEach { it.onAvailable(ShadowNetwork.newInstance(7)) }
        ShadowLooper.idleMainLooper()
        advance(10_000)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        awaitFetches(2)
    }

    // "Server unavailable" may last: a new list can be published meanwhile.
    @Test
    fun retriesWhileNoServerOnly() {
        be.relayDownAtStart = true
        start()
        awaitState(VpnState.CONNECTED)
        awaitFetches(1)
        netCallbacks().forEach { it.onAvailable(ShadowNetwork.newInstance(7)) }
        ShadowLooper.idleMainLooper()
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        awaitFetches(2)
        idle(TunnelVpnService.CONFIG_RETRY_MS)
        awaitFetches(3)

        be.startedHost!!.relayDown(false)
        ShadowLooper.idleMainLooper()
        assertEquals(null, TunnelState.waiting)
        idle(TunnelVpnService.CONFIG_RETRY_MS * 3)
        Thread.sleep(200)
        assertEquals(3, be.fetches.get())
    }

    @Test
    fun periodicFetchEndsWithTheTunnel() {
        start()
        awaitState(VpnState.CONNECTED)
        awaitFetches(1)
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_STOP), 0, 2)
        idle(TunnelVpnService.CONFIG_PERIOD_MS * 2)
        Thread.sleep(200)
        assertEquals(1, be.fetches.get())
    }

    @Test
    fun instrumentedTestRelayNeverFetches() {
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400), test = true)
        svc.onStartCommand(
            Intent(TunnelVpnService.ACTION_START)
                .putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram"))
                .putExtra(TunnelVpnService.EXTRA_ENDPOINT, "test.local|10.0.2.2|8443|/app/test"),
            0, 1,
        )
        awaitState(VpnState.CONNECTED)
        idle(TunnelVpnService.CONFIG_PERIOD_MS)
        Thread.sleep(200)
        assertEquals(0, be.fetches.get())
    }

    // The file only grows: one that does not parse is not replaced by the new list alone.
    @Test
    fun unreadableFileIsNotTruncated() {
        val file = File(app.filesDir, Endpoints.KNOWN_FILE)
        Endpoints.remember(listOf(baked))
        file.writeText("[{\"host\":")
        Endpoints.remember(listOf(fromRc))
        assertEquals(setOf(baked, fromRc), Endpoints.parse(file.readText()).toSet())
    }

    private fun netCallbacks() = shadowOf(app.getSystemService(android.net.ConnectivityManager::class.java)).networkCallbacks.toList()

    private fun idle(ms: Long) = ShadowLooper.idleMainLooper(ms, java.util.concurrent.TimeUnit.MILLISECONDS)

    private fun advance(ms: Long) {
        ShadowSystemClock.advanceBy(java.time.Duration.ofMillis(ms))
        ShadowLooper.idleMainLooper()
    }

    @Test
    fun fetchesAgainAfterSixHours() {
        start()
        awaitState(VpnState.CONNECTED)
        awaitFetches(1)
        ShadowLooper.idleMainLooper(TunnelVpnService.CONFIG_PERIOD_MS, java.util.concurrent.TimeUnit.MILLISECONDS)
        awaitFetches(2)
    }

    /** The UI process never fetches: it learns what to mask from the file :vpn writes. */
    @Test
    fun otherProcessMasksByTheFile() {
        val file = File(app.filesDir, Endpoints.KNOWN_FILE)
        val stranger = TunnelVpnService.Endpoint("late.example.com", "192.0.2.44", 443, "/app/late333")
        // The UI's side: a process that never remembers, only reads.
        Endpoints.resetForTests()
        Endpoints.attach(app)
        // As :vpn in another process would, behind this one's back.
        file.writeText("""[{"host":"${stranger.host}","ip":"${stranger.ip}","port":443,"path":"${stranger.path}"}]""")
        assertEquals("TLS to <host> <path>", Crash.redact("TLS to late.example.com /app/late333"))
        assertTrue(stranger in Endpoints.known)

        RemoteConfig.apply(app, RcValues(blob(fromRc), 0, 0, ""), be::decryptEndpoints)
        assertTrue("an RC list lands in the file", fromRc in Endpoints.parse(file.readText()))
        assertTrue("and the old entries stay", stranger in Endpoints.parse(file.readText()))
    }

    @Test
    fun fileLogIsMasked() {
        Endpoints.remember(listOf(fromRc))
        AppLog.go("tls: chrome hello to ${fromRc.host} ok in 120ms")
        AppLog.i("dial ${fromRc.ip}:443 ${fromRc.path} from 8.8.8.8")
        val text = logLines().joinToString("\n")
        assertFalse(text, fromRc.host in text || fromRc.ip in text || fromRc.path in text)
        assertTrue("public addresses stay: they are what the log is read for", "8.8.8.8" in text)
        assertTrue(text, "chrome hello to <host>" in text)
    }

    @Test
    fun brokenBlobIsReportedOncePerBlob() {
        assertTrue(RemoteConfig.firstDrop(app, "garbage"))
        assertFalse("the same blob on the next fetch: a breadcrumb", RemoteConfig.firstDrop(app, "garbage"))
        assertTrue("another broken one is news", RemoteConfig.firstDrop(app, "garbage2"))
    }

    // Server unavailable for hours: the fetch retry backs off like the rounds do.
    @Test
    fun noServerRetriesBackOff() {
        be.relayDownAtStart = true
        start()
        awaitState(VpnState.CONNECTED)
        awaitFetches(1)
        netCallbacks().forEach { it.onAvailable(ShadowNetwork.newInstance(7)) }
        ShadowLooper.idleMainLooper()
        awaitFetches(2)
        val base = TunnelVpnService.CONFIG_RETRY_MS
        var n = 2
        for (k in listOf(1, 2, 4, 8, 12, 12)) {
            idle(base * k - 1_000)
            Thread.sleep(100)
            assertEquals("not before ${k}x", n, be.fetches.get())
            idle(1_000)
            awaitFetches(++n)
        }
    }
}
