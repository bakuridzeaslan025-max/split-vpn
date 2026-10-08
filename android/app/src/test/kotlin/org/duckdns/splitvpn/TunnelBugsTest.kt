package org.duckdns.splitvpn

import android.content.Context
import android.content.Intent
import android.net.VpnService
import android.os.Handler
import android.os.Looper
import android.os.ParcelFileDescriptor
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
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
import org.robolectric.util.ReflectionHelpers
import org.robolectric.util.ReflectionHelpers.ClassParameter
import java.io.File
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.CountDownLatch

/** Regression tests for tunnel lifecycle bugs Б1, Б2, Б4. */
@RunWith(RobolectricTestRunner::class)
class TunnelBugsTest {

    private lateinit var controller: ServiceController<TunnelVpnService>
    private val app get() = RuntimeEnvironment.getApplication()
    private val svc get() = controller.get()
    private val be = FakeBackend()
    private val seen = CopyOnWriteArrayList<VpnState>()
    private val listener: (VpnState, String?) -> Unit = { s, _ -> seen += s }

    @Before
    fun setUp() {
        TunnelState.set(VpnState.DISCONNECTED)
        TunnelState.clearLog()
        ShadowLooper.idleMainLooper()
        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
        svc.backend = be
        // A fresh credential so startTunnel goes straight to establish/start.
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400))
        TunnelState.subscribe(listener)
        ShadowLooper.idleMainLooper()
        seen.clear()
    }

    @After
    fun tearDown() {
        be.release()
        TunnelState.unsubscribe(listener)
        controller.destroy()
        ShadowLooper.idleMainLooper()
    }

    // Only the default network's resolvers answer protect()ed sockets: a
    // carrier's refuses them while Wi-Fi is the default (MIUI keeps both up).
    @Test
    fun dnsOfTheDefaultNetworkOnly() {
        start("telegram")
        awaitState(VpnState.CONNECTED)
        val cb = ReflectionHelpers.getField<android.net.ConnectivityManager.NetworkCallback>(svc, "defaultCallback")
        val wifi = ShadowNetwork.newInstance(7)
        val lte = ShadowNetwork.newInstance(8)
        fun lp(vararg dns: String) = android.net.LinkProperties().also { p ->
            ReflectionHelpers.callInstanceMethod<Unit>(
                p, "setDnsServers", ClassParameter.from(Collection::class.java, dns.map { java.net.InetAddress.getByName(it) })
            )
        }
        be.events.clear()
        cb.onAvailable(wifi)
        cb.onLinkPropertiesChanged(wifi, lp("192.168.88.2"))
        cb.onLinkPropertiesChanged(wifi, lp("192.168.88.2"))
        assertEquals(1, logLines().count { it.contains("default network $wifi dns:") })
        assertEquals("192.168.88.2", be.startedHost!!.dnsServers())
        assertEquals("Go's cache must go with the resolvers", listOf("dnsChanged"), be.events)

        cb.onAvailable(lte)
        cb.onLinkPropertiesChanged(lte, lp("176.59.31.183", "176.59.31.182"))
        // A request's callback hears only AVAILABLE of the new one; a lost
        // of the old one, should it come, does not wipe the new one.
        cb.onLost(wifi)
        assertEquals("176.59.31.183\n176.59.31.182", be.startedHost!!.dnsServers())
        assertEquals(listOf("dnsChanged", "dnsChanged"), be.events)
        cb.onLost(lte)
        assertEquals("", be.startedHost!!.dnsServers())
    }

    @Test
    fun startFailureLogsStackOnce() {
        be.startError = IllegalStateException("relay unreachable")
        start("telegram")
        awaitState(VpnState.ERROR)
        val log = logLines()
        assertTrue(log.toString(), log.any { it.contains("start via ") && it.endsWith(" failed: java.lang.IllegalStateException: relay unreachable") })
        assertEquals(log.toString(), 1, log.count { it.startsWith("java.lang.IllegalStateException") })
    }

    private fun note() = shadowOf(app.getSystemService(android.app.NotificationManager::class.java))
        .getNotification(1)?.extras?.getString(android.app.Notification.EXTRA_TEXT)

    // Not only the service registers one (Firebase may too): tell them all.
    private fun netCallbacks() = shadowOf(app.getSystemService(android.net.ConnectivityManager::class.java)).networkCallbacks.toList()

    // Drop whatever networks Robolectric reports, as in airplane mode.
    private fun loseAllNetworks() {
        @Suppress("DEPRECATION")
        val all = app.getSystemService(android.net.ConnectivityManager::class.java).allNetworks
        for (cb in netCallbacks()) all.forEach(cb::onLost)
    }

    private fun advance(ms: Long) {
        ShadowSystemClock.advanceBy(java.time.Duration.ofMillis(ms))
        ShadowLooper.idleMainLooper()
    }

    // No relay: CONNECTED stays, the UI and the notification say why and
    // that nothing needs doing; the uptime survives the outage.
    @Test
    fun relayDownShowsWaitingThenClears() {
        start("telegram")
        awaitState(VpnState.CONNECTED)
        loseAllNetworks()
        be.startedHost!!.relayDown(true)
        ShadowLooper.idleMainLooper()
        assertNull("the network may be back in a moment", TunnelState.waiting)
        advance(5_000)
        assertEquals(Waiting.NO_NETWORK, TunnelState.waiting)
        assertEquals("Нет сети · подключится автоматически", note())

        netCallbacks().forEach { it.onAvailable(ShadowNetwork.newInstance(7)) }
        ShadowLooper.idleMainLooper()
        assertEquals("the prober is still trying the new network", Waiting.NO_NETWORK, TunnelState.waiting)
        advance(10_000)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        assertEquals("Сервер недоступен · подключится автоматически", note())

        val before = TunnelState.connectedAt
        advance(5_000)
        be.startedHost!!.relayDown(false)
        ShadowLooper.idleMainLooper()
        assertNull(TunnelState.waiting)
        assertEquals(VpnState.CONNECTED, TunnelState.state)
        assertEquals("VPN включён", note())
        assertEquals("a blip must not reset the uptime", before, TunnelState.connectedAt)

        be.startedHost!!.relayDown(true)
        stop()
        ShadowLooper.idleMainLooper()
        assertNull("waiting outlived the tunnel", TunnelState.waiting)
    }

    // The main case: VPN switched on with no server. Start says RelayDown
    // before CONNECTED; the warning must still show.
    @Test
    fun startWithoutRelayShowsWaiting() {
        be.relayDownAtStart = true
        start("telegram")
        awaitState(VpnState.CONNECTED)
        netCallbacks().forEach { it.onAvailable(ShadowNetwork.newInstance(7)) }
        ShadowLooper.idleMainLooper()
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
    }

    // Android says there is no network at all: warn after a short grace,
    // even before the relay's own verdict.
    @Test
    fun lastNetworkLostWarnsAfterGrace() {
        start("telegram")
        awaitState(VpnState.CONNECTED)
        loseAllNetworks()
        ShadowLooper.idleMainLooper()
        assertNull("a switch between networks is not worth a warning", TunnelState.waiting)
        advance(5_000)
        assertEquals(Waiting.NO_NETWORK, TunnelState.waiting)
    }

    private fun logLines() = AppLog.files(app).reversed().flatMap { it.readLines() }

    private fun vpnPrefs() = app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)

    private fun start(vararg services: String) {
        val i = Intent(TunnelVpnService.ACTION_START)
        if (services.isNotEmpty()) i.putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf(*services))
        svc.onStartCommand(i, 0, 1)
    }

    private fun stop() = svc.onStartCommand(Intent(TunnelVpnService.ACTION_STOP), 0, 2)

    private fun awaitState(want: VpnState) {
        val deadline = System.currentTimeMillis() + 10_000
        while (TunnelState.state != want && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, java.util.concurrent.TimeUnit.MILLISECONDS)
        }
        assertEquals(TunnelState.lastError ?: "", want, TunnelState.state)
    }

    // Б1: STOP while Tunnel.start is still running must not leave Go running
    // and must not flash CONNECTED after DISCONNECTING.
    @Test
    fun stopDuringConnectStopsGoAfterStartReturns() {
        be.blockStart = true
        start("telegram")
        awaitState(VpnState.CONNECTING)
        stop()
        assertEquals(VpnState.DISCONNECTING, TunnelState.state)
        Thread.sleep(200)
        be.startGate.countDown()
        awaitState(VpnState.DISCONNECTED)
        Thread.sleep(300)
        ShadowLooper.idleMainLooper()
        assertEquals(listOf("start", "stop"), be.events)
        assertFalse("CONNECTED seen: $seen", VpnState.CONNECTED in seen)
        assertFalse("ERROR seen: $seen", VpnState.ERROR in seen)
        assertEquals(VpnState.DISCONNECTED, TunnelState.state)
    }

    // Б1: probes must not outlive the tunnel or delay stop.
    @Test
    fun stopDuringProbesIsFastAndDropsLateLog() {
        be.blockProbe = true
        start("telegram")
        awaitState(VpnState.CONNECTED)
        val t0 = System.currentTimeMillis()
        stop()
        awaitState(VpnState.DISCONNECTED)
        assertTrue("stop took ${System.currentTimeMillis() - t0} ms", System.currentTimeMillis() - t0 < 2000)
        be.probeGate.countDown()
        Thread.sleep(300)
        assertTrue(TunnelState.logLines().toString(), TunnelState.logLines().none { it.contains("Telegram:") })
    }

    // Б2: failed Tunnel.start closes the TUN fd and a fresh process does not
    // retry immediately.
    @Test
    fun startFailureClosesTunAndBacksOffResume() {
        var closed = false
        be.pfd = ParcelFileDescriptor.open(
            File.createTempFile("tun", null), ParcelFileDescriptor.MODE_READ_ONLY, Handler(Looper.getMainLooper())
        ) { closed = true }
        be.startError = IllegalStateException("relay unreachable")
        start("telegram")
        awaitState(VpnState.ERROR)
        ShadowLooper.idleMainLooper()
        assertTrue("tun fd not closed", closed)
        assertTrue(vpnPrefs().getBoolean(TunnelVpnService.KEY_WANTED, false))

        controller.destroy()
        TunnelState.set(VpnState.DISCONNECTED)
        ShadowLooper.idleMainLooper()
        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
        assertNull("resumed right after a failure", shadowOf(app).nextStartedService)
    }

    // START while the stop thread is still waiting for Tunnel.start must not
    // spawn a second GoTunnel that the stop then kills from underneath.
    @Test
    fun startDuringDisconnectingIsRejected() {
        be.blockStart = true
        start("telegram")
        awaitState(VpnState.CONNECTING)
        stop()
        assertEquals(VpnState.DISCONNECTING, TunnelState.state)
        start("telegram")
        assertEquals(VpnState.DISCONNECTING, TunnelState.state)
        be.startGate.countDown()
        awaitState(VpnState.DISCONNECTED)
        Thread.sleep(300)
        assertEquals(listOf("start", "stop"), be.events)
        assertEquals(VpnState.DISCONNECTED, TunnelState.state)
    }

    // Tunnel.start failing after STOP already began is not an error to show.
    @Test
    fun startFailureDuringDisconnectingStaysQuiet() {
        be.blockStart = true
        be.startError = IllegalStateException("relay unreachable")
        start("telegram")
        awaitState(VpnState.CONNECTING)
        stop()
        be.startGate.countDown()
        awaitState(VpnState.DISCONNECTED)
        Thread.sleep(300)
        ShadowLooper.idleMainLooper()
        assertFalse("ERROR seen: $seen", VpnState.ERROR in seen)
        assertEquals(0L, vpnPrefs().getLong(TunnelVpnService.KEY_FAILED_AT, 0L))
    }

    // An empty set must not be persisted, or the null-intent restart fails too.
    @Test
    fun emptyServiceSetIsNotPersisted() {
        be.blockStart = true
        start("telegram")
        awaitState(VpnState.CONNECTING)
        be.startGate.countDown()
        awaitState(VpnState.CONNECTED)
        stop()
        awaitState(VpnState.DISCONNECTED)
        svc.onStartCommand(
            Intent(TunnelVpnService.ACTION_START).putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf()), 0, 3
        )
        awaitState(VpnState.ERROR)
        assertEquals(setOf("telegram"), vpnPrefs().getStringSet(TunnelVpnService.KEY_SERVICES, null))
    }

    // Underlying network gone: relay conns must be cut, not left to time out.
    @Test
    fun networkLostCutsConnections() {
        start("telegram")
        awaitState(VpnState.CONNECTED)
        val cb = ReflectionHelpers.getField<android.net.ConnectivityManager.NetworkCallback>(svc, "netCallback")
        cb.onLost(ShadowNetwork.newInstance(7))
        assertEquals(listOf("start", "networkLost"), be.events)
        cb.onAvailable(ShadowNetwork.newInstance(8))
        assertEquals(listOf("start", "networkLost", "networkChanged"), be.events)
    }

    // Б4: a second START while running must not change the persisted set;
    // prefs describe the routes actually installed.
    @Test
    fun startWhileRunningKeepsPersistedServices() {
        be.blockStart = true
        start("telegram")
        awaitState(VpnState.CONNECTING)
        start("telegram", "youtube")
        assertEquals(setOf("telegram"), vpnPrefs().getStringSet(TunnelVpnService.KEY_SERVICES, null))
    }

    // After backend.stop() returns, state must stay DISCONNECTING until either
    // onDestroy fires (system killed the service) or the 3s fallback. The UI
    // stays in sync with the system VPN icon that lingers on some ROMs.
    @Test
    fun stateStaysDisconnectingUntilFallback() {
        start("telegram")
        awaitState(VpnState.CONNECTED)
        stop()
        while ("stop" !in be.events && System.currentTimeMillis() < System.currentTimeMillis() + 5_000) Thread.sleep(20)
        assertEquals(VpnState.DISCONNECTING, TunnelState.state)
        // Fallback fires 3 s after stopSelf; advance the main looper past it.
        ShadowLooper.idleMainLooper(4, java.util.concurrent.TimeUnit.SECONDS)
        assertEquals(VpnState.DISCONNECTED, TunnelState.state)
    }

    // Race: after backend.stop() returns but before DISCONNECTED is set, a new
    // START must still be rejected (stopping flag must not leak clear).
    @Test
    fun startInPostStopWindowIsRejected() {
        start("telegram")
        awaitState(VpnState.CONNECTED)
        stop()
        val deadline = System.currentTimeMillis() + 5_000
        while ("stop" !in be.events && System.currentTimeMillis() < deadline) Thread.sleep(20)
        assertEquals(VpnState.DISCONNECTING, TunnelState.state)
        start("telegram")
        // No second "start" event: startTunnel saw stopping=true and bailed.
        assertEquals(listOf("start", "stop"), be.events)
        assertEquals(VpnState.DISCONNECTING, TunnelState.state)
        ShadowLooper.idleMainLooper(4, java.util.concurrent.TimeUnit.SECONDS)
        assertEquals(VpnState.DISCONNECTED, TunnelState.state)
    }

    // The user unblocked the channel or the app: the dropped notification
    // comes back while the service is in the foreground, never after STOP.
    @Test
    fun unblockedChannelRepostsOnlyWhileRunning() {
        val nm = app.getSystemService(android.app.NotificationManager::class.java)
        fun unblock(action: String, channel: String? = null) {
            val i = Intent(action).putExtra(android.app.NotificationManager.EXTRA_BLOCKED_STATE, false)
            channel?.let { i.putExtra(android.app.NotificationManager.EXTRA_NOTIFICATION_CHANNEL_ID, it) }
            app.sendBroadcast(i)
            ShadowLooper.idleMainLooper()
        }
        val channelChanged = android.app.NotificationManager.ACTION_NOTIFICATION_CHANNEL_BLOCK_STATE_CHANGED
        start("telegram")
        awaitState(VpnState.CONNECTED)
        nm.cancel(1)
        unblock(channelChanged, "quota_channel")
        assertNull(note())
        unblock(channelChanged, "vpn_channel")
        assertEquals("VPN включён", note())
        nm.cancel(1)
        unblock(android.app.NotificationManager.ACTION_APP_BLOCK_STATE_CHANGED)
        assertEquals("VPN включён", note())

        stop()
        awaitState(VpnState.DISCONNECTED)
        nm.cancel(1)
        unblock(channelChanged, "vpn_channel")
        assertNull("orphan after stop", note())
    }
}
