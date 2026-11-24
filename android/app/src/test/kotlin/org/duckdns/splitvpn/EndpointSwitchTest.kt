package org.duckdns.splitvpn

import android.content.Intent
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.telephony.TelephonyManager
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
import org.robolectric.shadows.ShadowNetworkCapabilities
import org.robolectric.shadows.ShadowSubscriptionManager
import org.robolectric.shadows.ShadowSystemClock
import org.robolectric.util.ReflectionHelpers
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

    private val tm get() = app.getSystemService(TelephonyManager::class.java)
    private val lte = ShadowNetwork.newInstance(8)
    private val wifi = ShadowNetwork.newInstance(9)

    private fun caps(transport: Int) = ShadowNetworkCapabilities.newInstance().also {
        shadowOf(it).addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
        shadowOf(it).addTransportType(transport)
    }

    /** The data SIM's carrier, and the active network before the default callback: as the VPN's over cellular. */
    private fun onCell(op: String) {
        ShadowSubscriptionManager.setDefaultDataSubscriptionId(3)
        shadowOf(tm).setTelephonyManagerForSubscriptionId(3, tm)
        shadowOf(tm).setNetworkOperator(op)
        activeNetwork(NetworkCapabilities.TRANSPORT_CELLULAR)
    }

    private fun activeNetwork(transport: Int) {
        val cm = app.getSystemService(ConnectivityManager::class.java)
        shadowOf(cm).setNetworkCapabilities(cm.activeNetwork, caps(transport))
    }

    private fun defaultCallback() = ReflectionHelpers.getField<ConnectivityManager.NetworkCallback>(svc, "defaultCallback")

    /** The default network becomes [net]: onAvailable, then its capabilities, as Android tells it. */
    private fun defaultNetwork(net: android.net.Network, transport: Int) {
        defaultCallback().onAvailable(net)
        capsTick(net, transport)
    }

    private fun capsTick(net: android.net.Network, transport: Int) {
        defaultCallback().onCapabilitiesChanged(net, caps(transport))
        ShadowLooper.idleMainLooper()
    }

    private val tele2 = "cell:25020"
    private fun record(net: String = tele2, list: List<TunnelVpnService.Endpoint> = listOf(a, b, c, d), test: Boolean = false) =
        Endpoints.forNet(app, net, list, test)

    private fun fileLog() = AppLog.files(app).reversed().flatMap { it.readLines() }

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

    // The label goes along with the switch, for relay_dial.
    @Test
    fun switchCarriesTheLabel() {
        be.endpoints = listOf(a, b, c).mapIndexed { i, e -> TunnelVpnService.Endpoint(e.host, e.ip, e.port, e.path, "nl$i") }
        start()
        online()
        verdict(true)
        assertEquals(b, be.endpointsSet.last())
        assertEquals("nl1", be.endpointsSet.last().id)
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
    fun carrierRemembersItsWorkingOneAndStartsOnIt() {
        onCell("25020")
        start()
        online()
        verdict(true)
        verdict(false)
        assertEquals(b, record())
        stop()
        start()
        assertEquals(b.host, be.startedSni)
        assertEquals("a stop and a start keep it", b, record())

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
        onCell("25020")
        Endpoints.cache(app, listOf(a, b, c))
        be.config = RcValues(blob(a, b, c), 0, 0, "")
        start()
        online()
        verdict(true)
        assertEquals(listOf(b), be.endpointsSet)
        verdict(false)
        verdict(true)
        val n = be.fetches.get()
        idle(TunnelVpnService.CONFIG_RETRY_MS)
        awaitFetchAfter(n)
        assertEquals(listOf(b, c), be.endpointsSet)
        assertEquals("the same list keeps the record", b, record())
    }

    @Test
    fun changedListGoesToItsFirst() {
        onCell("25020")
        Endpoints.cache(app, listOf(a, b, c))
        start()
        online()
        verdict(true)
        verdict(false)
        assertEquals(b, record())
        be.config = RcValues(blob(d, b), 0, 0, "")
        verdict(true)
        val n = be.fetches.get()
        idle(TunnelVpnService.CONFIG_RETRY_MS)
        awaitFetchAfter(n)
        assertEquals(d, be.endpointsSet.last())
        assertTrue(TunnelState.logLines().last().endsWith("Получен новый список серверов"))
        assertNull("a new list starts from its first", record())
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
        start(Intent(TunnelVpnService.ACTION_START).putExtra(TunnelVpnService.EXTRA_ENDPOINT, "stand|test.local|10.0.2.2|8443|/app/test"))
        online()
        verdict(true)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        idle(TunnelVpnService.ROUND_PAUSE_MS)
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), be.endpointsSet)
    }

    // The instrumented tests' way in (EXTRA_RC_ENDPOINTS): switches like a fetched list, the real one stays.
    @Test
    fun testListFromRcSwitchesApartFromTheRealOne() {
        onCell("25020")
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
        assertEquals(d, record(test = true))
        svc.onStartCommand(rc(d, a), 0, 3)
        ShadowLooper.idleMainLooper()
        assertEquals("already on the new first", listOf(d), be.endpointsSet)
        svc.onStartCommand(rc(a, d), 0, 4)
        ShadowLooper.idleMainLooper()
        assertEquals(listOf(d, a), be.endpointsSet)
        assertTrue(TunnelState.logLines().last().endsWith("Получен новый список серверов"))
        assertEquals(0, be.fetches.get())
        assertEquals(listOf(a, b, c), Endpoints.cached(app))
        assertNull(record())
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

    // Only a carrier has a name: a Wi-Fi's answer is no record.
    @Test
    fun wifiRemembersNothing() {
        activeNetwork(NetworkCapabilities.TRANSPORT_WIFI)
        start()
        online()
        verdict(true)
        verdict(false)
        assertEquals(listOf(b), be.endpointsSet)
        assertNull(app.getSharedPreferences(TunnelVpnService.PREFS, android.content.Context.MODE_PRIVATE).getString("endpoint.by_net", null))
        stop()
        start()
        assertEquals(a.host, be.startedSni)
    }

    // The answer must come over the network current was chosen on: another
    // carrier's would vouch for an endpoint never tried there.
    @Test
    fun answerOnAnotherCarrierIsNoRecord() {
        onCell("25020")
        start()
        shadowOf(tm).setNetworkOperator("25099")
        verdict(false)
        assertNull(record())
        assertNull(record("cell:25099"))
    }

    // Decided before the default network's callback: the active network's.
    @Test
    fun startOnACarrierTakesItsRecord() {
        Endpoints.setForNet(app, tele2, c)
        onCell("25020")
        start()
        assertEquals(c.host, be.startedSni)
        stop()
        onCell("25001")
        start()
        assertEquals(a.host, be.startedSni)
    }

    @Test
    fun recordGoneFromTheListIsForgotten() {
        Endpoints.setForNet(app, tele2, d)
        onCell("25020")
        start()
        assertEquals(a.host, be.startedSni)
        assertNull(record())
    }

    // LTE needs c, Wi-Fi gets the first; one SetEndpoint per change, none
    // per tick, none in the UI's journal.
    @Test
    fun followsTheNetwork() {
        Endpoints.setForNet(app, tele2, c)
        onCell("25020")
        start()
        assertEquals(c.host, be.startedSni)
        defaultNetwork(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), be.endpointsSet)

        defaultNetwork(wifi, NetworkCapabilities.TRANSPORT_WIFI)
        capsTick(wifi, NetworkCapabilities.TRANSPORT_WIFI)
        assertEquals(listOf(a), be.endpointsSet)
        defaultNetwork(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        capsTick(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        assertEquals(listOf(a, c), be.endpointsSet)
        assertEquals(listOf(true, true), be.suspects)
        assertTrue(TunnelState.logLines().none { "ервер" in it })
        val log = fileLog()
        assertTrue(log.any { it.endsWith("endpoint: #1 of 3 for wifi (network)") })
        assertTrue(log.any { it.endsWith("endpoint: #3 of 3 for cell:25020 (network)") })
    }

    // The code may come with a later callback: no move to the first and back meanwhile.
    @Test
    fun lateCarrierCodeSwitchesOnce() {
        Endpoints.setForNet(app, tele2, c)
        onCell("")
        activeNetwork(NetworkCapabilities.TRANSPORT_WIFI)
        start()
        online()
        verdict(true)
        assertEquals(listOf(b), be.endpointsSet)
        defaultNetwork(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        assertEquals(listOf(b), be.endpointsSet)
        shadowOf(tm).setNetworkOperator("25020")
        capsTick(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        assertEquals(listOf(b, c), be.endpointsSet)
    }

    // A network without a record mid-round: ticks must not pull it back to the first.
    @Test
    fun capsTicksDoNotUndoTheRound() {
        activeNetwork(NetworkCapabilities.TRANSPORT_WIFI)
        start()
        defaultNetwork(wifi, NetworkCapabilities.TRANSPORT_WIFI)
        online()
        verdict(true)
        repeat(3) { capsTick(wifi, NetworkCapabilities.TRANSPORT_WIFI) }
        assertEquals(listOf(b), be.endpointsSet)
    }

    // The old network's failures do not count towards the pause on the new one.
    @Test
    fun networkSwitchStartsTheRoundAfresh() {
        onCell("25020")
        activeNetwork(NetworkCapabilities.TRANSPORT_WIFI)
        start()
        defaultNetwork(wifi, NetworkCapabilities.TRANSPORT_WIFI)
        online()
        verdict(true)
        verdict(true)
        assertEquals(listOf(b, c), be.endpointsSet)
        defaultNetwork(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        assertEquals(listOf(b, c, a), be.endpointsSet)
        verdict(true)
        assertEquals(listOf(b, c, a, b), be.endpointsSet)
        assertTrue(be.suspects.all { it })
        assertEquals(3, TunnelState.logLines().count { "Сервер недоступен" in it })
    }

    // A rebuild holds the switch back; it lands once the rebuild is done.
    @Test
    fun networkChangeDuringARebuildLandsAfter() {
        Endpoints.setForNet(app, tele2, c)
        onCell("25020")
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START).putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("twitter")), 0, 1)
        awaitConnected()
        defaultNetwork(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        be.blockStart = true
        RouteCache.file(app).writeText("x.com 192.0.2.0 ${System.currentTimeMillis() / 1000}\n")
        be.startedHost!!.routesStale()
        ShadowLooper.idleMainLooper()
        val deadline = System.currentTimeMillis() + 5_000
        while ("stop" !in be.events && System.currentTimeMillis() < deadline) Thread.sleep(10)
        defaultNetwork(wifi, NetworkCapabilities.TRANSPORT_WIFI)
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), be.endpointsSet)
        be.startGate.countDown()
        awaitServiceThreads()
        ShadowLooper.idleMainLooper()
        assertEquals(listOf(a), be.endpointsSet)
        RouteCache.file(app).delete()
    }

    @Test
    fun instrumentedTestRelayIgnoresTheNetwork() {
        Endpoints.setForNet(app, tele2, b)
        onCell("25020")
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400), test = true)
        start(Intent(TunnelVpnService.ACTION_START).putExtra(TunnelVpnService.EXTRA_ENDPOINT, "stand|test.local|10.0.2.2|8443|/app/test"))
        defaultNetwork(wifi, NetworkCapabilities.TRANSPORT_WIFI)
        defaultNetwork(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), be.endpointsSet)
        assertEquals("the real record stays", b, record())
    }

    // Android 8, or the same endpoint across carriers: no switch, yet the answer counts there.
    @Test
    fun noSwitchStillTakesTheNetwork() {
        onCell("25020")
        activeNetwork(NetworkCapabilities.TRANSPORT_WIFI)
        start()
        defaultNetwork(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), be.endpointsSet)
        verdict(false)
        assertEquals(a, record())
    }

    // A code-less LTE stint between: back on the same Wi-Fi is decided again.
    @Test
    fun backOnTheSameWifiAfterCodelessLte() {
        onCell("")
        activeNetwork(NetworkCapabilities.TRANSPORT_WIFI)
        start()
        defaultNetwork(wifi, NetworkCapabilities.TRANSPORT_WIFI)
        online()
        verdict(true)
        assertEquals(listOf(b), be.endpointsSet)
        defaultNetwork(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        defaultNetwork(wifi, NetworkCapabilities.TRANSPORT_WIFI)
        assertEquals(listOf(b, a), be.endpointsSet)
    }

    private fun expired(ep: TunnelVpnService.Endpoint) = Endpoints.setForNet(app, tele2, ep, now = System.currentTimeMillis() - Endpoints.NET_TTL_MS - 1)

    @Test
    fun expiredRecordIsIgnoredAtStart() {
        expired(c)
        onCell("25020")
        start()
        assertEquals(a.host, be.startedSni)
        assertNull(record())
    }

    @Test
    fun expiredRecordIsIgnoredOnANetworkChange() {
        onCell("25020")
        activeNetwork(NetworkCapabilities.TRANSPORT_WIFI)
        start()
        online()
        verdict(true)
        assertEquals(listOf(b), be.endpointsSet)
        expired(c)
        defaultNetwork(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        assertEquals(listOf(b, a), be.endpointsSet)
    }

    // Flapping between two networks whose choices are both dead: the round
    // starts afresh once per base pause, not on every change.
    @Test
    fun flappingNetworksRestartTheRoundOncePerPause() {
        onCell("25020")
        activeNetwork(NetworkCapabilities.TRANSPORT_WIFI)
        start()
        defaultNetwork(wifi, NetworkCapabilities.TRANSPORT_WIFI)
        online()
        repeat(3) { verdict(true) }
        assertEquals("paused on c", listOf(b, c), be.endpointsSet)
        defaultNetwork(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        repeat(3) { verdict(true) }
        assertEquals(listOf(b, c, a, b, c), be.endpointsSet)
        defaultNetwork(wifi, NetworkCapabilities.TRANSPORT_WIFI)
        verdict(true)
        assertEquals("still paused", listOf(b, c, a, b, c, a), be.endpointsSet)
    }

    // The round left the record's c: a code-less tick and the code back on the same LTE must not return to it.
    @Test
    fun codelessTickDoesNotUndoTheRound() {
        Endpoints.setForNet(app, tele2, c)
        onCell("25020")
        start()
        assertEquals(c.host, be.startedSni)
        defaultNetwork(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        online()
        verdict(true)
        assertEquals(listOf(a), be.endpointsSet)
        shadowOf(tm).setNetworkOperator("")
        capsTick(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        shadowOf(tm).setNetworkOperator("25020")
        capsTick(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        assertEquals(listOf(a), be.endpointsSet)
    }
}
