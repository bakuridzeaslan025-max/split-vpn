package org.duckdns.splitvpn

import android.os.SystemClock
import android.view.accessibility.AccessibilityEvent
import android.widget.LinearLayout
import androidx.test.core.app.ActivityScenario
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.uiautomator.By
import androidx.test.uiautomator.Until
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import java.util.concurrent.CopyOnWriteArrayList

/**
 * Tapping a service row while the tunnel is up: one hint, not a queue of
 * toasts that keeps popping up long after the taps.
 */
@RunWith(AndroidJUnit4::class)
class LockedRowTest {
    private val vpn = VpnHarness()

    @Before
    fun setUp() {
        vpn.requireStand()
        vpn.consent()
        vpn.bind()
        if (vpn.state != VpnState.DISCONNECTED) { vpn.stop(); vpn.awaitStopped() }
    }

    @After
    fun tearDown() {
        vpn.instr.uiAutomation.setOnAccessibilityEventListener(null)
        vpn.stop()
        vpn.awaitState(VpnState.DISCONNECTED)
        vpn.unbind()
    }

    @Test
    fun rapidTapsShowOneToast() {
        vpn.start("telegram")
        vpn.awaitState(VpnState.CONNECTED)
        ActivityScenario.launch(MainActivity::class.java).use { scenario ->
            assertNotNull("no Telegram row", vpn.device.wait(Until.findObject(By.text("Telegram")), 10_000))
            // A toast announces itself when it is actually shown, so queued
            // ones arrive one by one over the following seconds.
            val t0 = SystemClock.uptimeMillis()
            val toasts = CopyOnWriteArrayList<String>()
            vpn.instr.uiAutomation.setOnAccessibilityEventListener { e ->
                if (e.eventType == AccessibilityEvent.TYPE_NOTIFICATION_STATE_CHANGED &&
                    e.text.any { it.contains("выключите VPN") }
                ) toasts.add("${e.eventTime - t0}ms ${e.packageName}")
            }
            // Injected taps cost ~0.5 s each, longer than a user's burst.
            scenario.onActivity { a ->
                val list = a.findViewById<LinearLayout>(R.id.serviceList)
                val row = list.getChildAt(Services.ALL.indexOfFirst { it.id == "telegram" })
                repeat(5) { row.performClick() }
            }
            // Three short toasts back to back would take more than this.
            Thread.sleep(8_000)
            assertEquals("toasts $toasts", 1, toasts.size)
        }
    }
}
