package org.duckdns.splitvpn

import android.app.NotificationManager
import android.net.VpnService
import android.view.View
import android.widget.TextView
import androidx.test.core.app.ActivityScenario
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.uiautomator.By
import androidx.test.uiautomator.Until
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith

/** Another VPN took the permission: a background start gives up, says so in the shade, and the app's own start takes it back. */
@RunWith(AndroidJUnit4::class)
class VpnTakenTest {
    private val vpn = VpnHarness()
    private val nm = vpn.ctx.getSystemService(NotificationManager::class.java)

    private fun appops(mode: String) = vpn.device.executeShellCommand("appops set ${vpn.ctx.packageName} ACTIVATE_VPN $mode")

    // The same uid as :vpn, so its notifications are ours to see.
    private fun permNotification() = nm.activeNotifications.firstOrNull { it.id == TunnelVpnService.PERM_NOTIFICATION_ID }

    private fun wanted() = vpn.device.executeShellCommand("run-as ${vpn.ctx.packageName} cat shared_prefs/${TunnelVpnService.PREFS}.xml")
        .contains("name=\"${TunnelVpnService.KEY_WANTED}\" value=\"true\"")

    // apply() reaches the file a moment later, and from the other process.
    private fun awaitNotWanted() {
        val deadline = System.currentTimeMillis() + 5_000
        while (wanted() && System.currentTimeMillis() < deadline) Thread.sleep(100)
        assertEquals("wanted", false, wanted())
    }

    @Before
    fun setUp() {
        vpn.requireStand()
        appops("allow")
        vpn.consent()
        vpn.bind()
        if (vpn.state != VpnState.DISCONNECTED) { vpn.stop(); vpn.awaitStopped() }
    }

    @After
    fun tearDown() {
        appops("allow")
        vpn.stop()
        vpn.awaitStopped()
        vpn.unbind()
        awaitNotWanted()
    }

    @Test
    fun backgroundStartGivesUpAndUiStartTakesItBack() {
        appops("deny")
        assertNotNull("prepare still null after deny", VpnService.prepare(vpn.ctx))

        vpn.start(fromUi = false)
        vpn.awaitState(VpnState.ERROR)
        assertEquals(TunnelVpnService.ERR_VPN_TAKEN, vpn.error)
        awaitNotWanted()

        val deadline = System.currentTimeMillis() + 5_000
        while (permNotification() == null && System.currentTimeMillis() < deadline) Thread.sleep(100)
        val n = permNotification()
        assertNotNull("no notification ${TunnelVpnService.PERM_NOTIFICATION_ID}: ${nm.activeNotifications.map { it.id }}", n)
        assertEquals("Split VPN выключен", n!!.notification.extras.getCharSequence("android.title")?.toString())
        assertEquals(TunnelVpnService.ERR_VPN_TAKEN, n.notification.extras.getCharSequence("android.text")?.toString())

        ActivityScenario.launch(MainActivity::class.java).use { scenario ->
            val shown = "Разрешение VPN занято другим приложением. Нажмите «Подключить», чтобы вернуть его"
            assertTrue("no '$shown' on screen", vpn.device.wait(Until.hasObject(By.text(shown)), 10_000))
            scenario.onActivity { a ->
                assertEquals(View.VISIBLE, a.findViewById<View>(R.id.errorBox).visibility)
                assertEquals(shown, a.findViewById<TextView>(R.id.errorText).text.toString())
                assertEquals(View.GONE, a.findViewById<View>(R.id.errorAction).visibility)
            }
        }

        appops("allow")
        assertNull("prepare not null after allow", VpnService.prepare(vpn.ctx))
        vpn.start("telegram")
        vpn.awaitState(VpnState.CONNECTED)
        assertNull("notification ${TunnelVpnService.PERM_NOTIFICATION_ID} left after a connect", permNotification())
        vpn.stop()
        vpn.awaitStopped()
    }
}
