package org.duckdns.splitvpn

import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith

/**
 * The tunnel without its relay or without a network, against the local
 * stand: it must say so, keep running and heal by itself. Emulator only
 * (`svc` needs the shell uid).
 */
@RunWith(AndroidJUnit4::class)
class OutageTest {
    private val vpn = VpnHarness()
    private val url = "https://web.telegram.org/"

    @Before
    fun setUp() {
        vpn.requireStand()
        vpn.shell("svc wifi enable"); vpn.shell("svc data enable")
        vpn.awaitWifi(true)
        vpn.relay(up = true)
        vpn.consent()
        vpn.bind()
        if (vpn.state != VpnState.DISCONNECTED) { vpn.stop(); vpn.awaitStopped() }
    }

    @After
    fun tearDown() {
        vpn.shell("svc wifi enable"); vpn.shell("svc data enable")
        vpn.relay(up = true)
        vpn.stop()
        vpn.awaitState(VpnState.DISCONNECTED)
        vpn.unbind()
    }

    // Airplane mode under a live tunnel: "no network" within the grace,
    // gone again once a network is back, traffic flows.
    @Test
    fun noNetworkIsSaidAndHeals() {
        vpn.start("telegram")
        vpn.awaitState(VpnState.CONNECTED)
        vpn.headEventually(url)

        vpn.shell("svc wifi disable"); vpn.shell("svc data disable")
        vpn.awaitWaiting(Waiting.NO_NETWORK, 15_000)
        assertEquals("the tunnel must stay up", VpnState.CONNECTED, vpn.state)

        vpn.shell("svc wifi enable"); vpn.shell("svc data enable")
        vpn.awaitWaiting(null, 30_000)
        vpn.headEventually(url)
    }

    // The relay dies mid-session: after a breaker's worth of failed app
    // dials the UI says "server unavailable"; the prober notices it back
    // (30 s backoff) without any app traffic.
    @Test
    fun relayOutageIsSaidAndHeals() {
        vpn.start("telegram")
        vpn.awaitState(VpnState.CONNECTED)
        vpn.headEventually(url)

        vpn.relay(up = false)
        vpn.trafficUntil(url, until = { vpn.waiting == Waiting.NO_SERVER }, timeoutMs = 60_000)
        vpn.awaitWaiting(Waiting.NO_SERVER, 1_000)
        assertEquals(VpnState.CONNECTED, vpn.state)

        vpn.relay(up = true)
        vpn.awaitWaiting(null, 90_000)
        vpn.headEventually(url)
    }

    // Switched on while the relay is down: not an error, CONNECTED and
    // waiting, then healed by the prober.
    @Test
    fun unreachableRelayAtStartIsNotAnError() {
        // A credential first: registering needs the relay.
        vpn.start("telegram")
        vpn.awaitState(VpnState.CONNECTED)
        vpn.stop()
        vpn.awaitStopped()

        vpn.relay(up = false)
        vpn.start("telegram")
        vpn.awaitState(VpnState.CONNECTED)
        vpn.awaitWaiting(Waiting.NO_SERVER, 15_000)

        vpn.relay(up = true)
        vpn.awaitWaiting(null, 90_000)
        vpn.headEventually(url)
    }

    // No credential and no network: the service waits for one instead of
    // asking for a code, registers as soon as it appears, no tap.
    @Test
    fun noCredentialOfflineRegistersWhenOnline() {
        vpn.shell("svc wifi disable"); vpn.shell("svc data disable")
        vpn.awaitWifi(false)
        vpn.start("telegram", forgetCred = true)
        vpn.awaitState(VpnState.ERROR)
        assertEquals(TunnelVpnService.ERR_OFFLINE, vpn.error)

        vpn.shell("svc wifi enable"); vpn.shell("svc data enable")
        vpn.awaitState(VpnState.CONNECTED, 60_000)
        assertNull(vpn.waiting)
        vpn.headEventually(url)
    }
}
