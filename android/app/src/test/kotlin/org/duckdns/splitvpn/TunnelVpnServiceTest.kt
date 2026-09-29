package org.duckdns.splitvpn

import android.content.ComponentName
import android.content.Intent
import android.net.VpnService
import android.os.Messenger
import android.util.Log
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.Shadows.shadowOf
import org.robolectric.android.controller.ServiceController
import org.robolectric.shadows.ShadowLog
import org.robolectric.shadows.ShadowLooper

@RunWith(RobolectricTestRunner::class)
class TunnelVpnServiceTest {

    private lateinit var controller: ServiceController<TunnelVpnService>
    private val app get() = RuntimeEnvironment.getApplication()

    private var fake: FakeBackend? = null

    data class Snap(val state: VpnState, val error: String?, val log: List<String>)

    @Before
    fun setUp() {
        TunnelState.set(VpnState.DISCONNECTED)
        TunnelState.clearLog()
        ShadowLooper.idleMainLooper()
        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
    }

    @After
    fun tearDown() {
        Updater.debug = true
        TunnelVpnService.VERSION_FETCH_WAIT_MS = 10_000L
        fake?.release()
        awaitServiceThreads()
        controller.destroy()
        ShadowLooper.idleMainLooper()
    }

    private fun bindClient(onSnap: (Snap) -> Unit): VpnClient {
        val binder = controller.get().onBind(Intent(TunnelVpnService.ACTION_BIND))!!
        shadowOf(app).setComponentNameAndServiceForBindService(
            ComponentName(app, TunnelVpnService::class.java), binder
        )
        return VpnClient(app) { s, e, l, _, _, _ -> onSnap(Snap(s, e, l.map { it.substringAfter("  ") })) }.also { it.bind() }
    }

    @Test
    fun uiBindReturnsMessengerAndSystemBindDoesNot() {
        val svc = controller.get()
        val ui = svc.onBind(Intent(TunnelVpnService.ACTION_BIND))!!
        Messenger(ui) // must be a Messenger binder
        val sys = svc.onBind(Intent(VpnService.SERVICE_INTERFACE))
        assertNotEquals(ui, sys)
    }

    @Test
    fun registerGetsSnapshotAndUpdates() {
        val got = mutableListOf<Snap>()
        val client = bindClient { got += it }
        ShadowLooper.idleMainLooper()
        assertEquals(listOf(Snap(VpnState.DISCONNECTED, null, emptyList())), got)

        TunnelState.set(VpnState.CONNECTED)
        TunnelState.log("Туннель поднят")
        ShadowLooper.idleMainLooper()
        assertEquals(Snap(VpnState.CONNECTED, null, listOf("Туннель поднят")), got.last())

        client.unbind()
        ShadowLooper.idleMainLooper()
        val n = got.size
        TunnelState.set(VpnState.ERROR, "x")
        ShadowLooper.idleMainLooper()
        assertEquals(n, got.size)
    }

    @Test
    fun snapshotCarriesConnectedAt() {
        val since = mutableListOf<Long>()
        val binder = controller.get().onBind(Intent(TunnelVpnService.ACTION_BIND))!!
        shadowOf(app).setComponentNameAndServiceForBindService(
            ComponentName(app, TunnelVpnService::class.java), binder
        )
        VpnClient(app) { _, _, _, s, _, _ -> since += s }.bind()
        ShadowLooper.idleMainLooper()
        assertEquals(TunnelState.connectedAt, since.last())

        TunnelState.set(VpnState.CONNECTED)
        ShadowLooper.idleMainLooper()
        assertTrue(since.last() > 0)
        assertEquals(TunnelState.connectedAt, since.last())
    }

    private fun awaitState(vararg want: VpnState) {
        val deadline = System.currentTimeMillis() + 10_000
        while (TunnelState.state !in want && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, java.util.concurrent.TimeUnit.MILLISECONDS)
        }
        assertTrue("state=${TunnelState.state} err=${TunnelState.lastError}", TunnelState.state in want)
    }

    private fun startAndExpectAttempt(intent: Intent?) {
        val svc = controller.get()
        svc.onStartCommand(intent, 0, 1)
        assertEquals(1, shadowOf(svc).lastForegroundNotificationId)
        // The Go AAR has no JVM native lib and Robolectric has no real
        // ConnectivityService, so the GoTunnel thread ends in ERROR instead of
        // CONNECTED. CONNECTING is still observable synchronously.
        awaitState(VpnState.CONNECTING, VpnState.ERROR)
        awaitState(VpnState.ERROR)
    }

    /** A build without endpoints is a bug to report, not a failure of normal life. */
    @Test
    fun noEndpointsFailsStartAndIsReported() {
        val svc = controller.get()
        svc.backend = FakeBackend().apply { endpoints = emptyList() }
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 7 * 86400))
        startAndExpectAttempt(Intent(TunnelVpnService.ACTION_START))
        val err = TunnelState.lastError.orEmpty()
        assertTrue(err, err.startsWith("no endpoints: "))
        assertFalse(TunnelVpnService.expected(Exception(err)))
    }

    @Test
    fun nullIntentStartsForegroundAndTunnel() {
        startAndExpectAttempt(null)
    }

    @Test
    fun alwaysOnIntentStartsForegroundAndTunnel() {
        startAndExpectAttempt(Intent(VpnService.SERVICE_INTERFACE))
    }

    @Test
    fun foregroundNotificationHasStopAction() {
        val svc = controller.get()
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START), 0, 1)
        val n = shadowOf(svc).lastForegroundNotification!!
        assertEquals(1, n.actions.size)
        assertEquals("Выключить", n.actions[0].title)
        val i = shadowOf(n.actions[0].actionIntent).savedIntent
        assertEquals(TunnelVpnService.ACTION_STOP, i.action)
        assertEquals(TunnelVpnService::class.java.name, i.component?.className)
        // A thread still failing would land its ERROR in the next test.
        awaitState(VpnState.ERROR)
    }

    // STOP with nothing running: no DISCONNECTING dance (it would hold
    // `stopping` for the fallback's 3 s and swallow the next START).
    @Test
    fun stopIntentDisconnects() {
        val svc = controller.get()
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_STOP), 0, 1)
        assertEquals(VpnState.DISCONNECTED, TunnelState.state)
        ShadowLooper.idleMainLooper()
        assertTrue(shadowOf(svc).isStoppedBySelf)
    }

    private fun vpnPrefs() = app.getSharedPreferences(TunnelVpnService.PREFS, android.content.Context.MODE_PRIVATE)

    @Test
    fun startPersistsWantedAndServices_stopClearsWanted() {
        val start = Intent(TunnelVpnService.ACTION_START)
            .putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram", "youtube"))
        controller.get().onStartCommand(start, 0, 1)
        assertTrue(vpnPrefs().getBoolean(TunnelVpnService.KEY_WANTED, false))
        assertEquals(setOf("telegram", "youtube"), vpnPrefs().getStringSet(TunnelVpnService.KEY_SERVICES, null))

        controller.get().onStartCommand(Intent(TunnelVpnService.ACTION_STOP), 0, 2)
        assertEquals(false, vpnPrefs().getBoolean(TunnelVpnService.KEY_WANTED, true))
        // Service list survives STOP so a later null-intent restart has routes.
        assertEquals(setOf("telegram", "youtube"), vpnPrefs().getStringSet(TunnelVpnService.KEY_SERVICES, null))
        // The stop thread joins the start one first: DISCONNECTED means both are done.
        awaitState(VpnState.DISCONNECTED)
    }

    @Test
    fun createWithWantedResumesTunnel() {
        controller.destroy()
        vpnPrefs().edit().putBoolean(TunnelVpnService.KEY_WANTED, true).commit()
        TunnelState.set(VpnState.DISCONNECTED)
        ShadowLooper.idleMainLooper()

        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
        val started = shadowOf(app).nextStartedService
        assertEquals(TunnelVpnService.ACTION_START, started?.action)
        assertEquals(TunnelVpnService::class.java.name, started?.component?.className)
    }

    @Test
    fun createWithoutWantedStaysIdle() {
        // setUp already created the service with wanted=false.
        assertEquals(null, shadowOf(app).nextStartedService)
    }

    @Test
    fun bootReceiverStartsOnlyWhenWanted() {
        val boot = Intent(Intent.ACTION_BOOT_COMPLETED)
        BootReceiver().onReceive(app, boot)
        assertEquals(null, shadowOf(app).nextStartedService)

        vpnPrefs().edit().putBoolean(TunnelVpnService.KEY_WANTED, true).commit()
        BootReceiver().onReceive(app, boot)
        assertEquals(TunnelVpnService.ACTION_START, shadowOf(app).nextStartedService?.action)
    }

    // A UI that binds while the tunnel waits gets the warning in its first snapshot.
    @Test
    fun registerSnapshotCarriesWaiting() {
        TunnelState.set(VpnState.CONNECTED)
        TunnelState.setWaiting(Waiting.NO_SERVER)
        val got = mutableListOf<Waiting?>()
        VpnClient(app) { _, _, _, _, w, _ -> got += w }.also {
            val binder = controller.get().onBind(Intent(TunnelVpnService.ACTION_BIND))!!
            shadowOf(app).setComponentNameAndServiceForBindService(ComponentName(app, TunnelVpnService::class.java), binder)
        }.bind()
        ShadowLooper.idleMainLooper()
        assertEquals(Waiting.NO_SERVER, got.last())
    }

    private val newer = BuildConfig.VERSION_CODE + 1L
    private val url = Updater.RELEASES + "v9.9.9/split-vpn.apk"

    private fun rc(min: Long = 0, latest: Long = 0) {
        vpnPrefs().edit()
            .putLong(RemoteConfig.KEY_MIN_VERSION, min)
            .putLong(RemoteConfig.KEY_LATEST_VERSION, latest)
            .putString(RemoteConfig.KEY_UPDATE_URL, url)
            .commit()
    }

    // A refusal of normal life: shown to the user, never a non-fatal.
    @Test
    fun minVersionBlocksEveryKindOfStart() {
        val svc = controller.get()
        // Offline: the start's own fetch fails, the cache decides.
        fake = FakeBackend().apply { configError = Exception("offline") }
        svc.backend = fake!!
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 7 * 86400))
        rc(min = newer)
        for (intent in listOf(Intent(TunnelVpnService.ACTION_START), null, Intent(VpnService.SERVICE_INTERFACE))) {
            ShadowLog.clear()
            svc.onStartCommand(intent, 0, 1)
            awaitState(VpnState.ERROR)
            assertEquals(TunnelVpnService.ERR_UPDATE_REQUIRED, TunnelState.lastError)
            assertTrue(fake!!.events.isEmpty())
            // The failed fetch is an expected error line of its own.
            assertTrue(ShadowLog.getLogsForTag(AppLog.TAG).none { it.type == Log.ERROR && !it.msg.startsWith("remote config") })
            assertFalse(vpnPrefs().getBoolean(TunnelVpnService.KEY_WANTED, true))
        }
        assertTrue(TunnelVpnService.expected(Exception(TunnelVpnService.ERR_UPDATE_REQUIRED)))
    }

    // The cache predates the lowered min_version: the first tap connects, not the second.
    @Test
    fun loweredMinVersionLetsTheFirstStartThrough() {
        val svc = controller.get()
        fake = FakeBackend().apply { config = RcValues("", 0, 0, "") }
        svc.backend = fake!!
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 7 * 86400))
        rc(min = newer)
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START), 0, 1)
        awaitState(VpnState.CONNECTED)
        assertTrue(vpnPrefs().getBoolean(TunnelVpnService.KEY_WANTED, false))
    }

    // A hung fetch holds the start no longer than the limit; when it lands
    // with a lower min_version, the red banner goes by itself.
    @Test
    fun hungFetchRefusesOnTheCacheInTimeAndLateOneClearsTheError() {
        TunnelVpnService.VERSION_FETCH_WAIT_MS = 300
        val svc = controller.get()
        fake = FakeBackend().apply { blockConfig = true; config = RcValues("", 0, 0, "") }
        svc.backend = fake!!
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 7 * 86400))
        rc(min = newer)
        val t0 = System.currentTimeMillis()
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START), 0, 1)
        awaitState(VpnState.ERROR)
        // Far from the 10 s default, far above the 300 ms: a loaded CI box is slow, not wrong.
        assertTrue(System.currentTimeMillis() - t0 < 8_000)
        assertEquals(TunnelVpnService.ERR_UPDATE_REQUIRED, TunnelState.lastError)

        val got = mutableListOf<VpnState>()
        val binder = svc.onBind(Intent(TunnelVpnService.ACTION_BIND))!!
        shadowOf(app).setComponentNameAndServiceForBindService(ComponentName(app, TunnelVpnService::class.java), binder)
        VpnClient(app) { st, _, _, _, _, _ -> got += st }.bind()
        fake!!.release()
        awaitState(VpnState.DISCONNECTED)
        ShadowLooper.idleMainLooper()
        assertEquals(VpnState.DISCONNECTED, got.last())
        assertTrue(fake!!.events.isEmpty())
    }

    // STOP while the start waits for the fetch: nothing after the wait runs, no 10 s hang.
    @Test
    fun stopDuringTheVersionWaitEndsItAtOnce() {
        val svc = controller.get()
        // No credential, a token at hand: going on would register.
        fake = FakeBackend().apply { blockConfig = true; token = ByteArray(1); registerResult = fakeCred(System.currentTimeMillis() / 1000 + 7 * 86400) }
        svc.backend = fake!!
        rc(min = newer)
        val t0 = System.currentTimeMillis()
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START), 0, 1)
        // Most likely inside the wait by now; before it, the STOP ends it just the same.
        Thread.sleep(200)
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_STOP), 0, 2)
        awaitState(VpnState.DISCONNECTED)
        // Under the 10 s wait it would otherwise sit out, with room for a slow box.
        assertTrue(System.currentTimeMillis() - t0 < 9_000)
        assertTrue(fake!!.registered.isEmpty())
        assertFalse("start" in fake!!.events)
    }

    // A fetch already under way when START comes: the start waits for that one and goes by it.
    @Test
    fun startWaitsForTheFetchAlreadyRunning() {
        TunnelVpnService.VERSION_FETCH_WAIT_MS = 300
        val svc = controller.get()
        fake = FakeBackend().apply { blockConfig = true; config = RcValues("", 0, 0, "") }
        svc.backend = fake!!
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 7 * 86400))
        rc(min = newer)
        // The first start's fetch hangs past its wait and stays running.
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START), 0, 1)
        awaitState(VpnState.ERROR)

        TunnelVpnService.VERSION_FETCH_WAIT_MS = 10_000
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START), 0, 2)
        Thread.sleep(300)
        assertEquals(VpnState.CONNECTING, TunnelState.state)
        fake!!.release()
        awaitState(VpnState.CONNECTED)
        assertEquals(1, fake!!.fetches.get())
    }

    @Test
    fun snapshotCarriesVersions() {
        rc(min = 3, latest = newer)
        val got = mutableListOf<Versions?>()
        val binder = controller.get().onBind(Intent(TunnelVpnService.ACTION_BIND))!!
        shadowOf(app).setComponentNameAndServiceForBindService(ComponentName(app, TunnelVpnService::class.java), binder)
        VpnClient(app) { _, _, _, _, _, v -> got += v }.bind()
        ShadowLooper.idleMainLooper()
        assertEquals(Versions(3, newer, url), got.last())
    }

    // A fetch that brings a new version tells the bound UI at once, not on the next state change.
    @Test
    fun fetchedVersionsArePushed() {
        val svc = controller.get()
        fake = FakeBackend().apply { config = RcValues("", 0, newer, url) }
        svc.backend = fake!!
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 7 * 86400))
        val got = mutableListOf<Versions?>()
        val binder = svc.onBind(Intent(TunnelVpnService.ACTION_BIND))!!
        shadowOf(app).setComponentNameAndServiceForBindService(ComponentName(app, TunnelVpnService::class.java), binder)
        VpnClient(app) { _, _, _, _, _, v -> got += v }.bind()
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START), 0, 1)
        val deadline = System.currentTimeMillis() + 5_000
        while (got.lastOrNull()?.latest != newer && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper()
        }
        assertEquals(Versions(0, newer, url), got.last())
        awaitState(VpnState.CONNECTED)
    }

    private fun startNotification(): android.app.Notification {
        val svc = controller.get()
        fake = FakeBackend().apply { blockConfig = true }
        svc.backend = fake!!
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START), 0, 1)
        val n = shadowOf(svc).lastForegroundNotification!!
        // No credential: the thread ends in ERROR, and must before the next test.
        awaitState(VpnState.ERROR)
        return n
    }

    @Test
    fun notificationOffersTheUpdate() {
        Updater.debug = false
        rc(latest = newer)
        val n = startNotification()
        assertEquals("Доступно обновление", n.extras.getCharSequence(android.app.Notification.EXTRA_SUB_TEXT).toString())
        assertEquals(listOf("Выключить", "Обновить"), n.actions.map { it.title.toString() })
        val i = shadowOf(n.actions[1].actionIntent).savedIntent
        assertEquals(MainActivity::class.java.name, i.component?.className)
        assertTrue(i.getBooleanExtra(MainActivity.EXTRA_UPDATE, false))
    }

    @Test
    fun notificationStaysQuietInDebug() {
        rc(latest = newer)
        val n = startNotification()
        assertEquals(null, n.extras.getCharSequence(android.app.Notification.EXTRA_SUB_TEXT))
        assertEquals(1, n.actions.size)
    }

    @Test
    fun minVersionIsNotAboutTheTestRelay() {
        val svc = controller.get()
        fake = FakeBackend().apply { blockConfig = true }
        svc.backend = fake!!
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 7 * 86400), test = true)
        rc(min = newer)
        svc.onStartCommand(
            Intent(TunnelVpnService.ACTION_START).putExtra(TunnelVpnService.EXTRA_ENDPOINT, "cover.example.org|203.0.113.10|443|/test"),
            0, 1,
        )
        awaitState(VpnState.CONNECTED)
        assertTrue(vpnPrefs().getBoolean(TunnelVpnService.KEY_WANTED, false))
    }

    // Still connecting when the fetch lands: the notification learns of the update anyway.
    @Test
    fun fetchedUpdateReachesTheNotificationWhileConnecting() {
        Updater.debug = false
        val svc = controller.get()
        fake = FakeBackend().apply { blockStart = true; config = RcValues("", 0, newer, url) }
        svc.backend = fake!!
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 7 * 86400))
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START), 0, 1)
        val nm = shadowOf(app.getSystemService(android.app.NotificationManager::class.java))
        fun sub() = nm.getNotification(1)?.extras?.getCharSequence(android.app.Notification.EXTRA_SUB_TEXT)?.toString()
        val deadline = System.currentTimeMillis() + 5_000
        while (sub() == null && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper()
        }
        assertEquals(VpnState.CONNECTING, TunnelState.state)
        assertEquals("Доступно обновление", sub())
        fake!!.release()
        awaitState(VpnState.CONNECTED)
    }
}
