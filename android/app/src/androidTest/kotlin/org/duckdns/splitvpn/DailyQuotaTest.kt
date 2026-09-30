package org.duckdns.splitvpn

import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import java.net.HttpURLConnection
import java.net.URL

/** A used-up daily limit takes the VPN down, TUN and all, and says why. */
@RunWith(AndroidJUnit4::class)
class DailyQuotaTest {
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
        // Back to the default limit; the test's traffic is not the emulator's day.
        vpn.start("telegram", quotaMb = Quota.DEFAULT_MB, quotaReset = true)
        vpn.awaitState(VpnState.CONNECTED)
        vpn.stop()
        vpn.awaitStopped()
        vpn.unbind()
    }

    private fun get(url: String) = runCatching {
        val c = URL(url).openConnection() as HttpURLConnection
        c.connectTimeout = 5000
        c.readTimeout = 5000
        c.inputStream.use { it.readBytes().size }.also { c.disconnect() }
    }

    @Test
    fun usedUpLimitStopsTheVpn() {
        vpn.start("youtube", quotaMb = 1, quotaReset = true)
        vpn.awaitState(VpnState.CONNECTED)
        val deadline = System.currentTimeMillis() + 60_000
        while (vpn.state == VpnState.CONNECTED && System.currentTimeMillis() < deadline) get("https://www.youtube.com/")
        vpn.awaitState(VpnState.ERROR, 10_000)
        assertEquals(TunnelVpnService.ERR_QUOTA, vpn.error)
        vpn.awaitTunDown()

        // Refused before any tunnel while the day lasts.
        vpn.start("youtube")
        val until = System.currentTimeMillis() + 10_000
        while ("startTunnel: quota" !in vpn.fileLog() && System.currentTimeMillis() < until) Thread.sleep(200)
        assertTrue(vpn.fileLog(), "startTunnel: quota" in vpn.fileLog())
        assertEquals(VpnState.ERROR, vpn.state)
        assertEquals(TunnelVpnService.ERR_QUOTA, vpn.error)
        assertEquals(false, vpn.tunUp())
    }
}
