package org.duckdns.splitvpn

import android.content.Context
import android.content.Intent
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import org.junit.After
import org.junit.Assert.assertEquals
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
import org.robolectric.util.ReflectionHelpers
import java.time.LocalDate
import java.time.ZoneId
import java.util.concurrent.TimeUnit

/** What :vpn tells Analytics, when, and that each event goes at most once a day. */
@RunWith(RobolectricTestRunner::class)
class AnalyticsTest {

    private lateinit var controller: ServiceController<TunnelVpnService>
    private val app get() = RuntimeEnvironment.getApplication()
    private val svc get() = controller.get()
    private val be = FakeBackend()
    private val nowS get() = System.currentTimeMillis() / 1000
    private var day = 20_000L

    @Before
    fun setUp() {
        Quota.today = { day }
        TunnelState.set(VpnState.DISCONNECTED)
        TunnelState.clearLog()
        ShadowLooper.idleMainLooper()
        app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE).edit().clear().commit()
        RouteCache.file(app).delete()
        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
        svc.backend = be
        Credentials.save(app, fakeCred(nowS + 5 * 86400))
        ShadowLooper.idleMainLooper()
    }

    @After
    fun tearDown() {
        Quota.today = { LocalDate.now().toEpochDay() }
        be.release()
        awaitServiceThreads()
        controller.destroy()
        ShadowLooper.idleMainLooper()
        RouteCache.file(app).delete()
    }

    private fun noon(d: Long) = LocalDate.ofEpochDay(d).atTime(12, 0).atZone(ZoneId.systemDefault()).toEpochSecond()

    private fun sent(name: String) = be.analytics.filter { it.first == name }

    private fun reasons() = sent("vpn_failed").map { it.second["reason"] }

    // Unit tests are a debug build: collection only with the extra.
    private fun startIntent(vararg services: String = arrayOf("telegram"), analytics: Boolean = true) =
        Intent(TunnelVpnService.ACTION_START).putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf(*services))
            .putExtra(TunnelVpnService.EXTRA_ANALYTICS, analytics)

    private fun start(intent: Intent = startIntent()) {
        svc.onStartCommand(intent, 0, 1)
        awaitState(VpnState.CONNECTED, VpnState.ERROR)
    }

    private fun stop() {
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_STOP), 0, 2)
        awaitServiceThreads()
        ShadowLooper.idleMainLooper(4, TimeUnit.SECONDS)
    }

    private fun awaitState(vararg want: VpnState) {
        val deadline = System.currentTimeMillis() + 10_000
        // CONNECTED comes a moment before the receiver is registered.
        while ((TunnelState.state !in want || TunnelState.state == VpnState.CONNECTED && screenReceivers().isEmpty()) &&
            System.currentTimeMillis() < deadline
        ) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, TimeUnit.MILLISECONDS)
        }
        assertTrue("state=${TunnelState.state} err=${TunnelState.lastError}", TunnelState.state in want)
        ShadowLooper.idleMainLooper()
    }

    private fun screenReceivers() = shadowOf(app).registeredReceivers.filter { it.intentFilter.hasAction(Intent.ACTION_SCREEN_ON) }

    private fun screen(action: String) {
        app.sendBroadcast(Intent(action))
        ShadowLooper.idleMainLooper()
    }

    private fun netCallbacks() = shadowOf(app.getSystemService(ConnectivityManager::class.java)).networkCallbacks.toList()

    private fun defaultCallback() = ReflectionHelpers.getField<ConnectivityManager.NetworkCallback>(svc, "defaultCallback")

    private fun online(netId: Int) {
        netCallbacks().forEach { it.onAvailable(ShadowNetwork.newInstance(netId)) }
        ShadowLooper.idleMainLooper()
    }

    /** What the default network's callback hears about Android's own internet check. */
    private fun validated(netId: Int, yes: Boolean = true) {
        val caps = ShadowNetworkCapabilities.newInstance()
        shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
        if (yes) shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
        defaultCallback().onCapabilitiesChanged(ShadowNetwork.newInstance(netId), caps)
        ShadowLooper.idleMainLooper()
    }

    /** hasInternet() for the registration path. */
    private fun activeNetworkValidated(yes: Boolean) {
        val cm = app.getSystemService(ConnectivityManager::class.java)
        val caps = ShadowNetworkCapabilities.newInstance()
        if (yes) {
            shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
        }
        shadowOf(cm).setNetworkCapabilities(cm.activeNetwork, caps)
    }

    private fun verdict(down: Boolean) {
        be.startedHost!!.relayDown(down)
        ShadowLooper.idleMainLooper()
    }

    private fun advance(ms: Long) = ShadowLooper.idleMainLooper(ms, TimeUnit.MILLISECONDS)

    @Test
    fun activeOnceADayWhenTheRelayLetASessionIn() {
        be.lastRelayOk = noon(day)
        start()
        assertEquals(listOf("vpn_active" to emptyMap<String, String>()), be.analytics.toList())
        online(7)
        screen(Intent.ACTION_SCREEN_ON)
        assertEquals("the same day again", 1, sent("vpn_active").size)
        day++
        be.lastRelayOk = noon(day)
        screen(Intent.ACTION_SCREEN_ON)
        assertEquals(2, sent("vpn_active").size)
    }

    @Test
    fun relayBackLaterMakesTheDayActive() {
        be.relayDownAtStart = true
        start()
        assertTrue(sent("vpn_active").isEmpty())
        be.lastRelayOk = noon(day)
        verdict(false)
        assertEquals(1, sent("vpn_active").size)
    }

    // A tunnel restarted after midnight in the same process: Go's last
    // success is yesterday's and the relay does not answer today.
    @Test
    fun yesterdaysSuccessIsNotToday() {
        be.lastRelayOk = noon(day - 1)
        start()
        screen(Intent.ACTION_SCREEN_ON)
        assertTrue(sent("vpn_active").isEmpty())
        be.lastRelayOk = noon(day)
        screen(Intent.ACTION_SCREEN_ON)
        assertEquals(1, sent("vpn_active").size)
    }

    // Unlocked, opened Telegram: Go tells nobody about an app's session,
    // the screen going off is the next look.
    @Test
    fun successAfterScreenOnCountsOnScreenOff() {
        start()
        screen(Intent.ACTION_SCREEN_ON)
        be.lastRelayOk = noon(day)
        ShadowLooper.idleMainLooper()
        assertTrue(sent("vpn_active").isEmpty())
        screen(Intent.ACTION_SCREEN_OFF)
        assertEquals(1, sent("vpn_active").size)
    }

    // The offline retry loop and resumes on every bind: one event a day.
    @Test
    fun offlineIsNotActiveAndFailsOnceADay() {
        be.lastRelayOk = noon(day)
        activeNetworkValidated(false)
        Credentials.save(app, fakeCred(nowS - 10))
        start()
        assertEquals(TunnelVpnService.ERR_OFFLINE, TunnelState.lastError)
        start()
        assertEquals(TunnelVpnService.ERR_OFFLINE, TunnelState.lastError)
        assertEquals(listOf("offline"), reasons())
        assertTrue(sent("vpn_active").isEmpty())
    }

    @Test
    fun failedStartsCarryTheirReasonNotTheirText() {
        activeNetworkValidated(true)
        Credentials.save(app, fakeCred(nowS - 10))
        be.token = "tok".toByteArray()
        be.registerError = Exception("registration rejected")
        start()
        assertEquals(TunnelVpnService.ERR_NEED_CODE, TunnelState.lastError)
        be.registerError = Exception("relay reply: EOF")
        start()
        assertEquals(TunnelVpnService.ERR_NO_SERVER, TunnelState.lastError)
        Credentials.save(app, fakeCred(nowS + 5 * 86400))
        be.startError = IllegalStateException("relay unreachable: dial tcp 203.0.113.10:443")
        start()
        assertEquals(VpnState.ERROR, TunnelState.state)
        assertEquals(
            listOf("need_code", "no_server", "other").map { "vpn_failed" to mapOf("reason" to it) },
            be.analytics.toList(),
        )
    }

    @Test
    fun reasonIsOneOfOursOrOther() {
        mapOf(
            TunnelVpnService.ERR_NEED_CODE to "need_code",
            TunnelVpnService.ERR_NO_SERVER to "no_server",
            TunnelVpnService.ERR_OFFLINE to "offline",
            TunnelVpnService.ERR_UPDATE_REQUIRED to "update_required",
            TunnelVpnService.ERR_QUOTA to "quota",
            TunnelVpnService.ERR_ESTABLISH to "establish",
            "no endpoints: empty list" to "other",
            "Не выбран ни один сервис" to "other",
            "credential rejected" to "other",
            "fdbased: bad file descriptor" to "other",
            "already running" to "other",
            "${TunnelVpnService.ERR_ESTABLISH}: 203.0.113.10" to "other",
        ).forEach { (msg, want) -> assertEquals(msg, want, TunnelVpnService.reasonOf(msg)) }
    }

    @Test
    fun failedRebuildIsRebuildAndTakesTheReceiverAlong() {
        start(startIntent("twitter"))
        assertEquals(1, screenReceivers().size)
        RouteCache.file(app).writeText("x.com 192.0.2.0 $nowS\n")
        be.startError = IllegalStateException("fdbased: bad file descriptor")
        be.startedHost!!.routesStale()
        awaitState(VpnState.ERROR)
        assertEquals(listOf("rebuild"), reasons())
        assertTrue(screenReceivers().isEmpty())
    }

    @Test
    fun screenReceiverOnlyWhileConnected() {
        be.startError = IllegalStateException("relay unreachable")
        start()
        assertEquals("a failure before CONNECTED has nothing to unregister", VpnState.ERROR, TunnelState.state)
        assertTrue(screenReceivers().isEmpty())
        be.startError = null
        start()
        assertEquals(1, screenReceivers().size)
        stop()
        assertTrue(screenReceivers().isEmpty())
    }

    // fail() on the rebuild thread against stopTunnel() on main: the window
    // between the rebuild's stopping check and its fail() cannot be hit on
    // purpose, so the two unwatch calls race here directly. Each callback
    // and the receiver must be let go once, by one of them.
    @Test
    fun concurrentUnwatchLetsGoOnce() {
        start()
        val errors = java.util.concurrent.CopyOnWriteArrayList<Throwable>()
        repeat(200) { round ->
            if (round > 0) {
                ReflectionHelpers.callInstanceMethod<Unit>(svc, "watchNetwork")
                ReflectionHelpers.callInstanceMethod<Unit>(svc, "watchScreen")
            }
            val barrier = java.util.concurrent.CyclicBarrier(2)
            List(2) {
                Thread {
                    try {
                        barrier.await()
                        ReflectionHelpers.callInstanceMethod<Unit>(svc, "unwatchNetwork")
                        ReflectionHelpers.callInstanceMethod<Unit>(svc, "unwatchScreen")
                    } catch (e: Throwable) {
                        errors += e
                    }
                }.apply { start() }
            }.forEach { it.join() }
            assertTrue(netCallbacks().isEmpty())
            assertTrue(screenReceivers().isEmpty())
        }
        assertEquals(emptyList<Throwable>(), errors.map { it.cause ?: it })
    }

    @Test
    fun usedUpQuotaIsQuotaExhaustedNotAFailure() {
        start()
        be.used = Quota.limit(app)
        be.startedHost!!.quotaExceeded()
        awaitState(VpnState.ERROR)
        awaitServiceThreads()
        assertEquals(1, sent("quota_exhausted").size)
        assertTrue(reasons().isEmpty())
        // A tap before midnight is a refused start.
        start()
        assertEquals(TunnelVpnService.ERR_QUOTA, TunnelState.lastError)
        assertEquals(listOf("quota"), reasons())
    }

    @Test
    fun midnightOrARaisedLimitIsNoExhaustion() {
        start()
        be.used = Quota.limit(app)
        day++
        be.startedHost!!.quotaExceeded()
        ShadowLooper.idleMainLooper()
        be.used = Quota.limit(app)
        Quota.setLimitMb(app, 2 * Quota.DEFAULT_MB)
        be.startedHost!!.quotaExceeded()
        ShadowLooper.idleMainLooper()
        assertEquals(VpnState.CONNECTED, TunnelState.state)
        assertTrue(sent("quota_exhausted").isEmpty())
    }

    @Test
    fun relayDownWithInternetOnceTheNetworkSettled() {
        start()
        online(7)
        validated(7)
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        verdict(true)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        assertEquals(listOf("relay_down"), reasons())
        verdict(false)
        verdict(true)
        assertEquals("once a day", listOf("relay_down"), reasons())
    }

    // Wi-Fi without internet, a captive portal: "no server" to the UI, not
    // our relay being down.
    @Test
    fun noServerWithoutValidationWaitsForIt() {
        start()
        online(7)
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        verdict(true)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        assertTrue(reasons().isEmpty())
        validated(7)
        assertEquals(listOf("relay_down"), reasons())
    }

    @Test
    fun noNetworkIsNotRelayDown() {
        start()
        online(7)
        validated(7)
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        ReflectionHelpers.getField<ConnectivityManager.NetworkCallback>(svc, "netCallback").onLost(ShadowNetwork.newInstance(7))
        verdict(true)
        advance(5_000)
        assertEquals(Waiting.NO_NETWORK, TunnelState.waiting)
        assertTrue(reasons().isEmpty())
    }

    @Test
    fun validationGoneIsNotRelayDown() {
        start()
        online(7)
        validated(7)
        validated(7, yes = false)
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        verdict(true)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        assertTrue(reasons().isEmpty())
    }

    // Below S the callback is a request: a new default network comes as an
    // available without a lost for the old one.
    @Test
    fun oldNetworksValidationDoesNotCarryOver() {
        start()
        online(7)
        validated(7)
        defaultCallback().onAvailable(ShadowNetwork.newInstance(8))
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        verdict(true)
        assertTrue(reasons().isEmpty())
        validated(8)
        assertEquals(listOf("relay_down"), reasons())
    }

    // The same network again after a restart: its old validation must not
    // stand in for one the new watch never heard.
    @Test
    fun validationIsForgottenWithTheNetworkWatch() {
        start()
        online(7)
        validated(7)
        stop()
        start()
        online(7)
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        verdict(true)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        assertTrue(reasons().isEmpty())
    }

    // Wi-Fi (no internet, blocks the VDS) and LTE both up; the Wi-Fi goes
    // and LTE, up for long, becomes the default: Go has only begun to try
    // the relay on it.
    @Test
    fun knownNetworkBecomingTheDefaultIsNewToTheRelay() {
        start()
        online(7)
        online(8)
        validated(7, yes = false)
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        verdict(true)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        advance(5_000)
        ReflectionHelpers.getField<ConnectivityManager.NetworkCallback>(svc, "netCallback").onLost(ShadowNetwork.newInstance(7))
        defaultCallback().onAvailable(ShadowNetwork.newInstance(8))
        validated(8)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        assertTrue(reasons().isEmpty())
        // The next look, the no-server fetch, comes after the switch time.
        advance(TunnelVpnService.CONFIG_RETRY_MS - 5_000)
        assertEquals(listOf("relay_down"), reasons())
    }

    // Right after a new network Go is still dialing the relay on it: a
    // relay back within that time was never down for this network.
    @Test
    fun newNetworkValidatedEarlyThenRelayBackIsNothing() {
        start()
        online(7)
        verdict(true)
        advance(10_000)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        online(8)
        advance(5_000)
        validated(8)
        verdict(false)
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        assertTrue(reasons().isEmpty())
    }

    @Test
    fun stillDownPastTheSwitchTimeIsRelayDown() {
        start()
        online(7)
        verdict(true)
        advance(10_000)
        online(8)
        advance(5_000)
        validated(8)
        assertTrue(reasons().isEmpty())
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS - 5_000)
        assertEquals(listOf("relay_down"), reasons())
    }

    @Test
    fun adBlockPropertyFollowsTheStart() {
        start(startIntent().putExtra(TunnelVpnService.EXTRA_ADBLOCK, true))
        assertEquals("on", be.userProperties["adblock"])
        stop()
        start(startIntent().putExtra(TunnelVpnService.EXTRA_ADBLOCK, false))
        assertEquals("off", be.userProperties["adblock"])
    }

    // The SDK's flag sticks across runs: a test run must not leave it on.
    @Test
    fun debugCollectionOnlyWithTheExtra() {
        start(startIntent(analytics = true))
        assertEquals(true, be.analyticsOn)
        stop()
        start(Intent(TunnelVpnService.ACTION_START).putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram")))
        assertEquals(false, be.analyticsOn)
    }

    // A run without collection (make itest) must leave the day free for
    // one with it: DebugView the same day.
    @Test
    fun nothingIsMarkedWhileCollectionIsOff() {
        be.lastRelayOk = noon(day)
        start(startIntent(analytics = false))
        assertEquals(VpnState.CONNECTED, TunnelState.state)
        assertTrue(be.analytics.isEmpty())
        stop()
        start(startIntent(analytics = true))
        assertEquals(listOf("vpn_active" to emptyMap<String, String>()), be.analytics.toList())
    }
}
