package org.duckdns.splitvpn

import androidx.test.ext.junit.runners.AndroidJUnit4
import org.junit.After
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import javax.net.ssl.SSLSocket
import javax.net.ssl.SSLSocketFactory

/**
 * A list as Remote Config brings it (EXTRA_RC_ENDPOINTS: the decrypted
 * blob), through the service's own apply → cache → switch → SetEndpoint,
 * against the local stand.
 */
@RunWith(AndroidJUnit4::class)
class RcListTest {
    private val vpn = VpnHarness()
    private val url = "https://web.telegram.org/"

    @Before
    fun setUp() {
        vpn.requireStand()
        vpn.relay(up = true)
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

    // The first entry is a closed port on the host: up on it, "server
    // unavailable", on to the stand, traffic through its relay.
    @Test
    fun deadFirstMovesToTheStand() {
        vpn.start("telegram", rcList = vpn.rcList(vpn.stand(port = 9), vpn.stand()))
        vpn.awaitState(VpnState.CONNECTED, 60_000)
        vpn.awaitLog("Сервер недоступен, переключаюсь на резервный (2 из 2)", 60_000)
        vpn.awaitWaiting(null, 60_000)
        vpn.headEventually(url)
    }

    // A new list with the other path first: new sessions move there, the
    // open one keeps going on the old endpoint.
    @Test
    fun newListMovesWithoutCuttingSessions() {
        val a = vpn.stand()
        val b = vpn.stand(path = "/app/test2")
        vpn.start("telegram", rcList = vpn.rcList(a, b))
        vpn.awaitState(VpnState.CONNECTED)
        vpn.headEventually(url)

        (SSLSocketFactory.getDefault().createSocket("web.telegram.org", 443) as SSLSocket).use { s ->
            s.soTimeout = 10_000
            val first = head(s)
            assertTrue(first, first.startsWith("HTTP/1.1 "))

            vpn.start("telegram", rcList = vpn.rcList(b, a))
            vpn.awaitLog("Получен новый список серверов")
            vpn.headEventually(url)
            assertNull("waiting=${vpn.waiting}\n${vpn.fileLog()}", vpn.waiting)
            val again = head(s)
            assertTrue("the session opened before the switch: $again", again.startsWith("HTTP/1.1 "))
        }
    }

    /** One keep-alive HEAD on [s]; the status line. */
    private fun head(s: SSLSocket): String {
        s.outputStream.write("HEAD / HTTP/1.1\r\nHost: web.telegram.org\r\n\r\n".toByteArray())
        s.outputStream.flush()
        val input = s.inputStream
        val lines = generateSequence {
            val b = StringBuilder()
            while (true) {
                val c = input.read()
                if (c < 0 || c == '\n'.code) break
                if (c != '\r'.code) b.append(c.toChar())
            }
            b.toString()
        }.takeWhile { it.isNotEmpty() }.toList()
        return lines.firstOrNull() ?: "<closed>"
    }
}
