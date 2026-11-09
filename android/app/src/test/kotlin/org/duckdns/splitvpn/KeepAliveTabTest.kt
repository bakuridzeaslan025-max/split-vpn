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

    // Б3: vendor activity missing (or hidden by package visibility) must land
    // on the app's own settings page instead of doing nothing.
    @Test
    fun vendorButtonFallsBackToAppDetails() {
        ReflectionHelpers.setStaticField(Build::class.java, "MANUFACTURER", "Xiaomi")
        shadowOf(app).checkActivities(true)
        val details = Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:${app.packageName}"))
        shadowOf(app.packageManager).addResolveInfoForIntent(
            details, ShadowResolveInfo.newResolveInfo("Settings", "com.android.settings", "AppDetails")
        )
        val activity = open()
        activity.findViewById<View>(R.id.vendorButton).performClick()
        val started = shadowOf(activity).nextStartedActivity
        assertEquals(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, started?.action)
        assertEquals("package:${app.packageName}", started?.dataString)
    }

    private fun MainActivity.steps(): List<String> {
        val list = findViewById<LinearLayout>(R.id.vendorSteps)
        return (0 until list.childCount).map { i ->
            val row = list.getChildAt(i) as LinearLayout
            (row.getChildAt(0) as TextView).text.toString() + " " + (row.getChildAt(1) as TextView).text.toString()
        }
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
        assertEquals(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, shadowOf(activity).nextStartedActivity?.action)
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
