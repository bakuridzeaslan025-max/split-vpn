package org.duckdns.splitvpn

import android.content.ComponentName
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import android.view.View
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.TextView
import org.junit.After
import org.junit.Assert.assertEquals
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
import org.robolectric.android.controller.ActivityController
import org.robolectric.android.controller.ServiceController
import org.robolectric.shadows.ShadowResolveInfo
import org.robolectric.util.ReflectionHelpers

@RunWith(RobolectricTestRunner::class)
class KeepAliveTabTest {

    private val app get() = RuntimeEnvironment.getApplication()
    private lateinit var service: ServiceController<TunnelVpnService>
    private var activity: ActivityController<MainActivity>? = null
    private val manufacturer = Build.MANUFACTURER

    @Before
    fun setUp() {
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
        activity?.destroy()
        service.destroy()
        ReflectionHelpers.setStaticField(Build::class.java, "MANUFACTURER", manufacturer)
    }

    private fun open(): MainActivity =
        Robolectric.buildActivity(MainActivity::class.java).setup().also { activity = it }.get()

    private fun MainActivity.stepRows(): List<LinearLayout> {
        val list = findViewById<LinearLayout>(R.id.vendorSteps)
        return (0 until list.childCount).map { list.getChildAt(it) as LinearLayout }
    }

    private fun MainActivity.steps(): List<String> = stepRows().map { row ->
        (row.getChildAt(0) as TextView).text.toString() + " " + ((row.getChildAt(1) as LinearLayout).getChildAt(0) as TextView).text.toString()
    }

    /** The step's link, or null. */
    private fun MainActivity.link(step: Int): TextView? =
        ((stepRows()[step].getChildAt(1) as LinearLayout).getChildAt(1) as TextView?)

    private val autostart = ComponentName("com.miui.securitycenter", "com.miui.permcenter.autostart.AutoStartManagementActivity")
    private val power = ComponentName("com.miui.securitycenter", "com.miui.powercenter.legacypowerrank.PowerDetailActivity")

    private val hidden = ComponentName("com.miui.powerkeeper", "com.miui.powerkeeper.ui.HiddenAppsConfigActivity")

    private fun installed(vararg pages: ComponentName, exported: Boolean = true) = pages.forEach { cn ->
        shadowOf(app.packageManager).addOrUpdateActivity(
            android.content.pm.ActivityInfo().apply {
                packageName = cn.packageName
                name = cn.className
                this.exported = exported
                applicationInfo = android.content.pm.ApplicationInfo().apply { packageName = cn.packageName }
            }
        )
    }

    @Test
    fun noFirmwareStepOnAStockPhone() {
        ReflectionHelpers.setStaticField(Build::class.java, "MANUFACTURER", "Google")
        assertEquals(View.GONE, open().findViewById<View>(R.id.vendorBlock).visibility)
    }

    // The instruction goes by the maker, the title by the name on the phone.
    @Test
    fun firmwareStepNamesTheBrandAndNumbersItsSteps() {
        ReflectionHelpers.setStaticField(Build::class.java, "MANUFACTURER", "Xiaomi")
        val brand = Build.BRAND
        ReflectionHelpers.setStaticField(Build::class.java, "BRAND", "Redmi")
        try {
            val activity = open()
            assertEquals(View.VISIBLE, activity.findViewById<View>(R.id.vendorBlock).visibility)
            assertEquals("Настройки Redmi", activity.findViewById<TextView>(R.id.vendorTitle).text.toString())
            val steps = activity.steps()
            assertEquals(3, steps.size)
            assertEquals("1. Автозапуск → включить для Split VPN", steps[0])
            assertTrue(steps[2].startsWith("3. "))
        } finally {
            ReflectionHelpers.setStaticField(Build::class.java, "BRAND", brand)
        }
    }

    @Test
    fun batteryWarnsWhileOptimizationIsOn() {
        val activity = open()
        assertEquals(View.VISIBLE, activity.findViewById<View>(R.id.batteryWarn).visibility)
        assertEquals(View.GONE, activity.findViewById<View>(R.id.batteryOk).visibility)
        activity.findViewById<View>(R.id.batteryButton).performClick()
        val started = shadowOf(activity).nextStartedActivity
        assertEquals(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, started?.action)
        assertEquals("package:${app.packageName}", started?.dataString)
        // Android's own dialog, not HyperOS's battery page that answers the bare request.
        assertEquals(
            ComponentName("com.android.settings", "com.android.settings.fuelgauge.RequestIgnoreBatteryOptimizations"),
            started?.component,
        )
    }

    // The link sits under the step it opens the page for; the step done by hand has none.
    @Test
    fun xiaomiStepsLinkAutostartAndItsOwnBatteryPage() {
        ReflectionHelpers.setStaticField(Build::class.java, "MANUFACTURER", "Xiaomi")
        installed(autostart, power)
        val activity = open()
        assertEquals("Открыть автозапуск\u2060\uFFFC", activity.link(0)!!.text.toString())
        assertEquals("Открыть автозапуск, откроются настройки телефона", activity.link(0)!!.contentDescription)
        assertNull(activity.link(2))
        activity.link(0)!!.performClick()
        assertEquals(autostart, shadowOf(activity).nextStartedActivity?.component)
        activity.link(1)!!.performClick()
        val started = shadowOf(activity).nextStartedActivity
        assertEquals(power, started?.component)
        assertEquals(app.packageName, started?.getStringExtra("package_name"))
    }

    // A firmware without the page: no link rather than one that opens something else.
    @Test
    fun linkHiddenWhenThePhoneLacksThePage() {
        ReflectionHelpers.setStaticField(Build::class.java, "MANUFACTURER", "Xiaomi")
        installed(power)
        val activity = open()
        assertNull(activity.link(0))
        assertEquals("Открыть экономию энергии\u2060\uFFFC", activity.link(1)!!.text.toString())
    }

    // A tap anywhere on the step reaches its link: 48dp without padding the link.
    @Test
    fun theWholeStepTakesTheTap() {
        ReflectionHelpers.setStaticField(Build::class.java, "MANUFACTURER", "Xiaomi")
        installed(autostart, power)
        val activity = open()
        activity.settingsTab().performClick()
        org.robolectric.shadows.ShadowLooper.idleMainLooper()
        val row = activity.stepRows()[0]
        assertNotNull(row.touchDelegate)
        assertNull(activity.stepRows()[2].touchDelegate)
        // On the step's text, well clear of the link.
        val x = row.width - 2f
        val y = 2f
        val t = android.os.SystemClock.uptimeMillis()
        row.dispatchTouchEvent(android.view.MotionEvent.obtain(t, t, android.view.MotionEvent.ACTION_DOWN, x, y, 0))
        row.dispatchTouchEvent(android.view.MotionEvent.obtain(t, t + 50, android.view.MotionEvent.ACTION_UP, x, y, 0))
        org.robolectric.shadows.ShadowLooper.idleMainLooper()
        assertEquals(autostart, shadowOf(activity).nextStartedActivity?.component)
    }

    // Resolving finds a non-exported activity too; starting it would throw.
    @Test
    fun nonExportedPageHidesTheLink() {
        ReflectionHelpers.setStaticField(Build::class.java, "MANUFACTURER", "Xiaomi")
        installed(autostart, exported = false)
        assertNull(open().link(0))
    }

    @Test
    fun theNextPageWhenTheFirstIsMissing() {
        ReflectionHelpers.setStaticField(Build::class.java, "MANUFACTURER", "Xiaomi")
        installed(hidden)
        val activity = open()
        activity.link(1)!!.performClick()
        val started = shadowOf(activity).nextStartedActivity
        assertEquals(hidden, started?.component)
        assertEquals(app.packageName, started?.getStringExtra("package_name"))
    }

    // Not a MIUI power page: a bare component, or Android 13+ may drop it for matching no filter.
    @Test
    fun autostartGetsABareIntent() {
        ReflectionHelpers.setStaticField(Build::class.java, "MANUFACTURER", "Xiaomi")
        installed(autostart)
        val activity = open()
        activity.link(0)!!.performClick()
        val started = shadowOf(activity).nextStartedActivity
        assertNull(started?.action)
        assertNull(started?.data)
    }

    @Test
    fun batteryFallsBackToTheRequestWithoutTheStockDialog() {
        shadowOf(app).checkActivities(true)
        val request = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:${app.packageName}"))
        shadowOf(app.packageManager).addResolveInfoForIntent(
            request, ShadowResolveInfo.newResolveInfo("Battery", "com.miui.securitycenter", "PowerDetailActivity")
        )
        val activity = open()
        activity.findViewById<View>(R.id.batteryButton).performClick()
        val started = shadowOf(activity).nextStartedActivity
        assertEquals(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, started?.action)
        assertNull(started?.component)
    }

    @Test
    fun samsungLinksTheAppsOwnPage() {
        ReflectionHelpers.setStaticField(Build::class.java, "MANUFACTURER", "samsung")
        val activity = open()
        assertEquals(2, activity.steps().size)
        assertNull(activity.link(1))
        activity.link(0)!!.performClick()
        val started = shadowOf(activity).nextStartedActivity
        assertEquals(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, started?.action)
        assertEquals("package:${app.packageName}", started?.dataString)
    }

    @Test
    fun batteryGoesQuietOnceWhitelisted() {
        shadowOf(app.getSystemService(PowerManager::class.java))
            .setIgnoringBatteryOptimizations(app.packageName, true)
        val activity = open()
        assertEquals(View.GONE, activity.findViewById<View>(R.id.batteryWarn).visibility)
        assertEquals(View.VISIBLE, activity.findViewById<View>(R.id.batteryOk).visibility)
        assertEquals("Оптимизация батареи отключена", activity.findViewById<TextView>(R.id.batteryStatus).text.toString())
    }

    private fun MainActivity.settingsTab(): LinearLayout =
        findViewById<LinearLayout>(R.id.tabBar).getChildAt(1) as LinearLayout

    // The dot: only the battery, the firmware step cannot be checked.
    @Test
    fun settingsTabHasABadgeWhileTheBatteryIsOptimized() {
        ReflectionHelpers.setStaticField(Build::class.java, "MANUFACTURER", "Xiaomi")
        val activity = open()
        assertEquals(2, (activity.settingsTab().getChildAt(0) as FrameLayout).childCount)
        shadowOf(app.getSystemService(PowerManager::class.java))
            .setIgnoringBatteryOptimizations(app.packageName, true)
        this.activity!!.pause().resume()
        assertEquals(1, (activity.settingsTab().getChildAt(0) as FrameLayout).childCount)
    }
}
