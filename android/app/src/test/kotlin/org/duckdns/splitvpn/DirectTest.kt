package org.duckdns.splitvpn

import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.net.ConnectivityManager
import android.net.LinkProperties
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
import org.robolectric.util.ReflectionHelpers.ClassParameter
import java.util.concurrent.TimeUnit

/** Direct YouTube: the switch, the strategies from Remote Config, the network's key, the verdict. */
@RunWith(RobolectricTestRunner::class)
class DirectTest {

    private lateinit var controller: ServiceController<TunnelVpnService>
    private val app get() = RuntimeEnvironment.getApplication()
    private val svc get() = controller.get()
    private val be = FakeBackend()
    private val vpnPrefs get() = app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)

    @Before
    fun setUp() {
        TunnelState.set(VpnState.DISCONNECTED)
        TunnelState.clearLog()
        ShadowLooper.idleMainLooper()
        AppLog.dir(app).deleteRecursively()
        TunnelVpnService.forgetNetKeyForTests()
        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
        svc.backend = be
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400))
        ShadowLooper.idleMainLooper()
    }

    @After
    fun tearDown() {
        be.release()
        awaitServiceThreads()
        controller.destroy()
        ShadowLooper.idleMainLooper()
    }

    private fun start(intent: Intent? = startIntent()) {
        svc.onStartCommand(intent, 0, 1)
        val deadline = System.currentTimeMillis() + 10_000
        while (TunnelState.state != VpnState.CONNECTED && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, TimeUnit.MILLISECONDS)
        }
        assertEquals(TunnelState.lastError, VpnState.CONNECTED, TunnelState.state)
        ShadowLooper.idleMainLooper()
    }

    private fun startIntent() = Intent(TunnelVpnService.ACTION_START)
        .putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("youtube"))

    private fun logLines() = AppLog.files(app).reversed().flatMap { it.readLines() }

    @Test
    fun onByDefault() {
        start()
        assertEquals(true, be.startedDirect)
        assertTrue(be.startedDesyncFile!!.endsWith("/files/desync.json"))
    }

    @Test
    fun offByTheSwitchAndKeptForRestarts() {
        start(startIntent().putExtra(TunnelVpnService.EXTRA_DIRECT, false))
        assertEquals(false, be.startedDirect)
        assertEquals(false, vpnPrefs.getBoolean(TunnelVpnService.KEY_DIRECT, true))
    }

    // Sticky restart, boot, always-on: what ran last.
    @Test
    fun nullIntentRestartTakesTheSavedOne() {
        vpnPrefs.edit().putStringSet(TunnelVpnService.KEY_SERVICES, setOf("youtube"))
            .putBoolean(TunnelVpnService.KEY_DIRECT, false).commit()
        start(null)
        assertEquals(false, be.startedDirect)
    }

    // Never fetched (fresh install offline, RC blocked): the list built in from rc/config.json.
    @Test
    fun strategiesBuiltInUntilAFetch() {
        start()
        assertEquals(BuildConfig.YT_STRATEGIES, be.startedStrategies)
        assertTrue(BuildConfig.YT_STRATEGIES, BuildConfig.YT_STRATEGIES.startsWith("[{\"id\":\"rec2\""))
    }

    @Test
    fun fetchedStrategiesBeatTheBuiltIn() {
        RemoteConfig.apply(app, RcValues("", 0, 0, "", ytStrategies = """[{"id":"x","spec":"cut=1"}]"""), be::decryptEndpoints)
        start()
        assertEquals("""[{"id":"x","spec":"cut=1"}]""", be.startedStrategies)
    }

    // "[]" is the switch for everyone; no key and garbage keep what was there.
    @Test
    fun whatAFetchLeaves() {
        fun fetch(yt: String?) = RemoteConfig.apply(app, RcValues("", 0, 0, "", ytStrategies = yt), be::decryptEndpoints)
        fetch(null)
        assertEquals(BuildConfig.YT_STRATEGIES, RemoteConfig.ytStrategies(app))
        fetch("[]")
        assertEquals("[]", RemoteConfig.ytStrategies(app))
        fetch("{garbage")
        fetch("{garbage")
        fetch(null)
        assertEquals("[]", RemoteConfig.ytStrategies(app))
        assertEquals(2, logLines().count { " E remote config: yt_strategies is not a JSON array" in it })
    }

    // Go drops the broken entries: the log says which, the dashboard hears once per value.
    @Test
    fun droppedStrategiesAreLogged() {
        be.dropped = "b: bad position \"x\""
        start()
        assertTrue(logLines().any { " E yt_strategies: dropped b: bad position" in it })
        assertTrue(!RemoteConfig.firstDrop(app, RemoteConfig.ytStrategies(app), RemoteConfig.KEY_DROPPED_YT))
    }

    @Test
    fun netKey() {
        assertEquals("cell:25001", TunnelVpnService.netKey(cell = true, wifi = false, operator = "25001", wifiId = null))
        assertEquals("cell:", TunnelVpnService.netKey(cell = true, wifi = false, operator = "", wifiId = null))
        assertEquals("cell:", TunnelVpnService.netKey(cell = true, wifi = false, operator = null, wifiId = null))
        assertEquals("wifi@123", TunnelVpnService.netKey(cell = false, wifi = true, operator = null, wifiId = "123"))
        assertEquals("", TunnelVpnService.netKey(cell = false, wifi = true, operator = null, wifiId = null))
        assertEquals("", TunnelVpnService.netKey(cell = false, wifi = false, operator = null, wifiId = "1"))
    }

    private fun defaultCallback() = ReflectionHelpers.getField<ConnectivityManager.NetworkCallback>(svc, "defaultCallback")

    private fun caps(transport: Int) = ShadowNetworkCapabilities.newInstance().also {
        shadowOf(it).addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
        shadowOf(it).addTransportType(transport)
    }

    private fun lp(dns: String) = LinkProperties().also { p ->
        ReflectionHelpers.callInstanceMethod<Unit>(
            p, "setDnsServers", ClassParameter.from(Collection::class.java, listOf(java.net.InetAddress.getByName(dns))),
        )
    }

    // The key follows the default network; the same one again is not news.
    @Test
    fun netKeyFollowsTheDefaultNetwork() {
        ShadowSubscriptionManager.setDefaultDataSubscriptionId(3)
        val tm = app.getSystemService(TelephonyManager::class.java)
        shadowOf(tm).setTelephonyManagerForSubscriptionId(3, tm)
        start()
        val cb = defaultCallback()
        val wifi = ShadowNetwork.newInstance(7)
        val lte = ShadowNetwork.newInstance(8)

        // Its resolvers come after its capabilities: only then can Go ask for the provider.
        cb.onAvailable(wifi)
        cb.onCapabilitiesChanged(wifi, caps(NetworkCapabilities.TRANSPORT_WIFI))
        cb.onLinkPropertiesChanged(wifi, lp("192.168.1.1"))
        cb.onCapabilitiesChanged(wifi, caps(NetworkCapabilities.TRANSPORT_WIFI))
        assertEquals(listOf("", "wifi@${wifi.networkHandle}"), be.netKeys)

        // The carrier's code may come only with a later callback.
        be.netKeys.clear()
        cb.onAvailable(lte)
        cb.onCapabilitiesChanged(lte, caps(NetworkCapabilities.TRANSPORT_CELLULAR))
        shadowOf(tm).setNetworkOperator("25001")
        cb.onCapabilitiesChanged(lte, caps(NetworkCapabilities.TRANSPORT_CELLULAR))
        cb.onLinkPropertiesChanged(lte, lp("10.0.0.1"))
        assertEquals(listOf("cell:", "cell:25001"), be.netKeys)
    }

    // Go keeps what it learned about a network through a stop and a start
    // (a rebuild every 10 min at most): the same network again is no news,
    // nor are its resolvers still on their way.
    @Test
    fun stopAndStartOnTheSameNetworkIsNoNews() {
        start()
        val wifi = ShadowNetwork.newInstance(7)
        defaultCallback().apply {
            onAvailable(wifi)
            onCapabilitiesChanged(wifi, caps(NetworkCapabilities.TRANSPORT_WIFI))
            onLinkPropertiesChanged(wifi, lp("192.168.1.1"))
        }
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_STOP), 0, 2)
        awaitServiceThreads()
        ShadowLooper.idleMainLooper(4, TimeUnit.SECONDS)
        be.netKeys.clear()
        start()
        defaultCallback().apply {
            onAvailable(wifi)
            onCapabilitiesChanged(wifi, caps(NetworkCapabilities.TRANSPORT_WIFI))
            onLinkPropertiesChanged(wifi, lp("192.168.1.1"))
        }
        assertEquals(emptyList<String>(), be.netKeys)
    }

    // The YouTube card reads Go's verdict from the snapshot.
    @Test
    fun verdictGoesToTheSnapshot() {
        val got = mutableListOf<String?>()
        shadowOf(app).setComponentNameAndServiceForBindService(
            ComponentName(app, TunnelVpnService::class.java), svc.onBind(Intent(TunnelVpnService.ACTION_BIND))!!,
        )
        val client = VpnClient(app) { _, _, _, _, _, _, _, d -> got += d }.also { it.bind() }
        start()
        be.startedHost!!.directVerdict("testing")
        ShadowLooper.idleMainLooper()
        be.startedHost!!.directVerdict("works")
        ShadowLooper.idleMainLooper()
        client.unbind()
        assertEquals("unknown", got.first())
        assertEquals(listOf("testing", "works"), got.distinct().takeLast(2))
    }
}
