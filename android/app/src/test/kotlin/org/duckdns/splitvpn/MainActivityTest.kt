package org.duckdns.splitvpn

import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.provider.Settings
import android.view.View
import android.widget.LinearLayout
import android.widget.TextView
import java.time.Duration
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.Shadows.shadowOf
import org.robolectric.android.controller.ActivityController
import org.robolectric.android.controller.ServiceController
import org.robolectric.shadows.ShadowDialog
import org.robolectric.shadows.ShadowLooper
import org.robolectric.shadows.ShadowSystemClock
import org.robolectric.shadows.ShadowToast

@RunWith(RobolectricTestRunner::class)
class MainActivityTest {

    private val app get() = RuntimeEnvironment.getApplication()
    private lateinit var service: ServiceController<TunnelVpnService>
    private var activity: ActivityController<MainActivity>? = null
    private val prefs get() = app.getSharedPreferences(Services.PREFS, Context.MODE_PRIVATE)

    @Before
    fun setUp() {
        TunnelState.set(VpnState.DISCONNECTED)
        TunnelState.clearLog()
        ShadowLooper.idleMainLooper()
        service = Robolectric.buildService(TunnelVpnService::class.java).create()
        // Without this Robolectric hands ServiceConnection null arguments and
        // VpnClient's non-null parameters blow up on bind.
        shadowOf(app).setComponentNameAndServiceForBindService(
            ComponentName(app, TunnelVpnService::class.java),
            service.get().onBind(Intent(TunnelVpnService.ACTION_BIND))!!,
        )
    }

    @After
    fun tearDown() {
        Updater.debug = true
        MainActivity.update = Updater::update
        MainActivity.background = { Thread(it, "Update").start() }
        activity?.destroy()
        service.destroy()
        ShadowLooper.idleMainLooper()
    }

    private fun launch(theme: String? = null, intent: Intent? = null): MainActivity {
        theme?.let { prefs.edit().putString("theme", it).commit() }
        val controller = Robolectric.buildActivity(MainActivity::class.java, intent).setup()
        activity = controller
        ShadowLooper.idleMainLooper()
        return controller.get()
    }

    private fun connect(vararg log: String) {
        TunnelState.set(VpnState.CONNECTED)
        log.forEach { TunnelState.log(it) }
        ShadowLooper.idleMainLooper()
    }

    private fun MainActivity.row(id: String): View =
        findViewById<LinearLayout>(R.id.serviceList).getChildAt(Services.ALL.indexOfFirst { it.id == id })

    private fun MainActivity.note(id: String) = row(id).findViewById<TextView>(R.id.note).text.toString()

    @Test
    fun pingFromLogShowsOnConnectedService() {
        val a = launch()
        connect("Telegram: 817 мс")
        assertEquals("817 мс", a.note("telegram"))
    }

    @Test
    fun probeFailureShowsError() {
        val a = launch()
        connect("Telegram: ошибка: TimeoutException")
        assertEquals("ошибка", a.note("telegram"))
    }

    @Test
    fun plainLogLinesAreNotPings() {
        val a = launch()
        connect("Туннель поднят", "Маршруты обновлены", "Доступ получен")
        assertEquals("", a.note("telegram"))
    }

    @Test
    fun lastProbeWins() {
        val a = launch()
        connect("Telegram: 817 мс", "Telegram: ошибка: TimeoutException", "Telegram: 42 мс")
        assertEquals("42 мс", a.note("telegram"))
    }

    @Test
    fun pingHiddenWhileDisconnected() {
        val a = launch()
        TunnelState.log("Telegram: 817 мс")
        ShadowLooper.idleMainLooper()
        assertEquals("", a.note("telegram"))
    }

    @Test
    fun pingHiddenForDisabledService() {
        val a = launch()
        connect("YouTube: 817 мс")
        assertEquals("", a.note("youtube"))
    }

    // The rows are labelled with `labels`, but the log carries Service.title;
    // a mismatch would silently drop every ping.
    @Test
    fun everyServiceTitleMatchesItsRow() {
        val a = launch()
        Services.ALL.forEach { prefs.edit().putBoolean(Services.prefKey(it.id), true).commit() }
        connect(*Services.ALL.map { "${it.title}: 100 мс" }.toTypedArray())
        for (svc in Services.ALL) assertEquals(svc.id, "100 мс", a.note(svc.id))
    }

    @Test
    fun needCodeErrorOffersTheDialog() {
        val a = launch()
        TunnelState.set(VpnState.ERROR, TunnelVpnService.ERR_NEED_CODE)
        ShadowLooper.idleMainLooper()
        val action = a.findViewById<TextView>(R.id.errorAction)
        assertEquals(View.VISIBLE, a.findViewById<View>(R.id.errorBox).visibility)
        assertEquals("Ввести код", action.text.toString())
        action.performClick()
        assertNotNull(ShadowDialog.getLatestDialog())
        assertTrue(ShadowDialog.getLatestDialog().isShowing)
    }

    @Test
    fun otherErrorOffersRetryAndShowsTheReason() {
        val a = launch()
        TunnelState.set(VpnState.ERROR, "связь потеряна")
        ShadowLooper.idleMainLooper()
        assertEquals("Повторить", a.findViewById<TextView>(R.id.errorAction).text.toString())
        assertEquals("связь потеряна", a.findViewById<TextView>(R.id.errorText).text.toString())
    }

    @Test
    fun goErrorsAreNotShownRaw() {
        val a = launch()
        TunnelState.set(VpnState.ERROR, "relay unreachable: dial tcp: i/o timeout")
        ShadowLooper.idleMainLooper()
        assertEquals(
            "Сервер недоступен. Проверьте интернет и повторите",
            a.findViewById<TextView>(R.id.errorText).text.toString(),
        )
        TunnelState.set(VpnState.ERROR, "fdbased: bad fd")
        ShadowLooper.idleMainLooper()
        assertEquals("Не удалось подключиться", a.findViewById<TextView>(R.id.errorText).text.toString())
    }

    @Test
    fun errorBoxHiddenWhenNotInError() {
        val a = launch()
        assertEquals(View.GONE, a.findViewById<View>(R.id.errorBox).visibility)
        connect()
        assertEquals(View.GONE, a.findViewById<View>(R.id.errorBox).visibility)
    }

    @Test
    fun defaultThemeIsTheSimpleLayout() {
        val a = launch()
        assertNotNull(a.findViewById<View>(R.id.titleText))
    }

    @Test
    fun lightThemeIsTheSimpleLayout() {
        val a = launch("light")
        assertNotNull(a.findViewById<View>(R.id.titleText))
    }

    @Test
    fun removedTermThemeFallsBackToSimpleLayout() {
        val a = launch("term-green")
        assertNotNull(a.findViewById<View>(R.id.titleText))
    }

    @Test
    fun rowTogglesWhileDisconnected() {
        val a = launch()
        a.row("telegram").performClick()
        assertEquals(false, prefs.getBoolean(Services.prefKey("telegram"), true))
        a.row("youtube").performClick()
        assertEquals(true, prefs.getBoolean(Services.prefKey("youtube"), false))
    }

    @Test
    fun rowIsLockedWhileConnected() {
        val a = launch()
        connect()
        a.row("telegram").performClick()
        assertEquals(true, prefs.getBoolean(Services.prefKey("telegram"), true))
        a.row("youtube").performClick()
        assertEquals(false, prefs.getBoolean(Services.prefKey("youtube"), false))
    }

    @Test
    fun lockedRowTapsDoNotQueueToasts() {
        val a = launch()
        connect()
        repeat(5) { a.row("telegram").performClick() }
        assertEquals(1, ShadowToast.shownToastCount())
        ShadowSystemClock.advanceBy(Duration.ofSeconds(3))
        a.row("telegram").performClick()
        assertEquals(2, ShadowToast.shownToastCount())
    }

    // Expired credential, no network: the service waits by itself, so the
    // toggle reads Stop and the list is locked, as when connected.
    @Test
    fun offlineRetryLooksLikeWaiting() {
        val a = launch()
        TunnelState.set(VpnState.ERROR, TunnelVpnService.ERR_OFFLINE)
        ShadowLooper.idleMainLooper()
        assertEquals(View.GONE, a.findViewById<View>(R.id.errorAction).visibility)
        assertEquals("Нет сети", a.findViewById<TextView>(R.id.statusText).text.toString())
        assertEquals("Выключить", a.findViewById<TextView>(R.id.buttonLabel).text.toString())
        shadowOf(app).clearStartedServices()
        a.findViewById<View>(R.id.toggleButton).performClick()
        assertEquals(TunnelVpnService.ACTION_STOP, shadowOf(app).nextStartedService?.action)
    }

    @Test
    fun waitingIsACalmBannerWithoutAction() {
        val a = launch()
        TunnelState.set(VpnState.CONNECTED)
        TunnelState.setWaiting(Waiting.NO_SERVER)
        ShadowLooper.idleMainLooper()
        assertEquals(View.VISIBLE, a.findViewById<View>(R.id.errorBox).visibility)
        assertEquals(View.GONE, a.findViewById<View>(R.id.errorAction).visibility)
        assertTrue(a.findViewById<TextView>(R.id.errorText).text.contains("ничего делать не нужно"))
        assertEquals("Сервер недоступен", a.findViewById<TextView>(R.id.statusText).text.toString())
        assertEquals("Выключить", a.findViewById<TextView>(R.id.buttonLabel).text.toString())

        TunnelState.setWaiting(null)
        ShadowLooper.idleMainLooper()
        assertEquals(View.GONE, a.findViewById<View>(R.id.errorBox).visibility)
        TunnelState.set(VpnState.ERROR, "связь потеряна")
        ShadowLooper.idleMainLooper()
        assertEquals(View.VISIBLE, a.findViewById<View>(R.id.errorAction).visibility)
    }

    private val url = Updater.RELEASES + "v9.9.9/split-vpn.apk"
    private val calls = mutableListOf<Pair<Long, String>>()

    private fun updater(vararg results: Updater.Result) {
        val queue = ArrayDeque(results.toList())
        MainActivity.update = { _, latest, u -> calls += latest to u; queue.removeFirst() }
        MainActivity.background = { it.run() }
    }

    /** What :vpn has from Remote Config; any state change pushes a fresh snapshot. */
    private fun rc(min: Long = 0, latest: Long = 0, url: String = this.url) {
        app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE).edit()
            .putLong(RemoteConfig.KEY_MIN_VERSION, min)
            .putLong(RemoteConfig.KEY_LATEST_VERSION, latest)
            .putString(RemoteConfig.KEY_UPDATE_URL, url)
            .commit()
        TunnelState.log("rc")
        ShadowLooper.idleMainLooper()
    }

    private val newer = BuildConfig.VERSION_CODE + 1L
    private fun MainActivity.updateBox() = findViewById<View>(R.id.updateBox).visibility
    private fun MainActivity.text(id: Int) = findViewById<TextView>(id).text.toString()
    private fun nextActivity() = shadowOf(app).nextStartedActivity

    @Test
    fun requiredUpdateIsAnErrorWithUpdateButton() {
        updater(Updater.Result.DISABLED)
        val a = launch()
        rc(min = newer, latest = newer)
        TunnelState.set(VpnState.ERROR, TunnelVpnService.ERR_UPDATE_REQUIRED)
        ShadowLooper.idleMainLooper()
        assertEquals(View.VISIBLE, a.findViewById<View>(R.id.errorBox).visibility)
        assertEquals("Эта версия больше не поддерживается — обновите приложение", a.text(R.id.errorText))
        assertEquals("Обновить", a.text(R.id.errorAction))
        a.findViewById<View>(R.id.errorAction).performClick()
        assertEquals(listOf(newer to url), calls)
        // Debug: Updater refuses, the button still leads somewhere.
        val i = nextActivity()
        assertEquals(Intent.ACTION_VIEW, i.action)
        assertEquals(Updater.RELEASES_PAGE, i.dataString)
    }

    @Test
    fun softBannerNamesTheVersion() {
        Updater.debug = false
        val a = launch()
        assertEquals(View.GONE, a.updateBox())
        rc(latest = newer)
        assertEquals(View.VISIBLE, a.updateBox())
        assertEquals("Доступна версия 9.9.9", a.text(R.id.updateText))
        assertEquals("Обновить", a.text(R.id.updateAction))
        rc(latest = newer, url = Updater.RELEASES + "latest/split-vpn.apk")
        assertEquals("Доступна новая версия", a.text(R.id.updateText))
    }

    @Test
    fun softBannerHiddenInDebugAndWhenCurrent() {
        val a = launch()
        rc(latest = newer)
        assertEquals(View.GONE, a.updateBox())
        Updater.debug = false
        rc(latest = BuildConfig.VERSION_CODE.toLong())
        assertEquals(View.GONE, a.updateBox())
    }

    @Test
    fun closedBannerStaysClosedUntilTheNextVersion() {
        Updater.debug = false
        rc(latest = newer)
        var a = launch()
        assertEquals(View.VISIBLE, a.updateBox())
        a.findViewById<View>(R.id.updateClose).performClick()
        assertEquals(View.GONE, a.updateBox())
        activity?.destroy()
        a = launch()
        assertEquals(View.GONE, a.updateBox())
        rc(latest = newer + 1)
        assertEquals(View.VISIBLE, a.updateBox())
    }

    @Test
    fun updateShowsProgressAndStartedLeavesTheRestToTheSystem() {
        Updater.debug = false
        var job: Runnable? = null
        MainActivity.update = { _, _, _ -> Updater.Result.STARTED }
        MainActivity.background = { job = it }
        val a = launch()
        rc(latest = newer)
        a.findViewById<View>(R.id.updateAction).performClick()
        assertEquals("Скачивается…", a.text(R.id.updateAction))
        job!!.run()
        assertEquals("Обновить", a.text(R.id.updateAction))
        assertEquals(null, nextActivity())
    }

    @Test
    fun needPermissionAsksAndRetriesOnReturn() {
        Updater.debug = false
        updater(Updater.Result.NEED_PERMISSION, Updater.Result.STARTED)
        val a = launch()
        rc(latest = newer)
        a.findViewById<View>(R.id.updateAction).performClick()
        val ask = nextActivity()
        assertEquals(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES, ask.action)
        shadowOf(app.packageManager).setCanRequestPackageInstalls(true)
        shadowOf(a).receiveResult(ask, android.app.Activity.RESULT_CANCELED, null)
        assertEquals(2, calls.size)
    }

    @Test
    fun deniedPermissionDoesNotLoop() {
        Updater.debug = false
        updater(Updater.Result.NEED_PERMISSION)
        val a = launch()
        rc(latest = newer)
        a.findViewById<View>(R.id.updateAction).performClick()
        shadowOf(app.packageManager).setCanRequestPackageInstalls(false)
        shadowOf(a).receiveResult(nextActivity(), android.app.Activity.RESULT_CANCELED, null)
        assertEquals(1, calls.size)
    }

    @Test
    fun failedDownloadOffersTheBrowser() {
        Updater.debug = false
        updater(Updater.Result.FAILED)
        val a = launch()
        rc(latest = newer)
        a.findViewById<View>(R.id.updateAction).performClick()
        assertEquals("Не удалось скачать", ShadowToast.getTextOfLatestToast())
        val i = nextActivity()
        assertEquals(Intent.ACTION_VIEW, i.action)
        assertEquals(url, i.dataString)
    }

    @Test
    fun badUrlOpensTheReleasesPageNotTheUrl() {
        Updater.debug = false
        updater(Updater.Result.BAD_URL)
        val a = launch()
        rc(latest = newer)
        a.findViewById<View>(R.id.updateAction).performClick()
        assertEquals(Updater.RELEASES_PAGE, nextActivity().dataString)
    }

    @Test
    fun upToDateHidesAndBusyIsIgnored() {
        Updater.debug = false
        updater(Updater.Result.BUSY, Updater.Result.UP_TO_DATE)
        val a = launch()
        rc(latest = newer)
        a.findViewById<View>(R.id.updateAction).performClick()
        assertEquals(null, nextActivity())
        assertEquals(View.VISIBLE, a.updateBox())
        assertEquals("Обновить", a.text(R.id.updateAction))
        a.findViewById<View>(R.id.updateAction).performClick()
        assertEquals(View.GONE, a.updateBox())
    }

    @Test
    fun notificationActionUpdatesOnceTheSnapshotArrives() {
        Updater.debug = false
        updater(Updater.Result.STARTED)
        app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE).edit()
            .putLong(RemoteConfig.KEY_LATEST_VERSION, newer).putString(RemoteConfig.KEY_UPDATE_URL, url).commit()
        launch(intent = Intent(app, MainActivity::class.java).putExtra(MainActivity.EXTRA_UPDATE, true))
        assertEquals(listOf(newer to url), calls)
    }

    // min_version raised, latest_version not: nothing to download, the page still helps.
    @Test
    fun requiredButUpToDateOpensTheReleasesPage() {
        Updater.debug = false
        updater(Updater.Result.UP_TO_DATE)
        val a = launch()
        rc(min = newer, latest = BuildConfig.VERSION_CODE.toLong())
        a.findViewById<View>(R.id.errorAction).performClick()
        assertEquals(Updater.RELEASES_PAGE, nextActivity().dataString)
        assertEquals(0L, prefs.getLong("update_dismissed", 0))
    }

    @Test
    fun requiredShowsBeforeAnyTryToConnect() {
        updater(Updater.Result.DISABLED)
        val a = launch()
        assertEquals(View.GONE, a.findViewById<View>(R.id.errorBox).visibility)
        rc(min = newer, latest = newer)
        assertEquals(VpnState.DISCONNECTED, TunnelState.state)
        assertEquals(View.VISIBLE, a.findViewById<View>(R.id.errorBox).visibility)
        assertEquals(TunnelVpnService.ERR_UPDATE_REQUIRED, a.text(R.id.errorText))
        assertEquals("Обновить", a.text(R.id.errorAction))
        a.findViewById<View>(R.id.errorAction).performClick()
        assertEquals(1, calls.size)
        assertEquals(View.GONE, a.updateBox())
    }
}
