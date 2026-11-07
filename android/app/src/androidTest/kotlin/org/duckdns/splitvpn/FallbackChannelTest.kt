package org.duckdns.splitvpn

import android.graphics.Bitmap
import android.util.Log
import androidx.test.core.app.ActivityScenario
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import androidx.test.uiautomator.By
import androidx.test.uiautomator.Until
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import java.io.File
import java.net.Socket

/**
 * The VK channel from Kotlin through JNI and Go's vkfallback to a SOCKS5 server,
 * with no VK: the AAR's vkfake engine goes to testenv/socks.py instead of a call.
 *
 * Skipped without the runner argument socks=10.0.2.2:1081, so `make itest` skips it.
 * With it the AAR must come from `make tunnel TUNNEL_TAGS=vkfake`. Rebuild without
 * the tag afterwards.
 */
@RunWith(AndroidJUnit4::class)
class FallbackChannelTest {
    private val vpn = VpnHarness()
    private val url = "https://web.telegram.org/"
    private val socks = InstrumentationRegistry.getArguments().getString("socks")
    private val room get() = "fake-socks://$socks"
    private val key = "c0de".repeat(16)
    private val settle = TunnelVpnService.NEW_NETWORK_SWITCH_MS + 5_000

    @Before
    fun setUp() {
        assumeTrue("no socks argument", socks != null)
        // A real AAR takes any room and hands it to olcrtc, and olcrtc would go to VK.
        assertTrue("the AAR is not vkfake", runCatching { vkfallback.Vkfallback.configure("https://example.invalid/", key, 0) }.isFailure)
        ledger().delete()
        vpn.shell("svc wifi enable"); vpn.shell("svc data enable")
        vpn.awaitWifi(true)
        vpn.relay(up = true)
        vpn.consent()
        vpn.bind()
        if (vpn.state != VpnState.DISCONNECTED) { vpn.stop(); vpn.awaitStopped() }
    }

    @After
    fun tearDown() {
        if (socks == null) return
        vpn.shell("svc wifi enable"); vpn.shell("svc data enable")
        vpn.relay(up = true)
        vpn.stop()
        vpn.awaitStopped()
        // A default start clears the debug extras from prefs for whatever runs next.
        vpn.start("telegram")
        vpn.awaitState(VpnState.CONNECTED)
        vpn.stop()
        vpn.awaitStopped()
        vpn.unbind()
        restartVpnProcess()
        ledger().delete()
        val logs = AppLog.files(vpn.ctx).joinToString("\n") { it.readText() }
        assertFalse("the room in the log", "fake-socks" in logs || socks!! in logs)
        assertFalse("the key in the log", key in logs)
    }

    private fun ledger() = File(vpn.ctx.filesDir, "vkfallback-joins.json")

    // Go's count of joins lives in the process and the ledger, and these tests spend it. Stopped
    // only: with `wanted` set the system would restart the service on the production path.
    private fun restartVpnProcess() {
        Thread.sleep(500)
        val wanted = File(vpn.ctx.dataDir, "shared_prefs/${TunnelVpnService.PREFS}.xml").readText()
        assertTrue("wanted is set", "name=\"wanted\" value=\"false\"" in wanted)
        val pid = vpnPid()
        if (pid.isNotEmpty()) vpn.device.executeShellCommand("run-as org.newvpn kill -9 $pid")
    }

    // Right after a switch the host is not routed yet.
    private fun socksLogOnceRouted(): List<String> {
        val deadline = System.currentTimeMillis() + 30_000
        while (true) {
            try {
                return vpn.socksLog()
            } catch (e: java.io.IOException) {
                if (System.currentTimeMillis() > deadline) throw e
                Thread.sleep(500)
            }
        }
    }

    private fun connectsSince(before: Int) = socksLogOnceRouted().drop(before).filter { it.contains(" connect ") }

    private var logMark = 0

    private fun startWithRelayDown(room: String = this.room, key: String = this.key, force: Boolean = true, maxJoins: Int = 0) {
        logMark = logLines().size
        // A credential first: registering needs the relay.
        vpn.start("telegram")
        vpn.awaitState(VpnState.CONNECTED)
        vpn.stop()
        vpn.awaitStopped()
        vpn.relay(up = false)
        vpn.start("telegram", fallbackRoom = room, fallbackKey = key, fallbackForce = force, fallbackMaxJoins = maxJoins)
        vpn.awaitState(VpnState.CONNECTED)
        vpn.awaitWaiting(Waiting.NO_SERVER, 30_000)
    }

    private fun logLines() = AppLog.files(vpn.ctx).firstOrNull()?.readLines().orEmpty()

    private fun vpnPid(): String = vpn.device.executeShellCommand("pidof org.newvpn:vpn").trim()

    private fun shot(name: String, expect: String) {
        assertTrue("no '$expect' for $name", vpn.device.wait(Until.hasObject(By.textContains(expect)), 10_000))
        Thread.sleep(400)
        val bmp = vpn.instr.uiAutomation.takeScreenshot()
        File(vpn.ctx.filesDir, "shots").apply { mkdirs() }.resolve("$name.png").outputStream().use { bmp.compress(Bitmap.CompressFormat.PNG, 100, it) }
    }

    private fun shotShade(name: String, expect: String) {
        vpn.device.openNotification()
        shot(name, expect)
        vpn.device.executeShellCommand("cmd statusbar collapse")
        Thread.sleep(1_000)
    }

    @Test
    fun relayDownAtStartEnablesAndCarries() {
        val before = vpn.socksLog().size
        startWithRelayDown()
        ActivityScenario.launch(MainActivity::class.java).use {
            vpn.awaitFallback(settle) { it != null }
            if (vpn.fallback == Fallback.READY) shot("1_ready", "Включён запасной канал")
            vpn.headEventually(url, 60_000)
            vpn.awaitFallback(10_000) { it == Fallback.WORKING }
            val seen = connectsSince(before)
            Log.i("FALLBACK", "socks saw: $seen")
            assertTrue("nothing at the SOCKS5 server", seen.any { it.endsWith(":443") })
            assertEquals(Waiting.NO_SERVER, vpn.waiting)
            assertEquals(VpnState.CONNECTED, vpn.state)
            shot("1_working", "трафик идёт через запасной канал")
            shotShade("1_working_shade", "Запасной канал · батарея садится быстрее")
        }
    }

    @Test
    fun relayDiesThenComesBack() {
        val before = vpn.socksLog().size
        vpn.start("telegram", fallbackRoom = room, fallbackKey = key, fallbackForce = true)
        vpn.awaitState(VpnState.CONNECTED)
        vpn.headEventually(url)
        assertNull(vpn.fallback)
        assertEquals("the relay is up: nothing goes to SOCKS5", emptyList<String>(), connectsSince(before))

        val down = System.currentTimeMillis()
        vpn.relay(up = false)
        vpn.trafficUntil(url, until = { vpn.fallback != null }, timeoutMs = 180_000)
        vpn.awaitFallback(1_000) { it != null }
        Log.i("FALLBACK", "enabled ${System.currentTimeMillis() - down} ms after the relay went down")
        vpn.headEventually(url, 60_000)
        vpn.awaitFallback(10_000) { it == Fallback.WORKING }
        assertTrue(connectsSince(before).isNotEmpty())

        // outage_ms: let the prober's interval grow and the call go idle first.
        InstrumentationRegistry.getArguments().getString("outage_ms")?.let {
            Thread.sleep(it.toLong())
            Log.i("FALLBACK", "after the wait: fallback=${vpn.fallback}")
            vpn.headEventually(url, 60_000)
            vpn.awaitFallback(10_000) { f -> f == Fallback.WORKING }
        }
        val up = System.currentTimeMillis()
        vpn.relay(up = true)
        // The prober's interval grows to 5 min, and nothing else looks at the relay.
        vpn.awaitFallback(6 * 60_000) { it == null }
        Log.i("FALLBACK", "off ${System.currentTimeMillis() - up} ms after the relay came back")
        vpn.awaitWaiting(null, 10_000)
        assertEquals(VpnState.CONNECTED, vpn.state)
        val atOff = vpn.socksLog().size
        vpn.headEventually(url)
        assertEquals("back on the relay: nothing goes to SOCKS5", atOff, vpn.socksLog().size)
        ActivityScenario.launch(MainActivity::class.java).use {
            shot("2_back", "Выключить")
            assertFalse("the channel still on the screen", vpn.device.hasObject(By.textContains("апасной канал")))
            shotShade("2_back_shade", "VPN включён")
        }
    }

    @Test
    fun flagOffDoesNotEnable() {
        val before = vpn.socksLog().size
        startWithRelayDown(force = false)
        vpn.trafficUntil(url, until = { vpn.fallback != null }, timeoutMs = settle)
        assertNull(vpn.fallback)
        assertEquals(VpnState.CONNECTED, vpn.state)
        assertEquals(emptyList<String>(), connectsSince(before))
        assertTrue(logLines().drop(logMark).none { "fallback" in it })
    }

    @Test
    fun noRoomDoesNotEnableNorFail() {
        val before = vpn.socksLog().size
        startWithRelayDown(room = "", key = "")
        vpn.trafficUntil(url, until = { vpn.fallback != null }, timeoutMs = settle)
        assertNull(vpn.fallback)
        assertEquals(VpnState.CONNECTED, vpn.state)
        assertNull(vpn.error)
        assertEquals(emptyList<String>(), connectsSince(before))
        assertTrue(logLines().drop(logMark).none { "fallback" in it })
        ActivityScenario.launch(MainActivity::class.java).use { shot("3_no_room", "Сервер пока не отвечает") }
    }

    // Configure refuses: the text Kotlin logs must not quote what it was given.
    @Test
    fun refusedRoomStaysOffAndIsNotLogged() {
        startWithRelayDown(room = "fake-socks://not-an-address")
        val deadline = System.currentTimeMillis() + settle
        while (logLines().none { "vkfake: bad address" in it } && System.currentTimeMillis() < deadline) Thread.sleep(500)
        assertTrue(vpn.fileLog(), logLines().any { "vkfake: bad address" in it })
        vpn.awaitFallback(10_000) { it == null }
        assertEquals(VpnState.CONNECTED, vpn.state)
        assertTrue(logLines().none { "not-an-address" in it })
    }

    // A channel that cannot connect says so, and keeps the VPN up.
    @Test
    fun deadExitSaysSo() {
        startWithRelayDown(room = "fake-socks://${socks!!.substringBefore(":")}:1")
        ActivityScenario.launch(MainActivity::class.java).use {
            vpn.awaitFallback(settle) { it != null }
            vpn.trafficUntil(url, until = { vpn.fallback == Fallback.FAILED }, timeoutMs = 60_000)
            vpn.awaitFallback(1_000) { it == Fallback.FAILED }
            assertEquals(VpnState.CONNECTED, vpn.state)
            shot("3_failed", "запасной канал пока не подключился")
            shotShade("3_failed_shade", "Split VPN")
        }
    }

    // The cap outlives the process, and the screen stops promising a call once it is spent.
    @Test
    fun spentJoinsSurviveARestartAndSaySo() {
        startWithRelayDown(maxJoins = 1)
        vpn.awaitFallback(settle) { it != null }
        vpn.headEventually(url, 60_000)
        vpn.awaitFallback(10_000) { it == Fallback.WORKING }
        vpn.stop()
        vpn.awaitStopped()
        val before = vpn.socksLog().size
        val firstPid = vpnPid()
        restartVpnProcess()
        Thread.sleep(2_000)
        logMark = logLines().size
        vpn.start("telegram", fallbackRoom = room, fallbackKey = key, fallbackForce = true, fallbackMaxJoins = 1)
        vpn.awaitState(VpnState.CONNECTED)
        assertTrue("the process did not restart", firstPid != vpnPid())
        ActivityScenario.launch(MainActivity::class.java).use {
            vpn.awaitFallback(settle) { it != null }
            vpn.trafficUntil(url, until = { vpn.fallback == Fallback.FAILED }, timeoutMs = 60_000)
            vpn.awaitFallback(1_000) { it == Fallback.FAILED }
            val lines = logLines().drop(logMark)
            assertTrue(vpn.fileLog(), lines.any { "join budget spent" in it })
            assertTrue(vpn.fileLog(), lines.none { "vkfake: start" in it })
            shot("3_budget_spent", "запасной канал пока не подключился")
        }
        assertEquals("a refused join reached SOCKS5", emptyList<String>(), connectsSince(before))
    }

    // What a real test leaves in prefs: every default start takes it back.
    @Test
    fun aDefaultStartClearsTheDebugExtras() {
        vpn.start("telegram", fallbackRoom = room, fallbackKey = key, fallbackForce = true, fallbackMaxJoins = 3)
        vpn.awaitState(VpnState.CONNECTED)
        vpn.stop()
        vpn.awaitStopped()
        val prefs = { File(vpn.ctx.dataDir, "shared_prefs/${TunnelVpnService.PREFS}.xml").readText() }
        val deadline = System.currentTimeMillis() + 5_000
        while ("fake-socks" !in prefs() && System.currentTimeMillis() < deadline) Thread.sleep(100)
        assertTrue("fake-socks" in prefs())
        vpn.start("telegram")
        vpn.awaitState(VpnState.CONNECTED)
        vpn.stop()
        vpn.awaitStopped()
        val cleared = System.currentTimeMillis() + 5_000
        while ("fake-socks" in prefs() && System.currentTimeMillis() < cleared) Thread.sleep(100)
        val now = prefs()
        assertFalse("the room stayed", "fake-socks" in now || key in now)
        assertTrue("the force stayed", "name=\"fallback_force.test\" value=\"false\"" in now)
        assertTrue("the cap stayed", "name=\"fallback_max_joins.test\" value=\"0\"" in now)
    }

    private fun awaitLogLine(what: String, since: Int, timeoutMs: Long = 15_000): Boolean {
        val deadline = System.currentTimeMillis() + timeoutMs
        while (System.currentTimeMillis() < deadline) {
            if (logLines().drop(since).any { what in it }) return true
            Thread.sleep(200)
        }
        return false
    }

    // The emulator has only the stand's credential, so this goes over the stand. The relay stays
    // up all along: only the hook can be what makes Go see an outage.
    @Test
    fun relayDeadIsAnOutageAndFalseBringsTheRelayBack() {
        val before = vpn.socksLog().size
        vpn.start("telegram")
        vpn.awaitState(VpnState.CONNECTED)
        vpn.stop()
        vpn.awaitStopped()
        logMark = logLines().size
        vpn.start("telegram", fallbackRoom = room, fallbackKey = key, fallbackForce = true, fallbackRelayDead = true)
        vpn.awaitState(VpnState.CONNECTED)
        vpn.awaitWaiting(Waiting.NO_SERVER, 30_000)
        assertTrue(vpn.log.toString(), vpn.log.any { "Сервер отключён для отладки" in it })
        assertTrue(vpn.fileLog(), logLines().drop(logMark).any { "unreachable at start" in it })
        vpn.awaitFallback(settle) { it != null }
        vpn.headEventually(url, 60_000)
        vpn.awaitFallback(10_000) { it == Fallback.WORKING }
        assertTrue("nothing at the SOCKS5 server", connectsSince(before).any { it.endsWith(":443") })
        assertEquals(Waiting.NO_SERVER, vpn.waiting)

        val pid = vpnPid()
        val starts = logLines().count { "Go tunnel started" in it }
        val up = System.currentTimeMillis()
        val mark = logLines().size
        vpn.start("telegram", fallbackRoom = room, fallbackKey = key, fallbackForce = true, fallbackRelayDead = false)
        vpn.awaitFallback(60_000) { it == null }
        Log.i("FALLBACK", "off ${System.currentTimeMillis() - up} ms after the hook went off")
        vpn.awaitWaiting(null, 10_000)
        assertEquals(VpnState.CONNECTED, vpn.state)
        assertTrue(vpn.fileLog(), awaitLogLine("fallback_off", mark))
        assertTrue(vpn.fileLog(), logLines().drop(mark).any { "fallback_off" in it && "relay_back" in it })
        assertEquals("the process survived", pid, vpnPid())
        assertEquals("the tunnel did not restart", starts, logLines().count { "Go tunnel started" in it })
        val atOff = vpn.socksLog().size
        vpn.headEventually(url)
        assertEquals("back on the relay: nothing goes to SOCKS5", atOff, vpn.socksLog().size)

        val prefs = File(vpn.ctx.dataDir, "shared_prefs/${TunnelVpnService.PREFS}.xml")
        val settled = System.currentTimeMillis() + 5_000
        while ("name=\"fallback_relay_dead.test\" value=\"false\"" !in prefs.readText() && System.currentTimeMillis() < settled) Thread.sleep(100)
        assertTrue("name=\"fallback_relay_dead.test\" value=\"false\"" in prefs.readText())
        assertFalse("the dead address in prefs", "192.0.2.1" in prefs.readText() || "relay-dead" in prefs.readText())
        val known = File(vpn.ctx.filesDir, Endpoints.KNOWN_FILE)
        assertFalse("the dead address in the known file", known.exists() && ("192.0.2.1" in known.readText() || "relay-dead" in known.readText()))
    }

    // The real activity and a real tap. True only: its START has no stand extras, so with false
    // this tap would send the emulator to the production endpoints.
    @Test
    fun relayDeadTapOnARunningTunnelKeepsItAndTheNextTapStops() {
        vpn.start("telegram")
        vpn.awaitState(VpnState.CONNECTED)
        vpn.stop()
        vpn.awaitStopped()
        vpn.start("telegram", fallbackRelayDead = true)
        vpn.awaitState(VpnState.CONNECTED)
        val pid = vpnPid()
        val component = "org.newvpn/org.duckdns.splitvpn.MainActivity"
        val first = vpn.device.executeShellCommand("am start --activity-clear-task -n $component --ez fallback_relay_dead true")
        assertTrue(first, "Warning" !in first)
        val toggle = vpn.device.wait(Until.findObject(By.res("org.newvpn", "toggleButton")), 10_000)
        assertTrue("no toggle", toggle != null)
        val mark = logLines().size
        toggle.click()
        assertTrue(vpn.fileLog(), awaitLogLine("startTunnel: busy", mark))
        assertEquals(VpnState.CONNECTED, vpn.state)
        assertEquals("the process survived", pid, vpnPid())
        toggle.click()
        vpn.awaitStopped()
    }

    @Test
    fun stopWithTheChannelOn() {
        startWithRelayDown()
        vpn.awaitFallback(settle) { it != null }
        vpn.headEventually(url, 60_000)
        vpn.awaitFallback(10_000) { it == Fallback.WORKING }
        val pid = vpnPid()
        val open = Socket("web.telegram.org", 443)

        val t0 = System.currentTimeMillis()
        vpn.stop()
        vpn.awaitStopped()
        Log.i("FALLBACK", "stopped in ${System.currentTimeMillis() - t0} ms")
        vpn.awaitTunDown()
        assertNull(vpn.fallback)
        open.soTimeout = 5_000
        assertEquals("the channel's connection must close", -1, runCatching { open.getInputStream().read() }.getOrDefault(-1))
        open.close()

        // A network event after the stop must not reach Go.
        val done = { logLines().indexOfLast { "stopTunnel done" in it } }
        val stopLine = done()
        assertTrue(stopLine >= 0)
        assertTrue(logLines().subList(0, stopLine).any { "vkfake: stop" in it })
        vpn.shell("svc wifi disable")
        vpn.awaitWifi(false)
        vpn.shell("svc wifi enable")
        vpn.awaitWifi(true)
        Thread.sleep(8_000)
        val after = logLines().drop(stopLine + 1).filter { "fallback" in it || "vkfake" in it }
        assertEquals("calls after the stop", emptyList<String>(), after)
        assertEquals("the process survived", pid, vpnPid())

        // vkfallback is one per process: the next run must find it clean.
        val before = vpn.socksLog().size
        vpn.start("telegram", fallbackRoom = room, fallbackKey = key, fallbackForce = true)
        vpn.awaitState(VpnState.CONNECTED)
        vpn.awaitFallback(settle + 30_000) { it != null }
        vpn.headEventually(url, 60_000)
        vpn.awaitFallback(10_000) { it == Fallback.WORKING }
        assertTrue(connectsSince(before).isNotEmpty())
    }

    @Test
    fun networkChangesWithTheChannelOn() {
        val before = vpn.socksLog().size
        startWithRelayDown()
        vpn.awaitFallback(settle) { it != null }
        vpn.headEventually(url, 60_000)
        vpn.awaitFallback(10_000) { it == Fallback.WORKING }
        val pid = vpnPid()
        val starts = { logLines().count { "vkfake: start" in it } }

        vpn.shell("svc wifi disable")
        vpn.awaitWifi(false)
        val onLte = socksLogOnceRouted().size
        vpn.headEventually(url, 90_000)
        assertTrue(connectsSince(onLte).isNotEmpty())
        // The restart for the new network waits out the gap since the last start.
        vpn.awaitFallback(60_000) { it == Fallback.WORKING }
        Log.i("FALLBACK", "on LTE: state=${vpn.state} fallback=${vpn.fallback} starts=${starts()}")

        vpn.shell("svc wifi enable")
        vpn.awaitWifi(true)
        val onWifi = socksLogOnceRouted().size
        vpn.headEventually(url, 90_000)
        assertTrue(connectsSince(onWifi).isNotEmpty())
        vpn.awaitFallback(60_000) { it == Fallback.WORKING }
        Log.i("FALLBACK", "on Wi-Fi: state=${vpn.state} fallback=${vpn.fallback} starts=${starts()}")

        // No network at all: says so, the channel comes back with the network.
        ActivityScenario.launch(MainActivity::class.java).use {
            vpn.shell("svc wifi disable"); vpn.shell("svc data disable")
            vpn.awaitWaiting(Waiting.NO_NETWORK, 30_000)
            shot("5_no_network", "Нет сети")
            vpn.shell("svc wifi enable"); vpn.shell("svc data enable")
            vpn.awaitWifi(true)
            vpn.awaitWaiting(Waiting.NO_SERVER, 60_000)
            val back = socksLogOnceRouted().size
            vpn.headEventually(url, 120_000)
            assertTrue(connectsSince(back).isNotEmpty())
            vpn.awaitFallback(60_000) { it == Fallback.WORKING }
            shot("5_working_again", "трафик идёт через запасной канал")
        }
        Log.i("FALLBACK", "after no network: fallback=${vpn.fallback} starts=${starts()} socks=${connectsSince(before).size}")
        assertEquals(VpnState.CONNECTED, vpn.state)
        assertEquals("the process survived", pid, vpnPid())
    }
}
