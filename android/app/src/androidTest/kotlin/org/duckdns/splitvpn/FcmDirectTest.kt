package org.duckdns.splitvpn

import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.After
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import java.net.Inet4Address
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.Socket
import java.nio.ByteBuffer
import javax.net.ssl.SSLSocket
import javax.net.ssl.SSLSocketFactory

/**
 * FCM's persistent connection goes direct though google.com is a YouTube
 * domain: the relay drops a session after 2 min idle, and push would
 * reconnect every 2 min. The emulator's own FCM may log the same line.
 */
@RunWith(AndroidJUnit4::class)
class FcmDirectTest {
    private val vpn = VpnHarness()

    @Before
    fun setUp() {
        vpn.consent()
        vpn.bind()
        if (vpn.state != VpnState.DISCONNECTED) { vpn.stop(); vpn.awaitStopped() }
    }

    @After
    fun tearDown() {
        vpn.stop()
        vpn.awaitState(VpnState.DISCONNECTED)
        vpn.unbind()
    }

    @Test
    fun mtalkGoesDirect() {
        vpn.start("youtube")
        vpn.awaitState(VpnState.CONNECTED)

        val routes = Services.ALL.first { it.id == "youtube" }.allRoutes
        fun routed(a: InetAddress): Boolean {
            val v = ByteBuffer.wrap(a.address).int
            return routes.any { r ->
                val mask = if (r.prefix == 0) 0 else (-1 shl (32 - r.prefix))
                (v and mask) == (ByteBuffer.wrap(InetAddress.getByName(r.address).address).int and mask)
            }
        }
        // Google rotates the answers, often outside YouTube's routes: any MCS
        // host will do. v4 only: the tunnel has no v6 routes.
        val resolved = (listOf("mtalk.google.com", "mtalk4.google.com") + (1..8).map { "alt$it-mtalk.google.com" }).associateWith { h ->
            runCatching { InetAddress.getAllByName(h).filterIsInstance<Inet4Address>() }.getOrDefault(emptyList())
        }
        val pair = resolved.firstNotNullOfOrNull { (h, ips) -> ips.firstOrNull(::routed)?.let { h to it } }
        assumeTrue(
            "no MCS host resolves into tunnel routes: " +
                resolved.entries.joinToString { (h, ips) -> "$h → ${ips.joinToString { it.hostAddress }}" },
            pair != null,
        )
        val (host, ip) = pair!!

        // Our own connect: a timeout on it, and the address for the log line.
        val raw = Socket()
        raw.connect(InetSocketAddress(ip, 5228), 10_000)
        raw.soTimeout = 10_000
        val tls = (SSLSocketFactory.getDefault() as SSLSocketFactory).createSocket(raw, host, 5228, true) as SSLSocket
        tls.use { it.startHandshake() }

        // Logged before the hello is forwarded, so it is there by now.
        val line = "sni $host → ${ip.hostAddress}:5228 direct"
        assertTrue("no '$line'\n${vpn.fileLog()}", line in vpn.fileLog())
    }
}
