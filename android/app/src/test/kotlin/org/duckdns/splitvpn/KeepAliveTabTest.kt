package org.duckdns.splitvpn

import android.content.ComponentName
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import android.view.View
import android.widget.TextView
import org.junit.After
import org.junit.Assert.assertEquals
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

    @Test
    fun batteryBlockAsksWhileOptimizationIsOn() {
        val activity = open()
        assertEquals("Оптимизация батареи включена", activity.findViewById<TextView>(R.id.batteryStatus).text.toString())
        assertEquals(View.VISIBLE, activity.findViewById<View>(R.id.batteryButton).visibility)
    }

    @Test
    fun batteryBlockGoesQuietOnceWhitelisted() {
        shadowOf(app.getSystemService(PowerManager::class.java))
            .setIgnoringBatteryOptimizations(app.packageName, true)
        val activity = open()
        assertEquals("✓ Оптимизация батареи отключена", activity.findViewById<TextView>(R.id.batteryStatus).text.toString())
        assertEquals(View.GONE, activity.findViewById<View>(R.id.batteryButton).visibility)
    }
}
