package org.duckdns.splitvpn

import android.app.NotificationManager
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.view.View
import android.widget.TextView
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
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
import java.util.concurrent.TimeUnit

/** The daily limit on relay traffic: the VPN goes off once it is used up. */
@RunWith(RobolectricTestRunner::class)
class QuotaTest {

    private lateinit var controller: ServiceController<TunnelVpnService>
    private val app get() = RuntimeEnvironment.getApplication()
    private val svc get() = controller.get()
    private val be = FakeBackend()
    private val vpnPrefs get() = app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)
    private val mb = 1024L * 1024
    private var day = 20_000L

    @Before
    fun setUp() {
        Quota.today = { day }
        TunnelState.set(VpnState.DISCONNECTED)
        TunnelState.clearLog()
        ShadowLooper.idleMainLooper()
        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
        svc.backend = be
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400))
        ShadowLooper.idleMainLooper()
    }

    @After
    fun tearDown() {
        Quota.today = { java.time.LocalDate.now().toEpochDay() }
        be.release()
        awaitServiceThreads()
        controller.destroy()
        ShadowLooper.idleMainLooper()
    }

    private fun start() {
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START).putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram")), 0, 1)
        awaitState(VpnState.CONNECTED, VpnState.ERROR)
    }

    private fun awaitState(vararg want: VpnState) {
        val deadline = System.currentTimeMillis() + 10_000
        while (TunnelState.state !in want && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, TimeUnit.MILLISECONDS)
        }
        assertTrue("state=${TunnelState.state} err=${TunnelState.lastError}", TunnelState.state in want)
        ShadowLooper.idleMainLooper()
    }

    private fun quotaNotification() = shadowOf(app.getSystemService(NotificationManager::class.java)).allNotifications
        .firstOrNull { it.extras.getString("android.title") == TunnelVpnService.ERR_QUOTA }

    @Test
    fun defaultLimitIsTwoGigabytesAndRcCanChangeOrLiftIt() {
        assertEquals(2048 * mb, Quota.limit(app))
        Quota.setLimitMb(app, null)
        assertEquals(2048 * mb, Quota.limit(app))
        Quota.setLimitMb(app, 500)
        assertEquals(500 * mb, Quota.limit(app))
        Quota.setLimitMb(app, 0)
        assertEquals(0, Quota.limit(app))
        assertFalse(Quota.exceeded(Long.MAX_VALUE, 0))
    }

    @Test
    fun startHandsGoTodaysCountAndLimit() {
        Quota.save(app, 300 * mb, day)
        start()
        assertEquals(VpnState.CONNECTED, TunnelState.state)
        assertEquals(listOf(300 * mb to 2048 * mb), be.quotasSet.toList())
    }

    @Test
    fun yesterdaysCountDoesNotCount() {
        Quota.save(app, 5000 * mb, day - 1)
        start()
        assertEquals(VpnState.CONNECTED, TunnelState.state)
        assertEquals(0L, be.quotasSet.single().first)
    }

    @Test
    fun usedUpDayRefusesToStartAndStaysOff() {
        Quota.save(app, 2048 * mb, day)
        start()
        assertEquals(VpnState.ERROR, TunnelState.state)
        assertEquals(TunnelVpnService.ERR_QUOTA, TunnelState.lastError)
        assertFalse("boot must not bring it back", vpnPrefs.getBoolean(TunnelVpnService.KEY_WANTED, true))
        assertTrue(be.quotasSet.isEmpty())
        assertFalse("start" in be.events)
    }

    @Test
    fun goReportingTheLimitStopsTheVpnWithAReason() {
        start()
        be.used = 2048 * mb
        be.startedHost!!.quotaExceeded()
        awaitState(VpnState.ERROR)
        awaitServiceThreads()
        assertEquals(TunnelVpnService.ERR_QUOTA, TunnelState.lastError)
        assertTrue("stop" in be.events)
        assertFalse(vpnPrefs.getBoolean(TunnelVpnService.KEY_WANTED, true))
        assertNotNull("the user must learn why the VPN went off", quotaNotification())
        assertEquals(2048 * mb, Quota.used(app))
        // A tap before midnight is refused on the stored count.
        start()
        assertEquals(TunnelVpnService.ERR_QUOTA, TunnelState.lastError)
    }

    @Test
    fun refusedStartStillAsksRcAndARaisedLimitLiftsTheRefusal() {
        Quota.save(app, 2048 * mb, day)
        be.config = RcValues("", 0, 0, "", dailyQuotaMb = 4096)
        start()
        val deadline = System.currentTimeMillis() + 5_000
        while (TunnelState.state == VpnState.ERROR && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper()
        }
        assertEquals(1, be.fetches.get())
        assertEquals(VpnState.DISCONNECTED, TunnelState.state)
        start()
        assertEquals(VpnState.CONNECTED, TunnelState.state)
    }

    @Test
    fun limitRaisedBeforeTheCallbackKeepsTheVpnOn() {
        start()
        be.used = 2048 * mb
        Quota.setLimitMb(app, 4096)
        be.startedHost!!.quotaExceeded()
        ShadowLooper.idleMainLooper()
        assertEquals(VpnState.CONNECTED, TunnelState.state)
        assertFalse("stop" in be.events)
    }

    @Test
    fun goReportingYesterdaysCountAfterMidnightStartsTheDayAfresh() {
        start()
        be.used = 2048 * mb
        day++
        be.startedHost!!.quotaExceeded()
        ShadowLooper.idleMainLooper()
        assertEquals(VpnState.CONNECTED, TunnelState.state)
        assertEquals(0L to 2048 * mb, be.quotasSet.last())
        assertEquals(0L, Quota.used(app))
    }

    @Test
    fun nextDayStartsAndClearsTheNotification() {
        start()
        be.used = 2048 * mb
        be.startedHost!!.quotaExceeded()
        awaitState(VpnState.ERROR)
        awaitServiceThreads()
        day++
        start()
        assertEquals(VpnState.CONNECTED, TunnelState.state)
        assertNull(quotaNotification())
    }

    @Test
    fun countIsSavedOnStopAndCountedOnFromThere() {
        start()
        be.used = 700 * mb
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_STOP), 0, 2)
        awaitServiceThreads()
        assertEquals(700 * mb, Quota.used(app))
    }

    @Test
    fun stopAfterMidnightKeepsTheCountOnItsOwnDay() {
        start()
        be.used = 2048 * mb
        day++
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_STOP), 0, 2)
        awaitServiceThreads()
        assertEquals(0L, Quota.used(app))
    }

    @Test
    fun progressSavesTheCountAndTellsTheUi() {
        start()
        val got = mutableListOf<Usage?>()
        shadowOf(app).setComponentNameAndServiceForBindService(
            ComponentName(app, TunnelVpnService::class.java), svc.onBind(Intent(TunnelVpnService.ACTION_BIND))!!,
        )
        VpnClient(app) { _, _, _, _, _, _, u -> got += u }.bind()
        ShadowLooper.idleMainLooper()
        val before = got.size
        be.used = 64 * mb
        be.startedHost!!.quotaProgress()
        ShadowLooper.idleMainLooper()
        assertEquals(64 * mb, Quota.used(app))
        assertTrue(got.size > before)
        assertEquals(Usage(64 * mb, 2048 * mb, day), got.last())
    }

    @Test
    fun progressAfterMidnightRollsTheDay() {
        start()
        be.used = 100 * mb
        day++
        be.startedHost!!.quotaProgress()
        ShadowLooper.idleMainLooper()
        assertEquals(0L to 2048 * mb, be.quotasSet.last())
        assertEquals(day, Quota.day(app))
        assertEquals(0L, Quota.used(app))
    }

    @Test
    fun idleTunnelDoesNothingByTheClock() {
        start()
        be.used = 100 * mb
        ShadowLooper.idleMainLooper(1, TimeUnit.HOURS)
        assertEquals(0L, Quota.used(app))
        assertEquals(1, be.quotasSet.size)
    }

    @Test
    fun newLimitFromRcReachesTheRunningTunnel() {
        be.config = RcValues("", 0, 0, "", dailyQuotaMb = 100)
        start()
        val deadline = System.currentTimeMillis() + 5_000
        while (be.quotasSet.lastOrNull()?.second != 100 * mb && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper()
        }
        assertEquals(100 * mb, be.quotasSet.last().second)
    }

    @Test
    fun snapshotCarriesTodaysUsage() {
        Quota.save(app, 300 * mb, day)
        val got = mutableListOf<Usage?>()
        shadowOf(app).setComponentNameAndServiceForBindService(
            ComponentName(app, TunnelVpnService::class.java), svc.onBind(Intent(TunnelVpnService.ACTION_BIND))!!,
        )
        VpnClient(app) { _, _, _, _, _, _, u -> got += u }.bind()
        ShadowLooper.idleMainLooper()
        assertEquals(Usage(300 * mb, 2048 * mb, day), got.last())
    }

    @Test
    fun formatsForPeople() {
        assertEquals("0 МБ", Quota.format(0))
        assertEquals("350 МБ", Quota.format(350 * mb))
        assertEquals("1,2 ГБ", Quota.format(1229 * mb))
        assertEquals("2 ГБ", Quota.format(2048 * mb))
    }

    private fun launchUi(): MainActivity {
        shadowOf(app).setComponentNameAndServiceForBindService(
            ComponentName(app, TunnelVpnService::class.java), svc.onBind(Intent(TunnelVpnService.ACTION_BIND))!!,
        )
        val a = Robolectric.buildActivity(MainActivity::class.java).setup().get()
        ShadowLooper.idleMainLooper()
        return a
    }

    private fun MainActivity.assertQuotaOutUi() {
        assertEquals(
            "Сегодня через VPN: 2 ГБ из 2 ГБ, снова после 00:00",
            findViewById<TextView>(R.id.usageText).text.toString(),
        )
        assertEquals(getColor(R.color.simple_blue), findViewById<TextView>(R.id.hintText).currentTextColor)
        assertEquals("a normal state, not a failure", View.GONE, findViewById<View>(R.id.errorBox).visibility)
        assertEquals("Лимит на сегодня исчерпан", findViewById<TextView>(R.id.buttonLabel).text.toString())
        assertFalse(findViewById<View>(R.id.toggleButton).isEnabled)
    }

    private fun MainActivity.assertFreshDayUi() {
        assertEquals("Сегодня через VPN: 0 МБ из 2 ГБ", findViewById<TextView>(R.id.usageText).text.toString())
        assertEquals(View.GONE, findViewById<View>(R.id.errorBox).visibility)
        assertEquals("Подключить", findViewById<TextView>(R.id.buttonLabel).text.toString())
        assertTrue(findViewById<View>(R.id.toggleButton).isEnabled)
        assertEquals(getColor(R.color.simple_blue), findViewById<TextView>(R.id.hintText).currentTextColor)
    }

    @Test
    fun uiShowsUsageAndTheReasonEvenAfterTheServiceIsGone() {
        Quota.save(app, 2048 * mb, day)
        launchUi().assertQuotaOutUi()
    }

    @Test
    fun uiShowsTheSameWhenTheServiceStopsOnTheLimit() {
        Quota.save(app, 2048 * mb, day)
        val a = launchUi()
        TunnelState.set(VpnState.ERROR, TunnelVpnService.ERR_QUOTA)
        ShadowLooper.idleMainLooper()
        a.assertQuotaOutUi()
    }

    @Test
    fun usedUpToggleDoesNothing() {
        Quota.save(app, 2048 * mb, day)
        shadowOf(app).grantPermissions(android.Manifest.permission.POST_NOTIFICATIONS)
        val a = launchUi()
        shadowOf(app).clearStartedServices()
        a.findViewById<View>(R.id.toggleButton).performClick()
        ShadowLooper.idleMainLooper()
        assertNull(shadowOf(app).nextStartedService)
        assertNull(shadowOf(a).nextStartedActivity)
    }

    @Test
    fun usageLineHasNoMidnightNoteBeforeTheLimit() {
        Quota.save(app, 300 * mb, day)
        val a = launchUi()
        assertEquals("Сегодня через VPN: 300 МБ из 2 ГБ", a.findViewById<TextView>(R.id.usageText).text.toString())
        assertEquals("Подключить", a.findViewById<TextView>(R.id.buttonLabel).text.toString())
        assertTrue(a.findViewById<View>(R.id.toggleButton).isEnabled)
    }

    @Test
    fun usedUpDayEndsAtMidnightWithoutANewSnapshot() {
        Quota.save(app, 2048 * mb, day)
        val a = launchUi()
        a.assertQuotaOutUi()
        day++
        ShadowLooper.idleMainLooper(25, TimeUnit.HOURS)
        a.assertFreshDayUi()
    }

    @Test
    fun quotaErrorOfYesterdayIsNoError() {
        Quota.save(app, 2048 * mb, day)
        val a = launchUi()
        TunnelState.set(VpnState.ERROR, TunnelVpnService.ERR_QUOTA)
        ShadowLooper.idleMainLooper()
        a.assertQuotaOutUi()
        day++
        ShadowLooper.idleMainLooper(25, TimeUnit.HOURS)
        a.assertFreshDayUi()
    }

    @Test
    fun requiredUpdateShowsOverAUsedUpDay() {
        Quota.save(app, 2048 * mb, day)
        vpnPrefs.edit().putLong(RemoteConfig.KEY_MIN_VERSION, Long.MAX_VALUE).commit()
        val a = launchUi()
        assertEquals(View.VISIBLE, a.findViewById<View>(R.id.errorBox).visibility)
        assertEquals("Обновить", a.findViewById<TextView>(R.id.errorAction).text.toString())
        assertEquals("Лимит на сегодня исчерпан", a.findViewById<TextView>(R.id.buttonLabel).text.toString())
        assertFalse(a.findViewById<View>(R.id.toggleButton).isEnabled)
        a.findViewById<View>(R.id.errorAction).performClick()
        assertEquals(Intent.ACTION_VIEW, shadowOf(a).nextStartedActivity?.action)
    }

    @Test
    fun uiHidesUsageWithoutALimit() {
        Quota.setLimitMb(app, 0)
        shadowOf(app).setComponentNameAndServiceForBindService(
            ComponentName(app, TunnelVpnService::class.java), svc.onBind(Intent(TunnelVpnService.ACTION_BIND))!!,
        )
        val a = Robolectric.buildActivity(MainActivity::class.java).setup().get()
        ShadowLooper.idleMainLooper()
        assertEquals(View.GONE, a.findViewById<View>(R.id.usageText).visibility)
        assertEquals(View.GONE, a.findViewById<View>(R.id.errorBox).visibility)
    }
}
