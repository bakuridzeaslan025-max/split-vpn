package org.duckdns.splitvpn

import android.content.Context
import android.content.Intent
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.NetworkCapabilities
import android.os.Looper
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
import org.robolectric.shadows.ShadowNetworkCapabilities
import org.robolectric.shadows.ShadowSystemClock
import org.robolectric.util.ReflectionHelpers
import java.net.InetAddress
import java.util.concurrent.TimeUnit

/** When the VK call channel goes on and off, and what Go hears about it. */
@RunWith(RobolectricTestRunner::class)
class FallbackTest {

    private lateinit var controller: ServiceController<TunnelVpnService>
    private val app get() = RuntimeEnvironment.getApplication()
    private val svc get() = controller.get()
    private val be = FakeBackend()
    private val prefs get() = app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)
    private val room = "https://vk.ru/call/join/testroom"
    private val key = "ab".repeat(32)
    private val a = TunnelVpnService.Endpoint("a.example.org", "203.0.113.10", 443, "/app/aaaa1")
    private val b = TunnelVpnService.Endpoint("b.example.net", "198.51.100.7", 8443, "/app/bbbb2")

    @Before
    fun setUp() {
        TunnelState.set(VpnState.DISCONNECTED)
        TunnelState.clearLog()
        ShadowLooper.idleMainLooper()
        prefs.edit().clear().commit()
        Endpoints.resetForTests()
        TunnelVpnService.forgetFallbackForTests()
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
        TunnelVpnService.drainFallbackForTests()
        ShadowLooper.idleMainLooper()
        Endpoints.resetForTests()
    }

    private fun intent(force: Boolean = true, room: String? = this.room, key: String? = this.key, analytics: Boolean = false) =
        Intent(TunnelVpnService.ACTION_START)
            .putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram"))
            .putExtra(TunnelVpnService.EXTRA_FALLBACK_FORCE, force)
            .putExtra(TunnelVpnService.EXTRA_ANALYTICS, analytics)
            .apply {
                room?.let { putExtra(TunnelVpnService.EXTRA_FALLBACK_ROOM, it) }
                key?.let { putExtra(TunnelVpnService.EXTRA_FALLBACK_KEY, it) }
            }

    private fun start(i: Intent = intent()) {
        svc.onStartCommand(i, 0, 1)
        val deadline = System.currentTimeMillis() + 10_000
        while ((TunnelState.state != VpnState.CONNECTED || netCallback() == null) && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, TimeUnit.MILLISECONDS)
        }
        assertEquals(TunnelState.lastError ?: "", VpnState.CONNECTED, TunnelState.state)
        ShadowLooper.idleMainLooper()
        awaitFetch()
    }

    // The start's own fetch must not land in the middle of a test.
    private fun awaitFetch() {
        val deadline = System.currentTimeMillis() + 5_000
        while (be.fetches.get() == 0 && System.currentTimeMillis() < deadline) Thread.sleep(10)
        awaitServiceThreads()
        ShadowLooper.idleMainLooper()
    }

    private fun stop() {
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_STOP), 0, 2)
        awaitServiceThreads()
        ShadowLooper.idleMainLooper(4, TimeUnit.SECONDS)
    }

    private fun netCallback() = ReflectionHelpers.getField<ConnectivityManager.NetworkCallback?>(svc, "netCallback")
    private fun defaultCallback() = ReflectionHelpers.getField<ConnectivityManager.NetworkCallback>(svc, "defaultCallback")
    private fun net(id: Int) = ShadowNetwork.newInstance(id)

    /** The network as the two callbacks hear it, up and validated unless told otherwise. */
    private fun online(id: Int = 7, validated: Boolean = true) {
        netCallback()!!.onAvailable(net(id))
        defaultCallback().onAvailable(net(id))
        ShadowLooper.idleMainLooper()
        if (validated) validated(id)
    }

    private fun validated(id: Int = 7, yes: Boolean = true) {
        val caps = ShadowNetworkCapabilities.newInstance()
        shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
        if (yes) shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
        defaultCallback().onCapabilitiesChanged(net(id), caps)
        ShadowLooper.idleMainLooper()
    }

    private fun dns(id: Int, vararg servers: String) {
        val lp = LinkProperties()
        lp.setDnsServers(servers.map { InetAddress.getByName(it) })
        defaultCallback().onLinkPropertiesChanged(net(id), lp)
        ShadowLooper.idleMainLooper()
    }

    private fun verdict(down: Boolean) {
        be.startedHost!!.relayDown(down)
        ShadowLooper.idleMainLooper()
    }

    private fun advance(ms: Long) {
        ShadowSystemClock.advanceBy(java.time.Duration.ofMillis(ms))
        ShadowLooper.idleMainLooper()
    }

    private fun settle() = advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS)

    private fun calls(): List<String> {
        TunnelVpnService.drainFallbackForTests()
        ShadowLooper.idleMainLooper()
        return be.events.filter { it.startsWith("fallback") }
    }

    private fun enabledCount() = calls().count { it == "fallbackEnable" }

    private fun channelOn() {
        start()
        online()
        settle()
        verdict(true)
        assertEquals(1, enabledCount())
    }

    // ---- on

    @Test
    fun verdictEnablesWithASingleEndpoint() {
        be.endpoints = listOf(a)
        start()
        online()
        settle()
        dns(7, "192.0.2.53", "192.0.2.54")
        assertEquals(emptyList<String>(), calls())
        verdict(true)
        assertEquals(listOf("fallbackConfigure", "fallbackSetStateDir", "fallbackSetMaxJoins:0", "fallbackSetDns:192.0.2.53 192.0.2.54", "fallbackEnable"), calls())
        assertEquals(room to key, be.fallbackRoom)
        assertEquals(Fallback.READY, TunnelState.fallback)
    }

    // With a longer list the round goes on beside it.
    @Test
    fun verdictEnablesWithSeveralEndpoints() {
        be.endpoints = listOf(a, b)
        start()
        online()
        settle()
        verdict(true)
        assertEquals(1, enabledCount())
        assertEquals(listOf(b), be.endpointsSet)
    }

    // Go's probe at start says down before CONNECTED, and with one endpoint it never says again.
    @Test
    fun relayDownAtStartEnablesAfterThePause() {
        be.endpoints = listOf(a)
        be.relayDownAtStart = true
        start()
        online()
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        assertEquals(0, enabledCount())
        settle()
        assertEquals(1, enabledCount())
    }

    @Test
    fun notInsideThePauseThenRecheckedAfterIt() {
        start()
        online()
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS - 10_000)
        verdict(true)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        assertEquals(0, enabledCount())
        ShadowLooper.idleMainLooper(9_999, TimeUnit.MILLISECONDS)
        assertEquals(0, enabledCount())
        ShadowLooper.idleMainLooper(1, TimeUnit.MILLISECONDS)
        assertEquals(1, enabledCount())
    }

    @Test
    fun recheckAfterThePauseFindsTheRelayBack() {
        start()
        online()
        verdict(true)
        verdict(false)
        settle()
        assertEquals(0, enabledCount())
    }

    @Test
    fun aNewDefaultNetworkInsideThePauseStartsItOver() {
        start()
        online()
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS - 10_000)
        verdict(true)
        advance(5_000)
        defaultCallback().onAvailable(net(8))
        validated(8)
        ShadowLooper.idleMainLooper(5_000, TimeUnit.MILLISECONDS)
        assertEquals(0, enabledCount())
        ShadowLooper.idleMainLooper(TunnelVpnService.NEW_NETWORK_SWITCH_MS, TimeUnit.MILLISECONDS)
        assertEquals(1, enabledCount())
    }

    // Allow lists fail Android's check too: that is the main case for the channel.
    @Test
    fun aNetworkThatIsNotValidatedStillEnables() {
        start()
        online(validated = false)
        validated(yes = false)
        settle()
        verdict(true)
        assertEquals(1, enabledCount())
    }

    @Test
    fun anUnknownDefaultNetworkStillEnables() {
        start()
        netCallback()!!.onAvailable(net(7))
        ShadowLooper.idleMainLooper()
        settle()
        verdict(true)
        assertEquals(1, enabledCount())
    }

    private fun portal(id: Int = 7, yes: Boolean = true) {
        val caps = ShadowNetworkCapabilities.newInstance()
        shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
        if (yes) shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_CAPTIVE_PORTAL)
        defaultCallback().onCapabilitiesChanged(net(id), caps)
        ShadowLooper.idleMainLooper()
    }

    @Test
    fun notAtACaptivePortalThenOnceItClears() {
        start()
        online(validated = false)
        portal()
        settle()
        verdict(true)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        portal()
        assertEquals(0, enabledCount())
        // Signed in, Android has not validated it yet.
        portal(yes = false)
        assertEquals(1, enabledCount())
    }

    // The recheck at the pause's end finds the portal, and still clears with it.
    @Test
    fun aPortalFoundAfterThePauseClearsToo() {
        start()
        online(validated = false)
        portal()
        verdict(true)
        settle()
        assertEquals(0, enabledCount())
        portal(yes = false)
        assertEquals(1, enabledCount())
    }

    @Test
    fun movingOffAPortalToAnotherDefaultNetworkEnablesPastItsPause() {
        start()
        online(validated = false)
        portal()
        settle()
        verdict(true)
        assertEquals(0, enabledCount())
        defaultCallback().onLost(net(7))
        defaultCallback().onAvailable(net(8))
        portal(8, yes = false)
        assertEquals(0, enabledCount())
        ShadowLooper.idleMainLooper(TunnelVpnService.NEW_NETWORK_SWITCH_MS, TimeUnit.MILLISECONDS)
        assertEquals(1, enabledCount())
    }

    // NO_SERVER outlives a network gone for a moment: not while none is left.
    @Test
    fun notWithNoNetworkLeftThenWhenOneAppears() {
        start()
        online()
        advance(TunnelVpnService.NEW_NETWORK_SWITCH_MS - 1_000)
        verdict(true)
        netCallback()!!.onLost(net(7))
        ShadowLooper.idleMainLooper(1_000, TimeUnit.MILLISECONDS)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        assertEquals(0, enabledCount())
        netCallback()!!.onAvailable(net(8))
        ShadowLooper.idleMainLooper()
        assertEquals(0, enabledCount())
        ShadowLooper.idleMainLooper(TunnelVpnService.NEW_NETWORK_SWITCH_MS, TimeUnit.MILLISECONDS)
        assertEquals(1, enabledCount())
    }

    @Test
    fun notWithTheFlagOff() {
        start(intent(force = false))
        online()
        settle()
        verdict(true)
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        assertEquals(emptyList<String>(), calls())
        assertNull(TunnelState.fallback)
    }

    @Test
    fun withoutARoomSilentlyNothing() {
        start(intent(room = null, key = null))
        online()
        settle()
        verdict(true)
        assertEquals(emptyList<String>(), calls())
        assertNull(TunnelState.fallback)
        assertFalse(TunnelState.logLines().any { "запасной" in it.lowercase() })
    }

    @Test
    fun anEmptyExtraTakesTheRoomBack() {
        start(intent(room = ""))
        online()
        settle()
        verdict(true)
        assertEquals(emptyList<String>(), calls())
    }

    // The START of a tap or a boot has no extras: what was given stays.
    @Test
    fun theRoomOutlivesTheStartThatBroughtIt() {
        start()
        stop()
        be.fetches.set(0)
        start(Intent(TunnelVpnService.ACTION_START).putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram")))
        online()
        settle()
        verdict(true)
        assertEquals(1, enabledCount())
    }

    @Test
    fun idempotent() {
        be.endpoints = listOf(a, b)
        channelOn()
        verdict(true)
        verdict(true)
        validated(yes = false)
        validated()
        assertEquals(1, enabledCount())
    }

    @Test
    fun enableFailureLeavesItOffAndTheNextVerdictTries() {
        be.fallbackEnableError = Exception("vkfallback: not configured")
        start()
        online()
        settle()
        verdict(true)
        assertEquals(1, enabledCount())
        assertNull(TunnelState.fallback)
        be.fallbackEnableError = null
        be.startedHost!!.relayDown(true)
        ShadowLooper.idleMainLooper()
        assertEquals(2, enabledCount())
        assertEquals(Fallback.READY, TunnelState.fallback)
    }

    // The START this comes with clears the log: the line has to land after it.
    @Test
    fun aRoomFromAnIntentShowsInTheJournalWithoutTheLink() {
        start()
        assertEquals(1, TunnelState.logLines().count { "Комната задана" in it })
        assertFalse(TunnelState.logLines().any { "testroom" in it })
    }

    @Test
    fun noJournalLineWithoutARoomInTheIntent() {
        start(intent(room = null, key = null))
        assertFalse(TunnelState.logLines().any { "Комната" in it })
    }

    // ---- off

    @Test
    fun relayUpDisablesOffTheMainThread() {
        channelOn()
        verdict(false)
        assertEquals("fallbackDisable:relay_back", calls().last())
        assertNull(TunnelState.fallback)
        be.fallbackThreads.forEach { assertTrue(it !== Looper.getMainLooper().thread) }
    }

    @Test
    fun theNextOutageEnablesAgain() {
        channelOn()
        verdict(false)
        verdict(true)
        assertEquals(2, enabledCount())
    }

    @Test
    fun stopDisablesAfterTunnelStop() {
        channelOn()
        stop()
        val ev = be.events.toList()
        assertTrue(ev.toString(), ev.indexOf("stop") in 0 until ev.indexOf("fallbackDisable:user"))
        be.fallbackThreads.forEach { assertTrue(it !== Looper.getMainLooper().thread) }
    }

    @Test
    fun quotaStopHasItsOwnReason() {
        channelOn()
        be.used = Quota.limit(app) + 1
        be.startedHost!!.quotaExceeded()
        ShadowLooper.idleMainLooper()
        val deadline = System.currentTimeMillis() + 10_000
        while (TunnelState.state != VpnState.ERROR && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, TimeUnit.MILLISECONDS)
        }
        awaitServiceThreads()
        assertEquals("fallbackDisable:quota", calls().last())
    }

    @Test
    fun maxJoinsReachesGoBeforeEnable() {
        start(intent().putExtra(TunnelVpnService.EXTRA_FALLBACK_MAX_JOINS, 3))
        online()
        settle()
        verdict(true)
        val c = calls()
        assertTrue(c.toString(), c.indexOf("fallbackSetMaxJoins:3") in 0 until c.indexOf("fallbackEnable"))
    }

    @Test
    fun stopWithNoChannelOnDoesNotTouchIt() {
        start()
        online()
        stop()
        assertEquals(emptyList<String>(), calls())
    }

    private fun twitterOn() {
        start(intent().putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("twitter")))
        online()
        settle()
        verdict(true)
        assertEquals(1, enabledCount())
    }

    private fun rebuild() {
        RouteCache.file(app).writeText("x.com 192.0.2.0 ${System.currentTimeMillis() / 1000}\n")
        be.startedHost!!.routesStale()
        ShadowLooper.idleMainLooper()
    }

    // Apps reconnect into the same call; a late dial here is no waste, the tunnel goes on.
    @Test
    fun aRebuildKeepsTheChannel() {
        RouteCache.file(app).delete()
        twitterOn()
        rebuild()
        val deadline = System.currentTimeMillis() + 10_000
        while (be.events.count { it == "start" } < 2 && System.currentTimeMillis() < deadline) Thread.sleep(20)
        awaitServiceThreads()
        ShadowLooper.idleMainLooper()
        RouteCache.file(app).delete()
        assertEquals(2, be.events.count { it == "start" })
        assertEquals(0, calls().count { it.startsWith("fallbackDisable") })
        assertEquals(Fallback.READY, TunnelState.fallback)
        // The new stack's own verdict.
        verdict(true)
        assertEquals(1, enabledCount())
    }

    @Test
    fun aFailedRebuildDisablesAfterTunnelStop() {
        RouteCache.file(app).delete()
        twitterOn()
        be.startError = Exception("relay unreachable")
        rebuild()
        val deadline = System.currentTimeMillis() + 10_000
        while (TunnelState.state != VpnState.ERROR && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, TimeUnit.MILLISECONDS)
        }
        RouteCache.file(app).delete()
        assertEquals(VpnState.ERROR, TunnelState.state)
        assertEquals("fallbackDisable:error", calls().last())
        val ev = be.events.toList()
        assertTrue(ev.toString(), ev.indexOf("stop") in 0 until ev.indexOf("fallbackDisable:error"))
        be.fallbackThreads.forEach { assertTrue(it !== Looper.getMainLooper().thread) }
    }

    // A stop after a START with no tunnel up: nothing to leave.
    @Test
    fun aFailedStartDoesNotTouchIt() {
        be.startError = Exception("relay unreachable")
        svc.onStartCommand(intent(), 0, 1)
        val deadline = System.currentTimeMillis() + 10_000
        while (TunnelState.state != VpnState.ERROR && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, TimeUnit.MILLISECONDS)
        }
        assertEquals(VpnState.ERROR, TunnelState.state)
        assertEquals(emptyList<String>(), calls())
    }

    // ---- the flag

    @Test
    fun flagFromAFetchEnablesWithoutRestart() {
        start(intent(force = false))
        online()
        settle()
        verdict(true)
        assertEquals(0, enabledCount())
        be.config = RcValues("", 0, 0, "", vkFallback = true)
        fetchAgain()
        assertEquals(1, enabledCount())
        assertTrue(RemoteConfig.vkFallback(app))
    }

    @Test
    fun flagOffFromAFetchDisables() {
        be.config = RcValues("", 0, 0, "", vkFallback = true)
        start(intent(force = false))
        online()
        settle()
        verdict(true)
        assertEquals(1, enabledCount())
        be.config = RcValues("", 0, 0, "", vkFallback = false)
        fetchAgain()
        assertEquals("fallbackDisable:flag_off", calls().last())
        assertNull(TunnelState.fallback)
    }

    @Test
    fun aFailedFetchKeepsTheLastFlag() {
        be.config = RcValues("", 0, 0, "", vkFallback = true)
        start(intent(force = false))
        be.configError = Exception("offline")
        fetchAgain()
        assertTrue(RemoteConfig.vkFallback(app))
    }

    @Test
    fun theFlagParses() {
        assertTrue(RemoteConfig.parseVkFallback("true"))
        assertTrue(RemoteConfig.parseVkFallback(" true\n"))
        for (raw in listOf(null, "", "false", "1", "TRUE", "yes")) assertFalse(raw.toString(), RemoteConfig.parseVkFallback(raw))
    }

    @Test
    fun forcedBackOffDisables() {
        channelOn()
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START).putExtra(TunnelVpnService.EXTRA_FALLBACK_FORCE, false), 0, 3)
        ShadowLooper.idleMainLooper()
        assertEquals("fallbackDisable:flag_off", calls().last())
    }

    // The next periodic fetch: its handler posts the verdict back.
    private fun fetchAgain() {
        val n = be.fetches.get()
        ShadowLooper.idleMainLooper(TunnelVpnService.CONFIG_PERIOD_MS, TimeUnit.MILLISECONDS)
        val deadline = System.currentTimeMillis() + 5_000
        while (be.fetches.get() == n && System.currentTimeMillis() < deadline) {
            Thread.sleep(10)
            ShadowLooper.idleMainLooper()
        }
        awaitServiceThreads()
        ShadowLooper.idleMainLooper()
    }

    // ---- the network, only as the default one changes

    @Test
    fun aNetworkLostBesideAnotherIsNotForwarded() {
        channelOn()
        netCallback()!!.onAvailable(net(8))
        ShadowLooper.idleMainLooper()
        netCallback()!!.onLost(net(8))
        ShadowLooper.idleMainLooper()
        assertEquals(0, calls().count { it == "fallbackNetworkLost" || it == "fallbackNetworkChanged" })
    }

    @Test
    fun theDefaultNetworkChangingIsForwardedOnce() {
        channelOn()
        defaultCallback().onAvailable(net(7))
        ShadowLooper.idleMainLooper()
        assertEquals(0, calls().count { it == "fallbackNetworkChanged" })
        defaultCallback().onAvailable(net(8))
        defaultCallback().onAvailable(net(8))
        ShadowLooper.idleMainLooper()
        assertEquals(1, calls().count { it == "fallbackNetworkChanged" })
    }

    @Test
    fun noNetworkLeftIsLostThenTheNextIsAChange() {
        channelOn()
        netCallback()!!.onLost(net(7))
        defaultCallback().onLost(net(7))
        ShadowLooper.idleMainLooper()
        assertEquals("fallbackNetworkLost", calls().last())
        netCallback()!!.onAvailable(net(7))
        defaultCallback().onAvailable(net(7))
        ShadowLooper.idleMainLooper()
        assertEquals("fallbackNetworkChanged", calls().last())
    }

    @Test
    fun nothingIsForwardedWhileOff() {
        start()
        online()
        defaultCallback().onAvailable(net(8))
        netCallback()!!.onLost(net(7))
        netCallback()!!.onLost(net(8))
        dns(8, "192.0.2.53")
        assertEquals(emptyList<String>(), calls())
    }

    // Go heard the network go, then was disabled, and nobody told it the network came back.
    @Test
    fun enableClearsALostGoStillBelieves() {
        channelOn()
        netCallback()!!.onLost(net(7))
        defaultCallback().onLost(net(7))
        ShadowLooper.idleMainLooper()
        verdict(false)
        calls()
        be.events.clear()
        online()
        settle()
        verdict(true)
        assertEquals(listOf("fallbackConfigure", "fallbackSetStateDir", "fallbackSetMaxJoins:0", "fallbackSetDns:", "fallbackNetworkChanged", "fallbackEnable"), calls())
    }

    @Test
    fun dnsChangeIsForwardedWhileOnBeforeAndAfterEnableInOrder() {
        start()
        online()
        dns(7, "192.0.2.53")
        settle()
        verdict(true)
        dns(7, "192.0.2.53")
        dns(7, "192.0.2.54", "2001:db8::1")
        assertEquals(
            listOf("fallbackConfigure", "fallbackSetStateDir", "fallbackSetMaxJoins:0", "fallbackSetDns:192.0.2.53", "fallbackEnable", "fallbackSetDns:192.0.2.54 ${InetAddress.getByName("2001:db8::1").hostAddress}"),
            calls(),
        )
    }

    // ---- state and events

    private fun go(name: String, result: String = "", step: String = "", ms: Long = 0, mb: Long = 0) {
        be.fallbackHost!!.event(name, result, step, ms, mb)
        ShadowLooper.idleMainLooper()
    }

    @Test
    fun goEventsDriveTheState() {
        channelOn()
        calls()
        assertEquals(Fallback.READY, TunnelState.fallback)
        go("fallback_connect", "ok", ms = 900)
        assertEquals(Fallback.WORKING, TunnelState.fallback)
        go("fallback_off", "ok", "idle", 1, 1)
        assertEquals(Fallback.READY, TunnelState.fallback)
        go("fallback_connect", "fail", "captcha", 900)
        assertEquals(Fallback.FAILED, TunnelState.fallback)
        go("fallback_connect", "ok", ms = 900)
        go("fallback_off", "fail", "error", 1, 1)
        assertEquals(Fallback.FAILED, TunnelState.fallback)
        go("fallback_connect", "ok", ms = 900)
        // A network change means a new call: not working until Go says so.
        defaultCallback().onAvailable(net(8))
        ShadowLooper.idleMainLooper()
        assertEquals(Fallback.READY, TunnelState.fallback)
        verdict(false)
        calls()
        // Late, from a run that was stopping: the channel stays off.
        go("fallback_connect", "ok", ms = 900)
        assertNull(TunnelState.fallback)
    }

    private fun notificationText(): String? {
        val nm = app.getSystemService(android.app.NotificationManager::class.java)
        return shadowOf(shadowOf(nm).allNotifications.last()).contentText?.toString()
    }

    @Test
    fun theNotificationSaysItWhileWorking() {
        channelOn()
        calls()
        go("fallback_connect", "ok", ms = 900)
        assertEquals("Запасной канал · батарея садится быстрее", notificationText())
        go("fallback_off", "ok", "idle")
        assertEquals("Сервер недоступен · подключится автоматически", notificationText())
    }

    @Test
    fun fallbackOnCarriesHowLongTheRelayWasDown() {
        start(intent(analytics = true))
        val caps = ShadowNetworkCapabilities.newInstance()
        shadowOf(caps).addTransportType(NetworkCapabilities.TRANSPORT_WIFI)
        shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
        netCallback()!!.onAvailable(net(7))
        defaultCallback().onAvailable(net(7))
        defaultCallback().onCapabilitiesChanged(net(7), caps)
        ShadowLooper.idleMainLooper()
        // Down inside the pause, on when it ends.
        verdict(true)
        settle()
        calls()
        val on = be.analytics.filter { it.first == "fallback_on" }.map { it.second }
        assertEquals(listOf(mapOf<String, Any>("net" to "wifi", "ms" to TunnelVpnService.NEW_NETWORK_SWITCH_MS)), on)
    }

    @Test
    fun noEventsWithoutCollection() {
        channelOn()
        calls()
        go("fallback_connect", "ok", ms = 900)
        assertEquals(emptyList<String>(), be.analytics.map { it.first }.filter { it.startsWith("fallback") })
    }

    @Test
    fun goEventsGoToAnalyticsEachTimeNotDaily() {
        start(intent(analytics = true))
        online()
        settle()
        verdict(true)
        calls()
        go("fallback_connect", "ok", ms = 900)
        go("fallback_connect", "fail", "captcha", 901)
        go("fallback_helped", ms = 1500)
        go("fallback_captcha", "backoff")
        go("fallback_off", "ok", "relay_back", 60_000, 12)
        go("fallback_off", "ok", "relay_back", 60_000, 12)
        val sent = be.analytics.filter { it.first != "fallback_on" && it.first.startsWith("fallback") }
        assertEquals(
            listOf<Pair<String, Map<String, Any>>>(
                "fallback_connect" to mapOf("result" to "ok", "ms" to 900L),
                "fallback_connect" to mapOf("result" to "fail", "step" to "captcha", "ms" to 901L),
                "fallback_helped" to mapOf("ms" to 1500L),
                "fallback_captcha" to mapOf("result" to "backoff"),
                "fallback_off" to mapOf("result" to "ok", "detail" to "relay_back", "ms" to 60_000L, "mb" to 12L),
                "fallback_off" to mapOf("result" to "ok", "detail" to "relay_back", "ms" to 60_000L, "mb" to 12L),
            ),
            sent,
        )
    }

    // Kotlin passes the reason; Go alone reports fallback_off.
    @Test
    fun kotlinDoesNotEmitFallbackOff() {
        start(intent(analytics = true))
        online()
        settle()
        verdict(true)
        verdict(false)
        calls()
        assertEquals(0, be.analytics.count { it.first == "fallback_off" })
    }

    @Test
    fun theLogNeverNamesTheRoom() {
        channelOn()
        verdict(false)
        calls()
        assertFalse(TunnelState.logLines().any { "testroom" in it })
        assertFalse(AppLog.files(app).any { "testroom" in it.readText() })
    }

    @Test
    fun theJournalAndTheNotificationNeverNameVk() {
        channelOn()
        go("fallback_connect", "ok", ms = 900)
        assertFalse(notificationText().orEmpty().contains("VK"))
        verdict(false)
        calls()
        assertFalse(TunnelState.logLines().toString(), TunnelState.logLines().any { "VK" in it })
    }

    // ---- fallback_relay_dead

    private val dead = TunnelVpnService.Endpoint("relay-dead.invalid", "192.0.2.1", 443, "/relay-dead")
    private val deadLine = "Сервер отключён для отладки"
    private val listJson = """[{"host":"a.example.org","ip":"203.0.113.10","port":443,"path":"/app/aaaa1"},{"host":"b.example.net","ip":"198.51.100.7","port":8443,"path":"/app/bbbb2"}]"""
    private val knownFile get() = java.io.File(app.filesDir, Endpoints.KNOWN_FILE)

    private fun deadIntent(on: Boolean = true) = intent().putExtra(TunnelVpnService.EXTRA_FALLBACK_RELAY_DEAD, on)

    // As MainActivity sends it to a running tunnel: no other extras, the start itself is skipped.
    private fun flip(on: Boolean) {
        svc.onStartCommand(
            Intent(TunnelVpnService.ACTION_START)
                .putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram"))
                .putExtra(TunnelVpnService.EXTRA_FALLBACK_RELAY_DEAD, on),
            0, 3,
        )
        ShadowLooper.idleMainLooper()
    }

    private fun assertNothingDeadPersisted() {
        assertFalse("known file", knownFile.exists() && "192.0.2.1" in knownFile.readText())
        assertFalse("known in memory", Endpoints.known.contains(dead))
        assertFalse("cache", Endpoints.cached(app).orEmpty().contains(dead))
        assertTrue("working", Endpoints.working(app) != dead)
        val leaks = prefs.all.filterValues { "192.0.2.1" in it.toString() || "relay-dead" in it.toString() }
        assertEquals(emptyMap<String, Any?>(), leaks)
    }

    private fun connectingError(i: Intent): String? {
        svc.onStartCommand(i, 0, 1)
        val deadline = System.currentTimeMillis() + 10_000
        while (TunnelState.state != VpnState.ERROR && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, TimeUnit.MILLISECONDS)
        }
        assertEquals(VpnState.ERROR, TunnelState.state)
        return TunnelState.lastError
    }

    private fun internet() {
        val cm = app.getSystemService(ConnectivityManager::class.java)
        val caps = ShadowNetworkCapabilities.newInstance()
        shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
        shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
        shadowOf(cm).setNetworkCapabilities(cm.activeNetwork, caps)
    }

    @Test
    fun relayDeadUsesTheDeadEndpointAndTheProductionCredential() {
        org.junit.Assume.assumeTrue(BuildConfig.DEBUG)
        be.endpoints = listOf(a, b)
        val real = Credentials.load(app)!!
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400, kind = 9), test = true)
        be.relayDownAtStart = true
        start(deadIntent())
        assertEquals(listOf(dead), be.startedEndpoints.toList())
        assertTrue(real.contentEquals(be.startedWith))
        assertEquals(0, be.registered.size)
        assertTrue("the fetch of RC goes on", be.fetches.get() > 0)
        assertEquals(1, TunnelState.logLines().count { deadLine in it })

        online()
        settle()
        assertEquals("a real outage to the channel", 1, enabledCount())
        assertEquals(Waiting.NO_SERVER, TunnelState.waiting)
        // Rounds and new networks: nothing to go to.
        verdict(true)
        ShadowLooper.idleMainLooper(TunnelVpnService.ROUND_PAUSE_MS * 2, TimeUnit.MILLISECONDS)
        netCallback()!!.onAvailable(net(8))
        ShadowLooper.idleMainLooper(TunnelVpnService.NEW_NETWORK_SWITCH_MS, TimeUnit.MILLISECONDS)
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), be.endpointsSet.toList())
        assertNothingDeadPersisted()
    }

    @Test
    fun aNewListWhileDeadIsCachedButNotDialed() {
        org.junit.Assume.assumeTrue(BuildConfig.DEBUG)
        be.endpoints = listOf(a)
        Endpoints.attach(app)
        be.relayDownAtStart = true
        start(deadIntent())
        online()
        settle()
        be.config = RcValues("blob:$listJson", 0, 0, "")
        fetchAgain()
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), be.endpointsSet.toList())
        assertEquals(listOf(a, b), Endpoints.cached(app))
        assertTrue(Endpoints.known.containsAll(listOf(a, b)))
        assertNothingDeadPersisted()
        // And it is what comes back.
        flip(false)
        assertEquals(listOf(a), be.endpointsSet.toList())
    }

    @Test
    fun aVerdictOfUpLandingOnTheDeadEndpointIsNotWorking() {
        org.junit.Assume.assumeTrue(BuildConfig.DEBUG)
        be.endpoints = listOf(a)
        start(deadIntent())
        verdict(false)
        assertNull(Endpoints.working(app))
        assertNothingDeadPersisted()
    }

    @Test
    fun turningOffBringsTheRealEndpointBackAndTheChannelGoesOff() {
        org.junit.Assume.assumeTrue(BuildConfig.DEBUG)
        be.endpoints = listOf(a, b)
        Endpoints.setWorking(app, b)
        be.relayDownAtStart = true
        start(deadIntent())
        online()
        settle()
        assertEquals(1, enabledCount())

        flip(false)
        assertEquals("the last that worked", listOf(b), be.endpointsSet.toList())
        assertEquals(listOf(true), be.suspects.toList())
        assertEquals("no restart", 1, be.events.count { it == "start" })
        assertEquals(0, be.events.count { it == "stop" })
        assertEquals(VpnState.CONNECTED, TunnelState.state)
        assertEquals("not before the relay says so", 0, calls().count { it.startsWith("fallbackDisable") })

        verdict(false)
        assertEquals("fallbackDisable:relay_back", calls().last())
        assertNull(TunnelState.fallback)
        assertNull(TunnelState.waiting)
        assertEquals(b, Endpoints.working(app))
        assertNothingDeadPersisted()
        assertEquals(1, TunnelState.logLines().count { "Сервер снова включён" in it })
    }

    @Test
    fun turningOffWithNothingWorkingGoesToTheFirst() {
        org.junit.Assume.assumeTrue(BuildConfig.DEBUG)
        be.endpoints = listOf(a, b)
        start(deadIntent())
        flip(false)
        assertEquals(listOf(a), be.endpointsSet.toList())
    }

    @Test
    fun turningOnSwitchesARunningTunnelWithoutARestart() {
        org.junit.Assume.assumeTrue(BuildConfig.DEBUG)
        be.endpoints = listOf(a)
        start()
        online()
        settle()
        flip(true)
        assertEquals(listOf(dead), be.endpointsSet.toList())
        assertEquals(1, be.events.count { it == "start" })
        assertEquals(1, TunnelState.logLines().count { deadLine in it })
        verdict(true)
        assertEquals(1, enabledCount())
        assertNothingDeadPersisted()
        // Told again: no second switch.
        flip(true)
        assertEquals(1, be.endpointsSet.size)
    }

    @Test
    fun itOutlivesTheStartThatBroughtItAndGoesWithFalse() {
        org.junit.Assume.assumeTrue(BuildConfig.DEBUG)
        be.endpoints = listOf(a)
        start(deadIntent())
        stop()
        be.fetches.set(0)
        start(Intent(TunnelVpnService.ACTION_START).putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram")))
        assertEquals(dead, be.startedEndpoints.last())
        assertEquals(1, TunnelState.logLines().count { deadLine in it })
        stop()
        be.fetches.set(0)
        start(deadIntent(false))
        assertEquals(a, be.startedEndpoints.last())
        assertEquals(0, TunnelState.logLines().count { deadLine in it })
    }

    @Test
    fun aRebuildStaysOnTheDeadEndpoint() {
        org.junit.Assume.assumeTrue(BuildConfig.DEBUG)
        RouteCache.file(app).delete()
        be.endpoints = listOf(a)
        start(deadIntent().putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("twitter")))
        online()
        rebuild()
        val deadline = System.currentTimeMillis() + 10_000
        while (be.startedEndpoints.size < 2 && System.currentTimeMillis() < deadline) Thread.sleep(20)
        awaitServiceThreads()
        ShadowLooper.idleMainLooper()
        RouteCache.file(app).delete()
        assertEquals(listOf(dead, dead), be.startedEndpoints.toList())
    }

    @Test
    fun anExpiredCredentialFailsAsTodayWithoutDialingTheRealOnes() {
        org.junit.Assume.assumeTrue(BuildConfig.DEBUG)
        be.endpoints = listOf(a, b)
        internet()
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 - 60))
        be.token = "tok".toByteArray()
        be.registerError = Exception("relay unreachable: i/o timeout")
        assertEquals(TunnelVpnService.ERR_NO_SERVER, connectingError(deadIntent()))
        assertEquals(listOf(dead.addr), be.registeredAddrs.toList())
        assertEquals(0, be.startedEndpoints.size)
    }

    @Test
    fun noCredentialAndNoIntegrityAsksForACodeAsToday() {
        org.junit.Assume.assumeTrue(BuildConfig.DEBUG)
        internet()
        Credentials.clear(app)
        assertEquals(TunnelVpnService.ERR_NEED_CODE, connectingError(deadIntent()))
        assertEquals(emptyList<String>(), be.registeredAddrs.toList())
    }

    @Test
    fun aCredentialCloseToExpiryTriesTheDeadEndpointOnlyAndStillWorks() {
        org.junit.Assume.assumeTrue(BuildConfig.DEBUG)
        be.endpoints = listOf(a, b)
        val cred = fakeCred(System.currentTimeMillis() / 1000 + 3600)
        Credentials.save(app, cred)
        be.token = "tok".toByteArray()
        be.registerError = Exception("relay unreachable: i/o timeout")
        start(deadIntent())
        assertEquals(listOf(dead.addr), be.registeredAddrs.toList())
        assertTrue(cred.contentEquals(be.startedWith))
        assertEquals(listOf(dead), be.startedEndpoints.toList())
    }

    // How the emulator exercises it: the stand's credential, and off goes to the stand.
    @Test
    fun overTheStandItFallsBackToTheStand() {
        org.junit.Assume.assumeTrue(BuildConfig.DEBUG)
        val stand = "stand|test.local|10.0.2.2|8443|/app/test"
        val standCred = fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400, kind = 9)
        Credentials.save(app, standCred, test = true)
        be.relayDownAtStart = true
        start(deadIntent().putExtra(TunnelVpnService.EXTRA_ENDPOINT, stand))
        assertEquals(listOf(dead), be.startedEndpoints.toList())
        assertTrue(standCred.contentEquals(be.startedWith))
        svc.onStartCommand(deadIntent(false).putExtra(TunnelVpnService.EXTRA_ENDPOINT, stand), 0, 3)
        ShadowLooper.idleMainLooper()
        assertEquals(listOf(TunnelVpnService.Endpoint("test.local", "10.0.2.2", 8443, "/app/test")), be.endpointsSet.toList())
        // A plain START would drop the stand, so nothing here may do without the extra.
        assertEquals(1, be.startedEndpoints.size)
    }

    // Only `testReleaseUnitTest --tests '*FallbackTest.aReleaseBuild*'` runs this: BuildConfig.DEBUG is false there.
    @Test
    fun aReleaseBuildIgnoresTheExtraAndThePref() {
        org.junit.Assume.assumeFalse(BuildConfig.DEBUG)
        be.endpoints = listOf(a)
        prefs.edit().putBoolean("fallback_relay_dead.test", true).commit()
        start(deadIntent())
        assertEquals(listOf(a), be.startedEndpoints.toList())
        assertFalse(TunnelState.logLines().any { deadLine in it })
        verdict(true)
        verdict(false)
        assertEquals(a, Endpoints.working(app))
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), be.endpointsSet.toList())
    }
}
