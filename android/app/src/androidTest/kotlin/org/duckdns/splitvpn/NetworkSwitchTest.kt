package org.duckdns.splitvpn

import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith

/**
 * Т6: wifi → cellular → wifi under a live tunnel; traffic must recover
 * without a reconnect. Emulator only: `svc` needs the shell uid, which
 * UiAutomation provides, and the emulator has both radios.
 */
@RunWith(AndroidJUnit4::class)
class NetworkSwitchTest {
    private val vpn = VpnHarness()
    private val url = "https://web.telegram.org/"

    @Before
    fun setUp() {
        vpn.shell("svc wifi enable"); vpn.shell("svc data enable")
        vpn.awaitWifi(true)
        vpn.consent()
        vpn.bind()
        if (vpn.state != VpnState.DISCONNECTED) { vpn.stop(); vpn.awaitStopped() }
    }

    @After
    fun tearDown() {
        vpn.shell("svc wifi enable"); vpn.shell("svc data enable")
        vpn.stop()
        vpn.awaitState(VpnState.DISCONNECTED)
        vpn.unbind()
    }

    @Test
    fun trafficSurvivesWifiToCellularAndBack() {
        vpn.start("telegram")
        vpn.awaitState(VpnState.CONNECTED)
        vpn.headEventually(url)

        vpn.shell("svc wifi disable")
        vpn.awaitWifi(false)
        vpn.headEventually(url)
        assertEquals("tunnel must survive the switch", VpnState.CONNECTED, vpn.state)

        vpn.shell("svc wifi enable")
        vpn.awaitWifi(true)
        vpn.headEventually(url)
        assertEquals(VpnState.CONNECTED, vpn.state)
    }
}
