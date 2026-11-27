package org.duckdns.splitvpn

import android.app.Notification
import android.app.NotificationManager
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.telephony.TelephonyManager
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
import org.robolectric.shadows.ShadowSubscriptionManager
import org.robolectric.util.ReflectionHelpers
import java.time.LocalDate
import java.util.concurrent.TimeUnit

/** NO_SERVER on a carrier's white lists: the probe, what the UI is told, relay_down and whitelist_detected. */
@RunWith(RobolectricTestRunner::class)
class WhitelistTest {

    private lateinit var controller: ServiceController<TunnelVpnService>
    private val app get() = RuntimeEnvironment.getApplication()
    private val svc get() = controller.get()
    private val be = FakeBackend()
    private var day = 20_000L
    private val shown = mutableListOf<Waiting?>()
    private val tm get() = app.getSystemService(TelephonyManager::class.java)

    @Before
    fun setUp() {
        Quota.today = { day }
        TunnelState.set(VpnState.DISCONNECTED)
        TunnelState.clearLog()
        ShadowLooper.idleMainLooper()
        app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE).edit().clear().commit()
        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
        svc.backend = be
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400))
        ShadowSubscriptionManager.setDefaultDataSubscriptionId(3)
        shadowOf(tm).setTelephonyManagerForSubscriptionId(3, tm)
        shadowOf(tm).setNetworkOperator("25020")
        shadowOf(app).setComponentNameAndServiceForBindService(
            ComponentName(app, TunnelVpnService::class.java), svc.onBind(Intent(TunnelVpnService.ACTION_BIND))!!,
        )
        VpnClient(app) { _, _, _, _, w, _, _, _ -> shown += w }.bind()
        ShadowLooper.idleMainLooper()
    }

    @After
    fun tearDown() {
        Quota.today = { LocalDate.now().toEpochDay() }
        be.release()
        awaitServiceThreads()
        controller.destroy()
        ShadowLooper.idleMainLooper()
    }

    private fun start(intent: Intent = Intent(TunnelVpnService.ACTION_START)) {
        svc.onStartCommand(
            intent.putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram")).putExtra(TunnelVpnService.EXTRA_ANALYTICS, true),
            0, 1,
        )
        val deadline = System.currentTimeMillis() + 10_000
        while ((TunnelState.state != VpnState.CONNECTED || netCallbacks().isEmpty()) && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, TimeUnit.MILLISECONDS)
        }
        assertEquals(TunnelState.lastError ?: "", VpnState.CONNECTED, TunnelState.state)
        ShadowLooper.idleMainLooper()
    }

    private fun netCallbacks() = shadowOf(app.getSystemService(ConnectivityManager::class.java)).networkCallbacks.toList()
    private fun defaultCallback() = ReflectionHelpers.getField<ConnectivityManager.NetworkCallback>(svc, "defaultCallback")

    /** [net] up and the default network, by [transport]. */
    private fun network(net: Network, transport: Int, validated: Boolean = false) {
        netCallbacks().forEach { it.onAvailable(net) }
        defaultCallback().onAvailable(net)
        caps(net, transport, validated)
    }

    private fun caps(net: Network, transport: Int, validated: Boolean = false) {
        val caps = ShadowNetworkCapabilities.newInstance()
        shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
        if (validated) shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
        shadowOf(caps).addTransportType(transport)
        defaultCallback().onCapabilitiesChanged(net, caps)
        ShadowLooper.idleMainLooper()
    }

    private fun verdict(down: Boolean) {
        be.startedHost!!.relayDown(down)
        ShadowLooper.idleMainLooper()
    }

    private fun advance(ms: Long) = ShadowLooper.idleMainLooper(ms, TimeUnit.MILLISECONDS)

    private fun awaitProbe() {
        awaitServiceThreads()
        ShadowLooper.idleMainLooper()
    }

    private fun awaitProbes(n: Int) {
        val deadline = System.currentTimeMillis() + 5_000
        while (be.whitelistProbes.size < n && System.currentTimeMillis() < deadline) Thread.sleep(10)
        assertEquals(n, be.whitelistProbes.size)
    }

    private fun notificationText() = shadowOf(app.getSystemService(NotificationManager::class.java))
        .getNotification(1)?.extras?.getCharSequence(Notification.EXTRA_TEXT)?.toString()

    private fun relayDowns() = be.analytics.filter { it.first == "vpn_failed" && it.second["reason"] == "relay_down" }
    private fun detected() = be.analytics.filter { it.first == "whitelist_detected" }.map { it.second }

    private val lte = ShadowNetwork.newInstance(7)

    /** On LTE, settled, the relay down: the probe has answered [answer], or hangs on the gate. */
    private fun whitelisted(answer: Pair<Int, Int> = 2 to 0, validated: Boolean = false) {
        start()
        network(lte, NetworkCapabilities.TRANSPORT_CELLULAR, validated)
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        be.whitelistAnswer = answer
        verdict(true)
        if (be.blockWhitelist) awaitProbes(1) else awaitProbe()
    }

    @Test
    fun verdictTable() {
        assertEquals(Outage.WHITELIST, TunnelVpnService.whitelistVerdict(2, 2, 0))
        assertEquals(Outage.SERVER_DOWN, TunnelVpnService.whitelistVerdict(2, 2, 1))
        assertEquals(Outage.SERVER_DOWN, TunnelVpnService.whitelistVerdict(2, 2, 2))
        // The package used up: vk.com alone is free.
        assertEquals(Outage.UNCLEAR, TunnelVpnService.whitelistVerdict(1, 2, 0))
        assertEquals(Outage.UNCLEAR, TunnelVpnService.whitelistVerdict(0, 2, 0))
        assertEquals(Outage.UNCLEAR, TunnelVpnService.whitelistVerdict(1, 2, 2))
    }

    @Test
    fun noServerOnCellularIsShownAsWhitelistUntilTheRelayIsBack() {
        whitelisted()
        assertEquals(listOf(lte.networkHandle), be.whitelistProbes)
        assertEquals("TunnelState keeps its own", Waiting.NO_SERVER, TunnelState.waiting)
        assertEquals(Waiting.WHITELIST, shown.last())
        assertEquals("Мобильный интернет ограничен · подключится автоматически", notificationText())

        verdict(false)
        assertEquals(null, shown.last())
        // A new episode: its own probe, and until it answers, the server.
        be.blockWhitelist = true
        verdict(true)
        assertEquals(Waiting.NO_SERVER, shown.last())
        assertEquals("Сервер недоступен · подключится автоматически", notificationText())
        awaitProbes(2)
        be.whitelistAnswer = 2 to 1
        be.whitelistGate.countDown()
        awaitProbe()
        assertEquals(Waiting.NO_SERVER, shown.last())
    }

    @Test
    fun unclearIsTheServerAsBefore() {
        whitelisted(answer = 1 to 0)
        assertEquals(1, be.whitelistProbes.size)
        assertEquals(Waiting.NO_SERVER, shown.last())
        assertTrue(detected().isEmpty())
    }

    @Test
    fun newDefaultNetworkDuringTheProbeDropsItsResult() {
        be.blockWhitelist = true
        whitelisted()
        val lte2 = ShadowNetwork.newInstance(8)
        network(lte2, NetworkCapabilities.TRANSPORT_CELLULAR)
        assertEquals("one probe at a time", listOf(lte.networkHandle), be.whitelistProbes)
        assertEquals(Waiting.NO_SERVER, shown.last())
        be.whitelistGate.countDown()
        awaitProbe()
        // The old one's result dropped, the new network's probe goes at once, not once it has settled.
        awaitProbes(2)
        awaitProbe()
        assertEquals(listOf(lte.networkHandle, lte2.networkHandle), be.whitelistProbes)
        assertEquals(Waiting.WHITELIST, shown.last())
    }

    // The start's NO_SERVER comes before the default network's caps: the
    // probe goes with them, not after the switch time.
    @Test
    fun startOnLteProbesAtOnce() {
        start()
        netCallbacks().forEach { it.onAvailable(lte) }
        be.whitelistAnswer = 2 to 0
        verdict(true)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        assertTrue(be.whitelistProbes.isEmpty())
        defaultCallback().onAvailable(lte)
        caps(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        awaitProbe()
        assertEquals(listOf(lte.networkHandle), be.whitelistProbes)
        assertEquals(Waiting.WHITELIST, shown.last())
    }

    // Only the probe goes at once: relay_down waits for Go to judge the relay on the new network.
    @Test
    fun serverDownBeforeTheSwitchTimeIsRelayDownOnlyAfterIt() {
        start()
        network(lte, NetworkCapabilities.TRANSPORT_CELLULAR, validated = true)
        be.whitelistAnswer = 2 to 1
        verdict(true)
        awaitProbe()
        assertEquals(1, be.whitelistProbes.size)
        assertTrue(relayDowns().isEmpty())
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        awaitProbe()
        assertEquals(1, relayDowns().size)
        assertEquals(1, be.whitelistProbes.size)
    }

    // A new LTE not ready yet fails the allowed hosts too: the settled check probes again.
    @Test
    fun unclearBeforeTheSwitchTimeProbesAgainAfterIt() {
        start()
        network(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        be.whitelistAnswer = 0 to 0
        verdict(true)
        awaitProbe()
        assertEquals(1, be.whitelistProbes.size)
        assertEquals(Waiting.NO_SERVER, shown.last())
        be.whitelistAnswer = 2 to 0
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        awaitProbes(2)
        awaitProbe()
        assertEquals(listOf(lte.networkHandle, lte.networkHandle), be.whitelistProbes)
        assertEquals(Waiting.WHITELIST, shown.last())
    }

    // Connects on a not-ready network hang till the probe's deadline: an early
    // probe that answers past the switch time probes again at once.
    @Test
    fun unclearFromAnEarlyProbeAfterTheSwitchTimeProbesAgainAtOnce() {
        be.blockWhitelist = true
        start()
        network(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        be.whitelistAnswer = 0 to 0
        verdict(true)
        awaitProbes(1)
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        be.whitelistGate.countDown()
        // Not awaitServiceThreads: its looper idling would start the second probe before the new answer.
        val deadline = System.currentTimeMillis() + 5_000
        while (Thread.getAllStackTraces().keys.any { it.isAlive && it.name == "WhitelistProbe" } && System.currentTimeMillis() < deadline) Thread.sleep(10)
        be.whitelistAnswer = 2 to 0
        ShadowLooper.idleMainLooper()
        awaitProbes(2)
        awaitProbe()
        assertEquals(listOf(lte.networkHandle, lte.networkHandle), be.whitelistProbes)
        assertEquals(Waiting.WHITELIST, shown.last())
    }

    // The new default network's own probe only: none over the old one's caps from onAvailable.
    @Test
    fun anotherLteProbesItselfOnly() {
        whitelisted()
        val lte2 = ShadowNetwork.newInstance(8)
        network(lte2, NetworkCapabilities.TRANSPORT_CELLULAR)
        awaitProbe()
        assertEquals(listOf(lte.networkHandle, lte2.networkHandle), be.whitelistProbes)
        assertEquals(Waiting.WHITELIST, shown.last())
    }

    @Test
    fun noProbeOnLeavingForAWifi() {
        whitelisted()
        network(ShadowNetwork.newInstance(8), NetworkCapabilities.TRANSPORT_WIFI)
        awaitProbe()
        assertEquals(listOf(lte.networkHandle), be.whitelistProbes)
    }

    @Test
    fun leavingForAWifiClearsItAtOnce() {
        whitelisted()
        assertEquals(Waiting.WHITELIST, shown.last())
        network(ShadowNetwork.newInstance(8), NetworkCapabilities.TRANSPORT_WIFI)
        assertEquals(Waiting.NO_SERVER, shown.last())
        assertEquals("Сервер недоступен · подключится автоматически", notificationText())
    }

    // The relay back: no flash of "server unavailable" on the way to connected.
    @Test
    fun relayBackGoesStraightToConnected() {
        whitelisted()
        val n = shown.size
        verdict(false)
        assertEquals(listOf<Waiting?>(null), shown.drop(n).distinct())
        assertEquals("VPN включён", notificationText())
    }

    @Test
    fun relayBackAndDownAgainDuringTheProbeProbesOnceItReturns() {
        be.blockWhitelist = true
        whitelisted()
        verdict(false)
        verdict(true)
        assertEquals(1, be.whitelistProbes.size)
        assertEquals(Waiting.NO_SERVER, shown.last())
        be.whitelistGate.countDown()
        awaitProbe()
        // The first one's result posts the second probe; wait for it and its own result.
        awaitProbes(2)
        awaitProbe()
        assertEquals(2, be.whitelistProbes.size)
        assertEquals(Waiting.WHITELIST, shown.last())
    }

    private fun stop() {
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_STOP), 0, 2)
        awaitServiceThreads()
        ShadowLooper.idleMainLooper(4, TimeUnit.SECONDS)
    }

    @Test
    fun aRestartForgetsTheVerdict() {
        whitelisted()
        stop()
        be.blockWhitelist = true
        start()
        network(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        verdict(true)
        awaitProbes(2)
        assertEquals(Waiting.NO_SERVER, shown.last())
    }

    @Test
    fun noProbeOnWifi() {
        start()
        network(ShadowNetwork.newInstance(8), NetworkCapabilities.TRANSPORT_WIFI, validated = true)
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        verdict(true)
        advance(TunnelVpnService.CONFIG_RETRY_MS)
        awaitProbe()
        assertTrue(be.whitelistProbes.isEmpty())
        assertEquals(1, relayDowns().size)
    }

    // LTE up for long beside a Wi-Fi without internet; the Wi-Fi goes: the
    // probe comes as LTE becomes the default, not after the switch time nor
    // with the no-server fetch's next tick.
    @Test
    fun lteBecomingTheDefaultProbesAtOnce() {
        start()
        val wifi = ShadowNetwork.newInstance(8)
        netCallbacks().forEach { it.onAvailable(lte) }
        network(wifi, NetworkCapabilities.TRANSPORT_WIFI)
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        verdict(true)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        advance(5_000)
        ReflectionHelpers.getField<ConnectivityManager.NetworkCallback>(svc, "netCallback").onLost(wifi)
        be.whitelistAnswer = 2 to 0
        defaultCallback().onAvailable(lte)
        assertTrue(be.whitelistProbes.isEmpty())
        caps(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        awaitProbe()
        assertEquals(listOf(lte.networkHandle), be.whitelistProbes)
        assertEquals(Waiting.WHITELIST, shown.last())
    }

    private fun relayDownWaitsForTheProbe(answer: Pair<Int, Int>): Int {
        be.blockWhitelist = true
        whitelisted(answer, validated = true)
        assertTrue(relayDowns().isEmpty())
        be.whitelistGate.countDown()
        awaitProbe()
        return relayDowns().size
    }

    @Test
    fun whitelistIsNoRelayDown() = assertEquals(0, relayDownWaitsForTheProbe(2 to 0))

    @Test
    fun serverDownIsRelayDown() = assertEquals(1, relayDownWaitsForTheProbe(2 to 1))

    @Test
    fun unclearIsNoRelayDown() = assertEquals(0, relayDownWaitsForTheProbe(1 to 1))

    // Each no-server fetch looks again: the lists may be lifted while the server is down.
    @Test
    fun theNoServerTickProbesAgain() {
        whitelisted()
        be.whitelistAnswer = 2 to 2
        advance(TunnelVpnService.CONFIG_RETRY_MS)
        awaitProbe()
        assertEquals(2, be.whitelistProbes.size)
        assertEquals(Waiting.NO_SERVER, shown.last())
    }

    // Weak LTE, ya.ru slow once: the white lists stay; only "server down" replaces them.
    @Test
    fun unclearRecheckKeepsTheWhitelist() {
        whitelisted()
        be.whitelistAnswer = 1 to 0
        advance(TunnelVpnService.CONFIG_RETRY_MS)
        awaitProbe()
        assertEquals(2, be.whitelistProbes.size)
        assertEquals(Waiting.WHITELIST, shown.last())
        be.whitelistAnswer = 2 to 1
        advance(2 * TunnelVpnService.CONFIG_RETRY_MS)
        awaitProbe()
        assertEquals(3, be.whitelistProbes.size)
        assertEquals(Waiting.NO_SERVER, shown.last())
    }

    // A tick that falls into the wait after a Wi-Fi appeared beside LTE
    // probes again at once, and the wait's end does not probe a third time.
    @Test
    fun aTickDuringANewNetworksWaitRechecksOnce() {
        whitelisted()
        advance(TunnelVpnService.CONFIG_RETRY_MS - 10_000)
        ReflectionHelpers.getField<ConnectivityManager.NetworkCallback>(svc, "netCallback").onAvailable(ShadowNetwork.newInstance(8))
        ShadowLooper.idleMainLooper()
        be.whitelistAnswer = 2 to 2
        advance(10_000)
        awaitProbe()
        assertEquals(2, be.whitelistProbes.size)
        assertEquals(Waiting.NO_SERVER, shown.last())
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        awaitProbe()
        assertEquals(2, be.whitelistProbes.size)
    }

    private fun episode() {
        verdict(false)
        verdict(true)
        awaitProbe()
    }

    @Test
    fun detectedOnceADayPerCarrier() {
        whitelisted()
        episode()
        assertEquals(listOf(mapOf<String, Any>("op" to "25020")), detected())
        shadowOf(tm).setNetworkOperator("25099")
        episode()
        assertEquals(listOf(mapOf<String, Any>("op" to "25020"), mapOf<String, Any>("op" to "25099")), detected())
        day++
        episode()
        assertEquals(3, detected().size)
    }

    @Test
    fun noCarrierCodeNoEventAndTheDayIsNotSpent() {
        shadowOf(tm).setNetworkOperator("")
        whitelisted()
        assertEquals(Waiting.WHITELIST, shown.last())
        assertTrue(detected().isEmpty())
        shadowOf(tm).setNetworkOperator("25020")
        episode()
        assertEquals(listOf(mapOf<String, Any>("op" to "25020")), detected())
    }

    @Test
    fun emptyListTurnsTheProbeOff() {
        be.config = RcValues("", 0, 0, "", whitelistHosts = """{"allowed":[],"blocked":["github.com"]}""")
        RemoteConfig.apply(app, be.config, be::decryptEndpoints)
        whitelisted(validated = true)
        assertTrue(be.whitelistProbes.isEmpty())
        assertEquals(Waiting.NO_SERVER, shown.last())
        assertEquals("as before the probe", 1, relayDowns().size)
    }

    private val stand = "stand|test.local|10.0.2.2|8443|/app/test"

    @Test
    fun testStandProbesOnlyWithTheExtra() {
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400), test = true)
        start(Intent(TunnelVpnService.ACTION_START).putExtra(TunnelVpnService.EXTRA_ENDPOINT, stand))
        network(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        verdict(true)
        awaitProbe()
        assertTrue(be.whitelistProbes.isEmpty())
    }

    @Test
    fun testStandProbesTheExtrasHosts() {
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400), test = true)
        whitelistedOnTheStand()
        assertEquals(listOf(lte.networkHandle), be.whitelistProbes)
        assertEquals(Waiting.WHITELIST, shown.last())
    }

    private fun whitelistedOnTheStand() {
        start(
            Intent(TunnelVpnService.ACTION_START).putExtra(TunnelVpnService.EXTRA_ENDPOINT, stand)
                .putExtra(TunnelVpnService.EXTRA_WHITELIST_HOSTS, """{"allowed":["10.0.2.2"],"blocked":["192.0.2.1"]}"""),
        )
        network(lte, NetworkCapabilities.TRANSPORT_CELLULAR)
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)
        be.whitelistAnswer = 1 to 0
        verdict(true)
        awaitProbe()
    }
}
