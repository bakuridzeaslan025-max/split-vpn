package org.duckdns.splitvpn

import android.content.Context
import android.content.Intent
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.net.VpnService
import androidx.test.core.app.ActivityScenario
import androidx.test.platform.app.InstrumentationRegistry
import androidx.test.uiautomator.By
import androidx.test.uiautomator.UiDevice
import androidx.test.uiautomator.Until
import org.junit.Assert.assertTrue
import org.junit.Assert.fail
import java.net.HttpURLConnection
import java.net.NetworkInterface
import java.net.URL
import java.util.concurrent.CopyOnWriteArrayList

/**
 * Drives the real TunnelVpnService against the real VDS. Runs in the app
 * process, so its own HTTP goes through the TUN like the user's would.
 * Emulator or stock ROM only: MIUI blocks the injected taps.
 */
class VpnHarness {
    val instr = InstrumentationRegistry.getInstrumentation()
    val ctx: Context = instr.targetContext
    val device: UiDevice = UiDevice.getInstance(instr)

    @Volatile var state = VpnState.DISCONNECTED
    @Volatile var error: String? = null
    @Volatile var waiting: Waiting? = null
    val log = CopyOnWriteArrayList<String>()

    @Volatile private var synced = false

    private val client = VpnClient(ctx) { s, e, l, _, w, _, _ ->
        state = s; error = e; waiting = w; synced = true
        log.clear(); log.addAll(l)
    }

    // The local stand (android/testenv, `make itest`): the relay on the host
    // behind nginx with its own CA, invites for registration, and a control
    // port that stops and starts the relay. Runner arguments from gradle.
    private val args = InstrumentationRegistry.getArguments()
    private val endpoint = args.getString("endpoint")
    private val ca = args.getString("ca")?.let { String(android.util.Base64.decode(it, android.util.Base64.DEFAULT)) }
    private val control = args.getString("control")

    companion object {
        // One-shot codes, shared by every test in the run: a harness per test
        // class must not start over from the first one.
        private val invites by lazy {
            ArrayDeque(InstrumentationRegistry.getArguments().getString("invites")?.split(",")?.filter { it.isNotBlank() } ?: emptyList())
        }
    }

    fun requireStand() {
        if (endpoint == null) fail("no local stand: run through `make itest`")
    }

    /**
     * Also clears `wanted`: installing the APK fires MY_PACKAGE_REPLACED and
     * binding creates the service, and with `wanted` left over either one
     * brings up the production tunnel. Our STOP queues behind their START.
     * Waits for the first snapshot, so the tests' "leftover tunnel" check
     * sees the real state rather than the initial DISCONNECTED.
     */
    fun bind() {
        instr.runOnMainSync { client.bind() }
        val deadline = System.currentTimeMillis() + 10_000
        while (!synced && System.currentTimeMillis() < deadline) Thread.sleep(50)
        stop()
    }
    fun unbind() = instr.runOnMainSync { client.unbind() }

    /**
     * One-time system consent dialog; a no-op once granted. Launched from a
     * foreground Activity: the system drops it when started from background.
     */
    fun consent() {
        if (VpnService.prepare(ctx) == null) return
        ActivityScenario.launch(MainActivity::class.java).use { scenario ->
            // Not MainActivity.VPN_REQUEST_CODE: its onActivityResult would
            // start the tunnel itself, with its own service set.
            scenario.onActivity { it.startActivityForResult(VpnService.prepare(it), 4242) }
            val ok = device.wait(Until.findObject(By.res("android", "button1")), 10_000)
            if (ok == null) fail("VPN consent dialog not found")
            ok!!.click()
            device.waitForIdle()
        }
        assertTrue("consent not granted", VpnService.prepare(ctx) == null)
    }

    /**
     * [forgetCred]: register afresh, one invite per call. A first start on the stand takes one too.
     * [rcList]: endpoints as if from Remote Config ([rcList] builds it) instead of the stand's
     * single one; on a running tunnel it is a new list, the start itself is skipped.
     * [quotaMb]: the daily limit, as if from Remote Config; it stays until the next one.
     * [quotaReset]: today's count starts from zero.
     */
    fun start(vararg services: String, forgetCred: Boolean = false, rcList: String? = null, quotaMb: Long? = null, quotaReset: Boolean = false) {
        val i = Intent(ctx, TunnelVpnService::class.java).setAction(TunnelVpnService.ACTION_START)
            .putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf(*services))
        quotaMb?.let { i.putExtra(TunnelVpnService.EXTRA_QUOTA_MB, it) }
        if (quotaReset) i.putExtra(TunnelVpnService.EXTRA_QUOTA_RESET, true)
        if (endpoint != null) {
            if (rcList != null) i.putExtra(TunnelVpnService.EXTRA_RC_ENDPOINTS, rcList)
            else i.putExtra(TunnelVpnService.EXTRA_ENDPOINT, endpoint)
            i.putExtra(TunnelVpnService.EXTRA_CA, ca)
            i.putExtra(TunnelVpnService.EXTRA_FORGET_CRED, forgetCred)
            i.putExtra(TunnelVpnService.EXTRA_INVITE, invites.removeFirstOrNull() ?: "")
        }
        ctx.startForegroundService(i)
    }

    /** The stand's endpoint with another path (testenv/nginx.conf) or port. */
    fun stand(path: String? = null, port: Int? = null): String {
        val (host, ip, p, pth) = endpoint!!.split("|")
        return listOf(host, ip, port ?: p, path ?: pth).joinToString("|")
    }

    /** Decrypted Remote Config JSON of "host|ip|port|path" entries. */
    fun rcList(vararg eps: String) = org.json.JSONArray(
        eps.map { e ->
            val (host, ip, port, path) = e.split("|")
            org.json.JSONObject().put("host", host).put("ip", ip).put("port", port.toInt()).put("path", path)
        },
    ).toString()

    /** Stops or starts the relay container on the host; nginx stays, so the
     *  TLS still answers. A raw socket: the app forbids cleartext HTTP. */
    fun relay(up: Boolean) {
        val (host, port) = control!!.split(":")
        java.net.Socket(host, port.toInt()).use { s ->
            s.soTimeout = 30_000
            s.getOutputStream().write("GET /relay/${if (up) "up" else "down"} HTTP/1.0\r\n\r\n".toByteArray())
            val status = s.getInputStream().bufferedReader().readLine()
            assertTrue("control: $status", status?.contains(" 200 ") == true)
        }
    }

    fun awaitWaiting(want: Waiting?, timeoutMs: Long) {
        val deadline = System.currentTimeMillis() + timeoutMs
        while (waiting != want && System.currentTimeMillis() < deadline) Thread.sleep(200)
        assertTrue("waiting=$waiting state=$state log=$log\n${fileLog()}", waiting == want)
    }

    /** One request every [everyMs] until [timeoutMs] or [until]: app traffic that the breaker counts. */
    fun trafficUntil(url: String, until: () -> Boolean, timeoutMs: Long, everyMs: Long = 2_000) {
        val deadline = System.currentTimeMillis() + timeoutMs
        while (!until() && System.currentTimeMillis() < deadline) {
            runCatching { head(url) }
            Thread.sleep(everyMs)
        }
    }

    fun stop() {
        ctx.startService(Intent(ctx, TunnelVpnService::class.java).setAction(TunnelVpnService.ACTION_STOP))
    }

    fun awaitStopped() = awaitState(VpnState.DISCONNECTED)

    /** Tail of the app's own file log, for failure messages: gradle
     *  uninstalls the app afterwards, so this is the only trace left. */
    fun fileLog(): String = AppLog.files(ctx).firstOrNull()?.readLines()?.takeLast(40)?.joinToString("\n") ?: "<no file log>"

    fun awaitState(want: VpnState, timeoutMs: Long = 30_000) {
        val deadline = System.currentTimeMillis() + timeoutMs
        while (state != want && System.currentTimeMillis() < deadline) Thread.sleep(100)
        assertTrue("state=$state err=$error log=$log\n${fileLog()}", state == want)
    }

    fun awaitLog(prefix: String, timeoutMs: Long = 30_000): String {
        val deadline = System.currentTimeMillis() + timeoutMs
        while (System.currentTimeMillis() < deadline) {
            log.firstOrNull { it.substringAfter("  ").startsWith(prefix) }?.let { return it }
            Thread.sleep(100)
        }
        fail("no '$prefix' in log: $log\n${fileLog()}"); throw IllegalStateException()
    }

    fun tunUp(): Boolean = NetworkInterface.getNetworkInterfaces().toList()
        .any { nif -> nif.inetAddresses.toList().any { it.hostAddress == "10.255.0.1" } }

    /** The kernel tears the interface down a moment after the fd closes. */
    fun awaitTunDown(timeoutMs: Long = 5_000) {
        val deadline = System.currentTimeMillis() + timeoutMs
        while (tunUp() && System.currentTimeMillis() < deadline) Thread.sleep(100)
        assertTrue("TUN still up", !tunUp())
    }

    /** HEAD through the tunnel; returns latency in ms. */
    fun head(url: String): Long {
        val t0 = System.nanoTime()
        val c = URL(url).openConnection() as HttpURLConnection
        c.requestMethod = "HEAD"
        c.connectTimeout = 5000
        c.readTimeout = 5000
        c.instanceFollowRedirects = false
        c.responseCode
        c.disconnect()
        return (System.nanoTime() - t0) / 1_000_000
    }

    fun headEventually(url: String, timeoutMs: Long = 20_000): Long {
        val deadline = System.currentTimeMillis() + timeoutMs
        var last: Exception? = null
        while (System.currentTimeMillis() < deadline) {
            try { return head(url) } catch (e: Exception) { last = e; Thread.sleep(500) }
        }
        fail("$url unreachable within $timeoutMs ms: $last"); throw IllegalStateException()
    }

    fun shell(cmd: String) {
        instr.uiAutomation.executeShellCommand(cmd).close()
    }

    // activeNetwork is our own VPN while the tunnel is up, so look at the
    // underlying transports instead.
    fun hasWifi(): Boolean {
        val cm = ctx.getSystemService(ConnectivityManager::class.java)
        return cm.allNetworks.any { cm.getNetworkCapabilities(it)?.hasTransport(NetworkCapabilities.TRANSPORT_WIFI) == true }
    }

    fun awaitWifi(want: Boolean, timeoutMs: Long = 30_000) {
        val deadline = System.currentTimeMillis() + timeoutMs
        while (hasWifi() != want && System.currentTimeMillis() < deadline) Thread.sleep(200)
        assertTrue("wifi present=${hasWifi()}, wanted $want", hasWifi() == want)
    }
}
