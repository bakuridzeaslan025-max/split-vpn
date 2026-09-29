package org.duckdns.splitvpn

import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.After
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith

/**
 * Real device/emulator + real VDS: consent → START → CONNECTED → probes →
 * traffic through the TUN → STOP → DISCONNECTED, nothing left behind.
 *
 *   ANDROID_SERIAL=emulator-5554 ./gradlew connectedDebugAndroidTest
 */
@RunWith(AndroidJUnit4::class)
class MainFlowTest {
    private val vpn = VpnHarness()

    @Before
    fun setUp() {
        vpn.consent()
        vpn.bind()
        // Leftover tunnel from a previous run.
        if (vpn.state != VpnState.DISCONNECTED) { vpn.stop(); vpn.awaitStopped() }
    }

    @After
    fun tearDown() {
        vpn.stop()
        vpn.awaitState(VpnState.DISCONNECTED)
        vpn.unbind()
    }

    @Test
    fun connectProbeAndDisconnect() {
        vpn.start("telegram", "youtube")
        vpn.awaitState(VpnState.CONNECTED)
        assertTrue("no TUN interface", vpn.tunUp())

        val tg = vpn.awaitLog("Telegram:")
        val yt = vpn.awaitLog("YouTube:")
        assertFalse(tg, tg.contains("ошибка"))
        assertFalse(yt, yt.contains("ошибка"))

        // Our own request takes the same path as the user's apps.
        vpn.head("https://web.telegram.org/")
        vpn.head("https://www.youtube.com/")

        vpn.stop()
        vpn.awaitState(VpnState.DISCONNECTED)
        vpn.awaitTunDown()
    }
}
