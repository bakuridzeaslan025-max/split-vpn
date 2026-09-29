package org.duckdns.splitvpn

import android.content.Intent
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
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
import java.util.concurrent.TimeUnit

/** The running tunnel moves between endpoints on "server unavailable". */
@RunWith(RobolectricTestRunner::class)
class EndpointSwitchTest {

    private lateinit var controller: ServiceController<TunnelVpnService>
    private val app get() = RuntimeEnvironment.getApplication()
    private val svc get() = controller.get()
    private val be = FakeBackend()

    private val a = TunnelVpnService.Endpoint("a.example.org", "203.0.113.10", 443, "/app/aaaa1")
    private val b = TunnelVpnService.Endpoint("b.example.net", "198.51.100.7", 8443, "/app/bbbb2")
    private val c = TunnelVpnService.Endpoint("c.example.com", "192.0.2.44", 443, "/app/cccc3")
    private val d = TunnelVpnService.Endpoint("d.example.org", "203.0.113.99", 443, "/app/dddd4")
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
        svc.backend = be.apply { endpoints = listOf(a, b, c) }
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400))
        ShadowLooper.idleMainLooper()
    }

    @After
    fun tearDown() {
        be.release()
        controller.destroy()
        ShadowLooper.idleMainLooper()
    }

    private fun start(intent: Intent = Intent(TunnelVpnService.ACTION_START)) {
        svc.onStartCommand(intent.putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram")), 0, 1)
        awaitConnected()
    }

    private fun awaitConnected() {
        val deadline = System.currentTimeMillis() + 10_000
        // CONNECTED comes a moment before the network callback is registered.
        while ((TunnelState.state != VpnState.CONNECTED || netCallbacks().isEmpty()) && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, TimeUnit.MILLISECONDS)
        }
        assertEquals(TunnelState.lastError ?: "", VpnState.CONNECTED, TunnelState.state)
        ShadowLooper.idleMainLooper()
    }

    private fun stop() {
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_STOP), 0, 2)
        val deadline = System.currentTimeMillis() + 5_000
        while (TunnelState.state != VpnState.DISCONNECTED && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, TimeUnit.MILLISECONDS)
        }
    }

    private fun netCallbacks() = shadowOf(app.getSystemService(android.net.ConnectivityManager::class.java)).networkCallbacks.toList()

    private fun online() {
        netCallbacks().forEach { it.onAvailable(ShadowNetwork.newInstance(7)) }
        ShadowLooper.idleMainLooper()
    }

    private fun offline() {
        val cm = app.getSystemService(android.net.ConnectivityManager::class.java)
        val cbs = netCallbacks()
        @Suppress("DEPRECATION")
        (cm.allNetworks.toList() + ShadowNetwork.newInstance(7)).forEach { n -> cbs.forEach { it.onLost(n) } }
        ShadowLooper.idleMainLooper()
    }

    /** Go's verdict on the endpoint it now talks to. */
    private fun verdict(down: Boolean) {
        be.startedHost!!.relayDown(down)
        ShadowLooper.idleMainLooper()
    }

    private fun advance(ms: Long) {
        ShadowSystemClock.advanceBy(java.time.Duration.ofMillis(ms))
        ShadowLooper.idleMainLooper()
    }

    private fun idle(ms: Long) = ShadowLooper.idleMainLooper(ms, TimeUnit.MILLISECONDS)

    // Fetches run on their own thread and post the list's verdict back.
    private fun awaitFetchAfter(n: Int) {
        val deadline = System.currentTimeMillis() + 5_000
        while (be.fetches.get() <= n && System.currentTimeMillis() < deadline) Thread.sleep(10)
        Thread.sleep(200)
        ShadowLooper.idleMainLooper()
    }

    @Test
    fun noServerMovesOnAndAFullRoundPauses() {
        start()
        assertEquals(a.host, be.startedSni)
        online()
        verdict(true)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        assertEquals(listOf(b), be.endpointsSet)
        assertTrue(TunnelState.logLines().any { it.endsWith("Сервер недоступен, переключаюсь на резервный (2 из 3)") })
        verdict(true)
        assertEquals(listOf(b, c), be.endpointsSet)
        verdict(true)
        assertEquals("all three down: stay", listOf(b, c), be.endpointsSet)
        idle(TunnelVpnService.ROUND_PAUSE_MS - 1_000)
        assertEquals(listOf(b, c), be.endpointsSet)
        idle(1_000)
        assertEquals(listOf(b, c, a), be.endpointsSet)
        assertTrue(TunnelState.logLines().last().endsWith("Сервер недоступен, возвращаюсь на основной"))
    }

    @Test
    fun noNetworkIsNotAReason() {
        start()
        offline()
        verdict(true)
        advance(5_000)
        assertEquals(Waiting.NO_NETWORK, TunnelState.waiting)
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), be.endpointsSet)
    }

    // The old verdict still reads "down" until Go judges the new endpoint.
    @Test
    fun networkBackBeforeTheVerdictDoesNotSkip() {
        start()
        online()
        verdict(true)
        assertEquals(listOf(b), be.endpointsSet)
        offline()
        advance(5_000)
        assertEquals(Waiting.NO_NETWORK, TunnelState.waiting)
        online()
        advance(10_000)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        assertEquals(listOf(b), be.endpointsSet)
    }

    @Test
    fun workingOneIsRememberedAndTriedFirst() {
        start()
        online()
        verdict(true)
        verdict(false)
        assertEquals(b, Endpoints.working(app))
        stop()
        start()
        assertEquals(b.host, be.startedSni)

        // Registration too.
        stop()
        Credentials.clear(app, false)
        be.token = "tok".toByteArray()
        val asked = mutableListOf<String>()
        svc.backend = object : Backend by be {
            override fun register(addr: String, sni: String, path: String, kind: Int, proof: ByteArray): ByteArray {
                asked += sni
                return fakeCred(System.currentTimeMillis() / 1000 + 7 * 86400)
            }
        }
        start()
        assertEquals(listOf(b.host), asked)
    }

    @Test
    fun sameListFromRcStays() {
        Endpoints.cache(app, listOf(a, b, c))
        be.config = RcValues(blob(a, b, c), 0, 0, "")
        start()
        online()
        verdict(true)
        assertEquals(listOf(b), be.endpointsSet)
        val n = be.fetches.get()
        idle(TunnelVpnService.CONFIG_RETRY_MS)
        awaitFetchAfter(n)
        assertEquals(listOf(b), be.endpointsSet)
    }

    @Test
    fun changedListGoesToItsFirst() {
        Endpoints.cache(app, listOf(a, b, c))
        start()
        online()
        verdict(true)
        verdict(false)
        assertEquals(b, Endpoints.working(app))
        be.config = RcValues(blob(d, b), 0, 0, "")
        verdict(true)
        val n = be.fetches.get()
        idle(TunnelVpnService.CONFIG_RETRY_MS)
        awaitFetchAfter(n)
        assertEquals(d, be.endpointsSet.last())
        assertTrue(TunnelState.logLines().last().endsWith("Получен новый список серверов"))
        assertNull("a new list starts from its first", Endpoints.working(app))
    }

    @Test
    fun currentGoneFromTheListGoesToTheFirst() {
        Endpoints.cache(app, listOf(a, b))
        start()
        online()
        verdict(true)
        assertEquals(listOf(b), be.endpointsSet)
        // As if another fetch had already cached it.
        Endpoints.cache(app, listOf(c, a))
        be.config = RcValues(blob(c, a), 0, 0, "")
        val n = be.fetches.get()
        idle(TunnelVpnService.CONFIG_RETRY_MS)
        awaitFetchAfter(n)
        assertEquals(listOf(b, c), be.endpointsSet)
    }

    @Test
    fun instrumentedTestRelayNeverSwitches() {
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400), test = true)
        start(Intent(TunnelVpnService.ACTION_START).putExtra(TunnelVpnService.EXTRA_ENDPOINT, "test.local|10.0.2.2|8443|/app/test"))
        online()
        verdict(true)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        idle(TunnelVpnService.ROUND_PAUSE_MS)
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), be.endpointsSet)
    }

    // The instrumented tests' way in (EXTRA_RC_ENDPOINTS): switches like a fetched list, the real one stays.
    @Test
    fun testListFromRcSwitchesApartFromTheRealOne() {
        Endpoints.cache(app, listOf(a, b, c))
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400), test = true)
        fun rc(vararg eps: TunnelVpnService.Endpoint) = Intent(TunnelVpnService.ACTION_START)
            .putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram"))
            .putExtra(TunnelVpnService.EXTRA_RC_ENDPOINTS, blob(*eps).removePrefix("blob:"))
        start(rc(c, d))
        assertEquals(c.host, be.startedSni)
        online()
        verdict(true)
        assertEquals(listOf(d), be.endpointsSet)
        verdict(false)
        assertEquals(d, Endpoints.working(app, test = true))
        svc.onStartCommand(rc(d, a), 0, 3)
        ShadowLooper.idleMainLooper()
        assertEquals("already on the new first", listOf(d), be.endpointsSet)
        svc.onStartCommand(rc(a, d), 0, 4)
        ShadowLooper.idleMainLooper()
        assertEquals(listOf(d, a), be.endpointsSet)
        assertTrue(TunnelState.logLines().last().endsWith("Получен новый список серверов"))
        assertEquals(0, be.fetches.get())
        assertEquals(listOf(a, b, c), Endpoints.cached(app))
        assertNull(Endpoints.working(app))
        assertEquals(Versions(), RemoteConfig.versions(app))
        assertTrue("test addresses stay out of the known file", d !in Endpoints.known)
    }

    @Test
    fun changedListWithTheSameFirstStays() {
        Endpoints.cache(app, listOf(a, b, c))
        be.config = RcValues(blob(a, d), 0, 0, "")
        start()
        awaitFetchAfter(0)
        online()
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), be.endpointsSet)
        verdict(true)
        assertEquals(listOf(d), be.endpointsSet)
    }

    @Test
    fun listThatLandedWhileConnectingSwitchesOnceUp() {
        Endpoints.cache(app, listOf(a, b))
        be.config = RcValues(blob(c, b), 0, 0, "")
        be.blockStart = true
        be.blockConfig = true
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START).putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram")), 0, 1)
        val deadline = System.currentTimeMillis() + 5_000
        while (be.startedHost == null && System.currentTimeMillis() < deadline) Thread.sleep(10)
        be.configGate.countDown()
        Thread.sleep(200)
        ShadowLooper.idleMainLooper()
        assertEquals(VpnState.CONNECTING, TunnelState.state)
        be.startGate.countDown()
        awaitConnected()
        assertEquals(a.host, be.startedSni)
        assertEquals(listOf(c), be.endpointsSet)
    }

    @Test
    fun singleEndpointNeverSwitchesNorNags() {
        be.endpoints = listOf(a)
        start()
        online()
        verdict(true)
        idle(TunnelVpnService.ROUND_PAUSE_MS * 3)
        verdict(true)
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), be.endpointsSet)
        assertEquals(0, AppLog.files(app).flatMap { it.readLines() }.count { "next round" in it })
    }

    @Test
    fun stopStartForgetsTheRound() {
        start()
        online()
        repeat(3) { verdict(true) }
        assertEquals("paused on c", listOf(b, c), be.endpointsSet)
        stop()
        start()
        assertEquals(a.host, be.startedSni)
        online()
        verdict(true)
        assertEquals(listOf(b, c, b), be.endpointsSet)
    }

    // Still on the list, just not first: only the "changed" mark tells.
    @Test
    fun changeThatLandedWhileConnectingIsNotLost() {
        Endpoints.cache(app, listOf(a, b))
        be.config = RcValues(blob(c, a), 0, 0, "")
        be.blockStart = true
        be.blockConfig = true
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START).putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram")), 0, 1)
        val deadline = System.currentTimeMillis() + 5_000
        while (be.startedHost == null && System.currentTimeMillis() < deadline) Thread.sleep(10)
        be.configGate.countDown()
        Thread.sleep(200)
        ShadowLooper.idleMainLooper()
        be.startGate.countDown()
        awaitConnected()
        assertEquals(a.host, be.startedSni)
        assertEquals(listOf(c), be.endpointsSet)
    }

    private val base get() = TunnelVpnService.ROUND_PAUSE_MS

    // Two endpoints, both dead: the first pause is pending, a base wait.
    private fun bothDeadPaused() {
        be.endpoints = listOf(a, b)
        start()
        online()
        verdict(true)
        verdict(true)
        assertEquals(listOf(b), be.endpointsSet)
    }

    // The next round comes exactly [ms] after now, not a moment sooner.
    private fun nextRoundIn(ms: Long) {
        val n = be.endpointsSet.size
        idle(ms - 1_000)
        assertEquals("not before $ms ms", n, be.endpointsSet.size)
        idle(1_000)
        assertEquals("at $ms ms", n + 1, be.endpointsSet.size)
        verdict(true)
    }

    @Test
    fun pausesGrowUpToTwelveTimes() {
        bothDeadPaused()
        for (k in listOf(1, 2, 4, 8, 12, 12)) nextRoundIn(base * k)
    }

    private fun newNetwork(id: Int) {
        netCallbacks().forEach { it.onAvailable(ShadowNetwork.newInstance(id)) }
        ShadowLooper.idleMainLooper()
    }

    private val settle = TunnelVpnService.NEW_NETWORK_SWITCH_MS

    // The current one gets its probe on the new network first; still down, the pause is over.
    @Test
    fun newNetworkEndsThePauseOnceTheCurrentHadItsChance() {
        bothDeadPaused()
        newNetwork(8)
        idle(settle - 1_000)
        assertEquals(listOf(b), be.endpointsSet)
        idle(1_000)
        assertEquals(listOf(b, a), be.endpointsSet)
        assertTrue(be.suspects.all { it })
        verdict(true)
        nextRoundIn(base)
    }

    @Test
    fun currentAnsweringOnTheNewNetworkStays() {
        bothDeadPaused()
        newNetwork(8)
        verdict(false)
        idle(settle * 2)
        assertEquals(listOf(b), be.endpointsSet)
    }

    @Test
    fun flappingNetworksSwitchOnce() {
        bothDeadPaused()
        for (id in 8..12) {
            newNetwork(id)
            idle(settle / 2)
        }
        assertEquals(listOf(b), be.endpointsSet)
        idle(settle)
        assertEquals(listOf(b, a), be.endpointsSet)
    }

    // Networks changing just slower than the settle wait, for one base pause
    // (3 changes in debug, 30 in release): one round, not one each.
    @Test
    fun steadilyChangingNetworksBuyOneRoundPerPause() {
        bothDeadPaused()
        for (id in 8 until 8 + (base / 30_000).toInt()) {
            val n = be.endpointsSet.size
            newNetwork(id)
            idle(30_000)
            // Go's verdict on each one switched to: dead too.
            if (be.endpointsSet.size > n) verdict(true)
        }
        assertEquals(listOf(b, a), be.endpointsSet)
    }

    // A stopped tunnel keeps no round: the UI binding afterwards schedules nothing.
    @Test
    fun stopLeavesNoRoundBehind() {
        bothDeadPaused()
        nextRoundIn(base)
        stop()
        register()
        assertEquals(java.time.Duration.ZERO, shadowOf(android.os.Looper.getMainLooper()).nextScheduledTaskTime)
    }

    private fun register() {
        val binder = svc.onBind(Intent(TunnelVpnService.ACTION_BIND))!!
        android.os.Messenger(binder).send(android.os.Message.obtain(null, VpnClient.MSG_REGISTER).apply {
            replyTo = android.os.Messenger(android.os.Handler(android.os.Looper.getMainLooper()))
        })
        ShadowLooper.idleMainLooper()
    }

    @Test
    fun theUserLookingBringsThePauseBackToBase() {
        bothDeadPaused()
        nextRoundIn(base)
        register()
        nextRoundIn(base)
    }

    @Test
    fun relayBackBringsThePauseBackToBase() {
        bothDeadPaused()
        nextRoundIn(base)
        verdict(false)
        verdict(true)
        verdict(true)
        nextRoundIn(base)
    }

    @Test
    fun restartBringsThePauseBackToBase() {
        bothDeadPaused()
        nextRoundIn(base)
        stop()
        start()
        online()
        verdict(true)
        verdict(true)
        nextRoundIn(base)
    }

    @Test
    fun newListBringsThePauseBackToBase() {
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400), test = true)
        fun rc(vararg eps: TunnelVpnService.Endpoint) = Intent(TunnelVpnService.ACTION_START)
            .putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram"))
            .putExtra(TunnelVpnService.EXTRA_RC_ENDPOINTS, blob(*eps).removePrefix("blob:"))
        start(rc(a, b))
        online()
        verdict(true)
        verdict(true)
        nextRoundIn(base)
        svc.onStartCommand(rc(c, d), 0, 3)
        ShadowLooper.idleMainLooper()
        assertEquals(c, be.endpointsSet.last())
        assertEquals("a list's first is not suspect", false, be.suspects.last())
        verdict(true)
        verdict(true)
        nextRoundIn(base)
    }
}
