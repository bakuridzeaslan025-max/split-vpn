package org.duckdns.splitvpn

import android.content.Context
import android.content.Intent
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.net.VpnService
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
import org.robolectric.shadows.ShadowVpnService
import org.robolectric.util.ReflectionHelpers
import java.time.LocalDate
import java.util.concurrent.TimeUnit

/** The events and parameters added after 0.9.0: src, detail, relay_day, relay_dial, yt_direct. */
@RunWith(RobolectricTestRunner::class)
class AnalyticsEventsTest {

    private lateinit var controller: ServiceController<TunnelVpnService>
    private val app get() = RuntimeEnvironment.getApplication()
    private val svc get() = controller.get()
    private val be = FakeBackend()
    private val nowS get() = System.currentTimeMillis() / 1000
    private var day = 20_000L
    private val prefs get() = app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)
    private val mb = 1024L * 1024

    @Before
    fun setUp() {
        Quota.today = { day }
        TunnelState.set(VpnState.DISCONNECTED)
        TunnelState.clearLog()
        ShadowLooper.idleMainLooper()
        prefs.edit().clear().commit()
        RouteCache.file(app).delete()
        Endpoints.resetForTests()
        // Robolectric's default is an unprepared app.
        ShadowVpnService.setPrepareResult(null)
        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
        svc.backend = be
        Credentials.save(app, fakeCred(nowS + 5 * 86400))
        ShadowLooper.idleMainLooper()
    }

    @After
    fun tearDown() {
        Quota.today = { LocalDate.now().toEpochDay() }
        ShadowVpnService.setPrepareResult(null)
        be.release()
        awaitServiceThreads()
        controller.destroy()
        ShadowLooper.idleMainLooper()
        RouteCache.file(app).delete()
        Endpoints.resetForTests()
    }

    private fun sent(name: String) = be.analytics.filter { it.first == name }.map { it.second }
    private fun failed() = sent("vpn_failed")

    private fun ui(vararg services: String = arrayOf("telegram"), analytics: Boolean = true) =
        Intent(TunnelVpnService.ACTION_START).putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf(*services))
            .putExtra(TunnelVpnService.EXTRA_ANALYTICS, analytics)

    // Boot, the resume on create: a START without the services.
    private fun resume() = Intent(TunnelVpnService.ACTION_START).putExtra(TunnelVpnService.EXTRA_ANALYTICS, true)

    private fun alwaysOn() = Intent(VpnService.SERVICE_INTERFACE).putExtra(TunnelVpnService.EXTRA_ANALYTICS, true)

    private fun start(intent: Intent? = ui()) {
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
        while ((TunnelState.state !in want || TunnelState.state == VpnState.CONNECTED && screenReceivers().isEmpty()) &&
            System.currentTimeMillis() < deadline
        ) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, TimeUnit.MILLISECONDS)
        }
        assertTrue("state=${TunnelState.state} err=${TunnelState.lastError}", TunnelState.state in want)
        awaitServiceThreads()
        ShadowLooper.idleMainLooper()
    }

    private fun screenReceivers() = shadowOf(app).registeredReceivers.filter { it.intentFilter.hasAction(Intent.ACTION_SCREEN_ON) }

    private fun screenOn() {
        app.sendBroadcast(Intent(Intent.ACTION_SCREEN_ON))
        ShadowLooper.idleMainLooper()
    }

    private fun activeNetwork(validated: Boolean = true, transport: Int? = null) {
        val cm = app.getSystemService(ConnectivityManager::class.java)
        val caps = ShadowNetworkCapabilities.newInstance()
        shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
        if (validated) shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
        transport?.let { shadowOf(caps).addTransportType(it) }
        shadowOf(cm).setNetworkCapabilities(cm.activeNetwork, caps)
    }

    private val wanted get() = prefs.getBoolean(TunnelVpnService.KEY_WANTED, false)

    // ---- src

    @Test
    fun srcTellsWhoStarted() {
        assertEquals("sticky", TunnelVpnService.srcOf(null))
        assertEquals("always_on", TunnelVpnService.srcOf(Intent(VpnService.SERVICE_INTERFACE)))
        assertEquals("ui", TunnelVpnService.srcOf(ui()))
        assertEquals("resume", TunnelVpnService.srcOf(Intent(TunnelVpnService.ACTION_START)))
    }

    // One reason, two starters: two events; the same starter again: none.
    @Test
    fun failedStartsCarryTheirSrcOncePerSrc() {
        be.startError = IllegalStateException("relay unreachable")
        start(resume())
        start(ui())
        start(ui())
        start(alwaysOn())
        assertEquals(listOf("resume", "ui", "always_on"), failed().map { it["src"] })
        assertTrue(failed().all { it["reason"] == "other" })
    }

    @Test
    fun offlineCarriesTheStartsSrcAndItsRetryIsResume() {
        activeNetwork(validated = false)
        Credentials.save(app, fakeCred(nowS - 10))
        start(ui())
        assertEquals(TunnelVpnService.ERR_OFFLINE, TunnelState.lastError)
        assertEquals(listOf(mapOf("reason" to "offline", "src" to "ui")), failed())
        // The network is back, the retry fails otherwise.
        activeNetwork(validated = true)
        be.token = "tok".toByteArray()
        be.registerError = Exception("registration rejected")
        shadowOf(app.getSystemService(ConnectivityManager::class.java)).networkCallbacks.toList()
            .forEach { it.onAvailable(ShadowNetwork.newInstance(9)) }
        ShadowLooper.idleMainLooper()
        awaitState(VpnState.ERROR)
        awaitServiceThreads()
        ShadowLooper.idleMainLooper()
        assertEquals(TunnelVpnService.ERR_NEED_CODE, TunnelState.lastError)
        assertEquals(mapOf("reason" to "need_code", "src" to "resume"), failed().last())
    }

    @Test
    fun relayDownIsRun() {
        start()
        shadowOf(app.getSystemService(ConnectivityManager::class.java)).networkCallbacks.toList()
            .forEach { it.onAvailable(ShadowNetwork.newInstance(7)) }
        val caps = ShadowNetworkCapabilities.newInstance()
        shadowOf(caps).addCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
        ReflectionHelpers.getField<ConnectivityManager.NetworkCallback>(svc, "defaultCallback")
            .onCapabilitiesChanged(ShadowNetwork.newInstance(7), caps)
        ShadowLooper.idleMainLooper(TunnelVpnService.NEW_NETWORK_SWITCH_MS, TimeUnit.MILLISECONDS)
        be.startedHost!!.relayDown(true)
        ShadowLooper.idleMainLooper()
        assertEquals(listOf(mapOf("reason" to "relay_down", "src" to "run")), failed())
    }

    // ---- establish

    @Test
    fun noTunWithoutPermissionIsPerm() {
        be.establishNull = true
        ShadowVpnService.setPrepareResult(Intent("consent"))
        start()
        assertEquals(TunnelVpnService.ERR_VPN_TAKEN, TunnelState.lastError)
        assertEquals(listOf(mapOf("reason" to "establish", "src" to "ui", "detail" to "perm")), failed())
    }

    @Test
    fun noTunWithPermissionHasNoDetail() {
        be.establishNull = true
        start()
        assertEquals(TunnelVpnService.ERR_ESTABLISH, TunnelState.lastError)
        assertEquals(listOf(mapOf("reason" to "establish", "src" to "ui")), failed())
    }

    @Test
    fun builderExceptionsGoByClassNeverText() {
        val errors = listOf(
            IllegalArgumentException("Bad address 203.0.113.10"),
            IllegalStateException("203.0.113.10"),
            SecurityException("203.0.113.10"),
            UnsupportedOperationException("203.0.113.10"),
        )
        for ((i, e) in errors.withIndex()) {
            day = 20_000L + i // a day each: one event per (reason, src) a day
            be.establishError = e
            start()
            assertEquals(TunnelVpnService.ERR_ESTABLISH, TunnelState.lastError)
        }
        assertEquals(errors.map { it.javaClass.simpleName }, failed().map { it["detail"] })
        assertTrue(failed().all { it["reason"] == "establish" })
        assertFalse(be.analytics.toString().contains("203.0.113"))
    }

    @Test
    fun establishOnReRegistrationToo() {
        be.rejectCreds += Credentials.load(app)!!.toList()
        be.token = "tok".toByteArray()
        be.registerResult = fakeCred(nowS + 5 * 86400)
        // The first TUN comes up, the one after the new credential does not.
        var n = 0
        val real = be.pfd
        be.establishHook = { if (++n > 1) throw IllegalArgumentException("Bad route 203.0.113.0/24") }
        start()
        be.establishHook = null
        be.pfd = real
        assertEquals(listOf(mapOf("reason" to "establish", "src" to "ui", "detail" to "IllegalArgumentException")), failed())
    }

    @Test
    fun establishOnRebuildIsRebuildWithItsDetail() {
        start(ui("twitter"))
        RouteCache.file(app).writeText("x.com 192.0.2.0 $nowS\n")
        be.establishNull = true
        ShadowVpnService.setPrepareResult(Intent("consent"))
        be.startedHost!!.routesStale()
        awaitState(VpnState.ERROR)
        assertEquals(listOf(mapOf("reason" to "rebuild", "src" to "run", "detail" to "perm")), failed())
        assertEquals(TunnelVpnService.ERR_VPN_TAKEN, TunnelState.lastError)
        assertFalse("a background start would fail on every boot", wanted)
    }

    // Another VPN holds the permission: only a tap can take it back.
    @Test
    fun permFromTheBackgroundStopsTheRetries() {
        be.establishNull = true
        ShadowVpnService.setPrepareResult(Intent("consent"))
        for (intent in listOf(resume(), null, alwaysOn())) {
            prefs.edit().putBoolean(TunnelVpnService.KEY_WANTED, true).putBoolean(TunnelVpnService.KEY_RESUME_AFTER_UPDATE, true).commit()
            start(intent)
            assertEquals(TunnelVpnService.ERR_VPN_TAKEN, TunnelState.lastError)
            assertFalse("src ${TunnelVpnService.srcOf(intent)}", wanted)
            assertFalse(prefs.getBoolean(TunnelVpnService.KEY_RESUME_AFTER_UPDATE, true))
        }
        // Boot and the next bind leave it be.
        while (shadowOf(app).nextStartedService != null) Unit
        BootReceiver().onReceive(app, Intent(Intent.ACTION_BOOT_COMPLETED))
        BootReceiver().onReceive(app, Intent(Intent.ACTION_MY_PACKAGE_REPLACED))
        prefs.edit().remove(TunnelVpnService.KEY_FAILED_AT).commit()
        Robolectric.buildService(TunnelVpnService::class.java).create().destroy()
        assertNull(shadowOf(app).nextStartedService)
    }

    private fun permNote() = shadowOf(app.getSystemService(android.app.NotificationManager::class.java))
        .getNotification(TunnelVpnService.PERM_NOTIFICATION_ID)

    // Wanted is gone for good, often with the app closed: the user is told.
    @Test
    fun permFromTheBackgroundNotifiesUntilTheNextStart() {
        be.establishNull = true
        ShadowVpnService.setPrepareResult(Intent("consent"))
        start(resume())
        val n = permNote()!!
        assertEquals("Split VPN выключен", n.extras.getString(android.app.Notification.EXTRA_TITLE))
        assertEquals(TunnelVpnService.ERR_VPN_TAKEN, n.extras.getString(android.app.Notification.EXTRA_TEXT))
        assertEquals("quota_channel", n.channelId)
        assertTrue(n.flags and android.app.Notification.FLAG_AUTO_CANCEL != 0)
        assertEquals(
            MainActivity::class.java.name,
            shadowOf(n.contentIntent).savedIntent.component?.className,
        )
        be.establishNull = false
        ShadowVpnService.setPrepareResult(null)
        start(ui())
        assertNull(permNote())
    }

    @Test
    fun permOnRebuildNotifies() {
        start(ui("twitter"))
        RouteCache.file(app).writeText("x.com 192.0.2.0 $nowS\n")
        be.establishNull = true
        ShadowVpnService.setPrepareResult(Intent("consent"))
        be.startedHost!!.routesStale()
        awaitState(VpnState.ERROR)
        assertEquals("Split VPN выключен", permNote()?.extras?.getString(android.app.Notification.EXTRA_TITLE))
    }

    // The screen already shows the error; no notification for a start the user made.
    @Test
    fun permFromATapOrNoPermDoesNotNotify() {
        be.establishNull = true
        ShadowVpnService.setPrepareResult(Intent("consent"))
        start(ui())
        assertNull(permNote())
        ShadowVpnService.setPrepareResult(null)
        start(resume())
        assertNull(permNote())
    }

    @Test
    fun permFromATapOrNoPermIssueKeepsWanted() {
        be.establishNull = true
        ShadowVpnService.setPrepareResult(Intent("consent"))
        start(ui())
        assertEquals(TunnelVpnService.ERR_VPN_TAKEN, TunnelState.lastError)
        assertTrue(wanted)
        ShadowVpnService.setPrepareResult(null)
        start(resume())
        assertEquals(TunnelVpnService.ERR_ESTABLISH, TunnelState.lastError)
        assertTrue(wanted)
    }

    @Test
    fun bothTextsAreEstablishAndExpected() {
        for (m in listOf(TunnelVpnService.ERR_ESTABLISH, TunnelVpnService.ERR_VPN_TAKEN)) {
            assertEquals("establish", TunnelVpnService.reasonOf(m))
            assertTrue(TunnelVpnService.expected(Exception(m)))
        }
    }

    // ---- relay_day

    @Test
    fun screenTurnsTheDayAndSendsYesterdaysFullCount() {
        start()
        be.used = 5 * mb + 1
        day++
        screenOn()
        assertEquals(listOf(mapOf<String, Any>("mb" to 6L)), sent("relay_day"))
        assertEquals("today starts from zero", 0L to Quota.limit(app), be.quotasSet.last())
        assertEquals(day, Quota.day(app))
        assertEquals(0L, Quota.used(app))
        screenOn()
        assertEquals(1, sent("relay_day").size)
    }

    @Test
    fun noTrafficNoEvent() {
        start()
        day++
        screenOn()
        assertTrue(sent("relay_day").isEmpty())
        assertEquals(day, Quota.day(app))
    }

    @Test
    fun aDayFromTomorrowIsDroppedWithoutAnEvent() {
        start()
        be.used = 5 * mb
        day--
        screenOn()
        assertTrue(sent("relay_day").isEmpty())
        assertEquals(0L to Quota.limit(app), be.quotasSet.last())
    }

    // The limit's day does not wait for Analytics.
    @Test
    fun withoutCollectionTheDayStillTurns() {
        start(ui(analytics = false))
        be.used = 5 * mb
        day++
        screenOn()
        assertTrue(be.analytics.isEmpty())
        assertEquals(0L to Quota.limit(app), be.quotasSet.last())
        assertEquals(day, Quota.day(app))
    }

    @Test
    fun offAtMidnightSendsTheStoredDayOnStart() {
        Quota.save(app, 3 * mb + 1, day - 1)
        start()
        assertEquals(listOf(mapOf<String, Any>("mb" to 4L)), sent("relay_day"))
        stop()
        start()
        assertEquals(1, sent("relay_day").size)
    }

    @Test
    fun aWeekOldDaySendsToo() {
        Quota.save(app, 2 * mb, day - 7)
        start()
        assertEquals(listOf(mapOf<String, Any>("mb" to 2L)), sent("relay_day"))
    }

    // Before the credential: a start that ends in need_code still sends it.
    @Test
    fun offAtMidnightSendsEvenWhenTheStartFails() {
        Quota.save(app, mb, day - 1)
        activeNetwork(validated = true)
        Credentials.save(app, fakeCred(nowS - 10))
        be.token = "tok".toByteArray()
        be.registerError = Exception("registration rejected")
        start()
        assertEquals(TunnelVpnService.ERR_NEED_CODE, TunnelState.lastError)
        assertEquals(listOf(mapOf<String, Any>("mb" to 1L)), sent("relay_day"))
    }

    @Test
    fun checkDayOnlyWhileUp() {
        start()
        stop()
        be.quotasSet.clear()
        day++
        ReflectionHelpers.callInstanceMethod<Unit>(svc, "checkDay")
        assertTrue("DISCONNECTED", be.quotasSet.isEmpty())
        start()
        be.quotasSet.clear()
        day++
        ReflectionHelpers.setField(svc, "stopping", true)
        ReflectionHelpers.callInstanceMethod<Unit>(svc, "checkDay")
        ReflectionHelpers.setField(svc, "stopping", false)
        assertTrue("stopping", be.quotasSet.isEmpty())
    }

    // The start's check from prefs and a turn of Go's day against the
    // count's own day while connecting: one event either way.
    @Test
    fun aTurnWhileConnectingIsOneEvent() {
        start()
        be.used = 7 * mb
        stop()
        day++
        be.blockStart = true
        svc.onStartCommand(ui(), 0, 3)
        ReflectionHelpers.callInstanceMethod<Unit>(svc, "checkDay")
        be.release()
        awaitState(VpnState.CONNECTED)
        assertEquals(listOf(mapOf<String, Any>("mb" to 7L)), sent("relay_day"))
    }

    @Test
    fun theLimitCountsFromZeroAfterTheTurn() {
        Quota.setLimitMb(app, 100)
        start()
        be.used = 90 * mb
        day++
        screenOn()
        be.used += 20 * mb
        be.startedHost!!.quotaExceeded()
        ShadowLooper.idleMainLooper()
        assertEquals("20 MB of today's 100 is no exhaustion", VpnState.CONNECTED, TunnelState.state)
    }

    // ---- yt_direct

    @Test
    fun ytDirectFollowsTheVerdictOrOff() {
        start(ui().putExtra(TunnelVpnService.EXTRA_DIRECT, false))
        be.startedHost!!.directVerdict("unknown")
        assertEquals("off", be.userProperties["yt_direct"])
        stop()
        start(ui().putExtra(TunnelVpnService.EXTRA_DIRECT, true))
        be.startedHost!!.directVerdict("testing")
        assertEquals("testing", be.userProperties["yt_direct"])
        be.startedHost!!.directVerdict("works")
        assertEquals("works", be.userProperties["yt_direct"])
        stop()
        prefs.edit().putString(RemoteConfig.KEY_YT_STRATEGIES, "[]").commit()
        start(ui().putExtra(TunnelVpnService.EXTRA_DIRECT, true))
        be.startedHost!!.directVerdict("unknown")
        assertEquals("off", be.userProperties["yt_direct"])
        stop()
        // Every entry broken: Go dropped them all.
        prefs.edit().putString(RemoteConfig.KEY_YT_STRATEGIES, """[{"id":"x","spec":"cut=?"}]""").commit()
        be.dropped = "x: bad"
        be.kept = 0
        start(ui().putExtra(TunnelVpnService.EXTRA_DIRECT, true))
        be.startedHost!!.directVerdict("unknown")
        assertEquals("off", be.userProperties["yt_direct"])
    }

    // A failed rebuild leaves Go's count in prefs: the next start goes on
    // from it, and yesterday goes once.
    @Test
    fun failedRebuildKeepsTheCount() {
        start(ui("twitter"))
        be.used = 100 * mb
        RouteCache.file(app).writeText("x.com 192.0.2.0 $nowS\n")
        be.startError = IllegalStateException("fdbased: bad file descriptor")
        be.startedHost!!.routesStale()
        awaitState(VpnState.ERROR)
        be.startError = null
        be.used = 0
        start(ui("twitter"))
        assertEquals(100 * mb, be.quotasSet.last().first)
        RouteCache.file(app).delete()
        be.startError = IllegalStateException("fdbased: bad file descriptor")
        be.used = 120 * mb
        ReflectionHelpers.setField(svc, "lastRebuildAt", 0L)
        RouteCache.file(app).writeText("x.com 192.0.3.0 $nowS\n")
        be.startedHost!!.routesStale()
        awaitState(VpnState.ERROR)
        be.startError = null
        day++
        start(ui("twitter"))
        screenOn()
        assertEquals(listOf(mapOf<String, Any>("mb" to 120L)), sent("relay_day"))
    }

    // ---- relay_dial

    private val a = TunnelVpnService.Endpoint("a.example.org", "203.0.113.10", 443, "/app/aaaa1")
    private val b = TunnelVpnService.Endpoint("b.example.net", "198.51.100.7", 8443, "/app/bbbb2")
    private fun labelled(ep: TunnelVpnService.Endpoint, id: String) = TunnelVpnService.Endpoint(ep.host, ep.ip, ep.port, ep.path, id)
    private fun json(vararg eps: TunnelVpnService.Endpoint) = org.json.JSONArray(
        eps.map { org.json.JSONObject().put("id", it.id).put("host", it.host).put("ip", it.ip).put("port", it.port).put("path", it.path) },
    ).toString()

    @Test
    fun labelGoesToGoAndIsNotPartOfEquality() {
        assertEquals(a, labelled(a, "nl1"))
        Endpoints.cache(app, listOf(labelled(a, "nl1"), labelled(b, "nl2")))
        start()
        assertEquals("nl1", be.startedId)
    }

    // A cache from before the labels: the fetch brings them, no switch.
    @Test
    fun labelsComeWithAFetchWithoutASwitch() {
        Endpoints.cache(app, listOf(a, b))
        be.config = RcValues("blob:" + json(labelled(a, "nl1"), labelled(b, "nl2")), 0, 0, "")
        start()
        awaitServiceThreads()
        ShadowLooper.idleMainLooper()
        assertTrue(be.endpointsSet.isEmpty())
        assertTrue(be.startedId == "nl1" || be.endpointIds.toList() == listOf("nl1"))
        assertEquals(listOf("nl1", "nl2"), Endpoints.cached(app)!!.map { it.id })
    }

    @Test
    fun dialEventNamesTheNetwork() {
        activeNetwork(transport = NetworkCapabilities.TRANSPORT_WIFI)
        start()
        // Before the default network's callback heard anything: the active one.
        be.startedHost!!.dialEvent("nl1", "ok", 42)
        val cell = ShadowNetworkCapabilities.newInstance()
        shadowOf(cell).addTransportType(NetworkCapabilities.TRANSPORT_CELLULAR)
        ReflectionHelpers.getField<ConnectivityManager.NetworkCallback>(svc, "defaultCallback")
            .onCapabilitiesChanged(ShadowNetwork.newInstance(7), cell)
        be.startedHost!!.dialEvent("nl1", "hello_timeout", 10_000)
        val got = sent("relay_dial")
        assertEquals(mapOf("ep" to "nl1", "net" to "wifi", "result" to "ok", "ms" to 42L), got[0])
        assertEquals("cell", got[1]["net"])
        assertEquals("hello_timeout", got[1]["result"])
        assertEquals(10_000L, got[1]["ms"])
        got[1]["op"]?.let { assertTrue(it.toString().all(Char::isDigit)) }
    }

    @Test
    fun noDialEventsWithoutCollection() {
        start(ui(analytics = false))
        be.startedHost!!.dialEvent("nl1", "ok", 42)
        assertTrue(be.analytics.isEmpty())
    }

    @Test
    fun dialEventBpGoesToGoOnStartAndFetch() {
        prefs.edit().putInt(RemoteConfig.KEY_DIAL_EVENT_BP, 7).commit()
        be.config = RcValues("", 0, 0, "", dialEventBp = 30)
        be.blockConfig = true
        start()
        assertEquals(7, be.dialEvents)
        be.release()
        awaitServiceThreads()
        assertEquals(30, be.dialEvents)
    }

    @Test
    fun dialEventBpParse() {
        mapOf(null to 0, "" to 0, "x" to 0, "-1" to 0, "10001" to 0, "1.5" to 0, "0" to 0, " 25 " to 25, "10000" to 10000)
            .forEach { (raw, want) -> assertEquals("$raw", want, RemoteConfig.parseDialEventBp(raw)) }
    }

    // 0.11 and older read the fields by name: the label is ignored.
    @Test
    fun oldParserIgnoresTheLabel() {
        val o = org.json.JSONArray(json(labelled(a, "nl1"))).getJSONObject(0)
        assertEquals(a, TunnelVpnService.Endpoint(o.getString("host"), o.getString("ip"), o.getInt("port"), o.getString("path")))
    }

    @Test
    fun numbersGoAsLongs() {
        val bundle = Analytics.bundle(mapOf("i" to 3, "l" to 4L, "s" to "x"))
        assertEquals(3L, bundle.getLong("i"))
        assertEquals(4L, bundle.getLong("l"))
        assertEquals("x", bundle.getString("s"))
    }
}
