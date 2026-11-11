package org.duckdns.splitvpn

import android.app.NotificationManager
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInstaller
import android.util.Log
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.Shadows.shadowOf
import org.robolectric.shadows.ShadowLog
import java.io.File
import org.json.JSONArray
import org.json.JSONObject
import java.net.ServerSocket
import java.net.Socket
import kotlin.concurrent.thread

/** Self-update: which mirror, what may be installed, and how a failed download ends. */
@RunWith(RobolectricTestRunner::class)
class UpdaterTest {

    private val app get() = RuntimeEnvironment.getApplication()
    private val dest get() = File(app.cacheDir, "test.apk")
    private var server: ServerSocket? = null
    private val realVisible = Updater.visible
    private val realEvent = Analytics.event
    private val events = mutableListOf<Map<String, Any>>()

    @org.junit.Before
    fun setUp() {
        Analytics.event = { _, name, params -> if (name == "self_update") events += params }
        app.getSharedPreferences(Services.PREFS, Context.MODE_PRIVATE).edit().remove(Updater.KEY_SESSION).commit()
    }

    @After
    fun tearDown() {
        Analytics.event = realEvent
        server?.close()
        Versions.debug = true
        Updater.busy.set(false)
        Updater.takePending(app)
        Updater.visible = realVisible
        Updater.timing = Updater.Timing()
        Updater.checkApk = realCheck
        Updater.installer = realInstaller
        Updater.lastMirror = null
        Updater.lastDetail = null
        ShadowLog.clear()
    }

    private val realCheck = Updater.checkApk
    private val realInstaller = Updater.installer

    // Requests that asked to close the connection: on Android, OkHttp keeps the rest in its pool.
    private val closing = java.util.concurrent.ConcurrentLinkedQueue<String>()

    /** Serves one canned response per connection, whatever the request. */
    private fun serve(respond: (path: String, Socket) -> Unit) = serveRequests { _, path, s -> respond(path, s) }

    private fun serveRequests(respond: (method: String, path: String, Socket) -> Unit): String {
        val s = ServerSocket(0).also { server = it }
        thread(isDaemon = true) {
            while (!s.isClosed) {
                val c = try { s.accept() } catch (_: Exception) { break }
                thread(isDaemon = true) {
                    c.use {
                        // Read the whole request: closing on unread bytes sends a RST.
                        val req = it.getInputStream().bufferedReader()
                        val line = req.readLine().orEmpty().split(' ')
                        while (true) {
                            val h = req.readLine()
                            if (h.isNullOrEmpty()) break
                            if (h.equals("Connection: close", ignoreCase = true)) closing += "${line[0]} ${line.getOrElse(1) { "" }}"
                        }
                        try { respond(line[0], line.getOrElse(1) { "" }, it) } catch (_: Exception) {}
                    }
                }
            }
        }
        return "http://127.0.0.1:${s.localPort}"
    }

    private fun Socket.reply(head: String, body: ByteArray = ByteArray(0)) {
        getOutputStream().apply { write("$head\r\nConnection: close\r\n\r\n".toByteArray()); write(body); flush() }
    }

    private val url = Updater.RELEASES + "download/v0.5.7/split-vpn-0.5.7.apk"

    @Test
    fun acceptsOnlyAnAssetOfOurRepo() {
        assertTrue(Updater.urlOk(url))
        // rc/config.json's: whatever release is the latest.
        assertTrue(Updater.urlOk(Updater.RELEASES + "latest/download/split-vpn.apk"))
        for (bad in listOf(
            "http://github.com/bakuridzeaslan025-max/split-vpn/releases/download/v1/a.apk",
            "https://github.com/someone/split-vpn/releases/download/v1/a.apk",
            "https://github.com/bakuridzeaslan025-max/split-vpn-evil/releases/download/v1/a.apk",
            "https://github.com.evil.org/bakuridzeaslan025-max/split-vpn/releases/download/v1/a.apk",
            Updater.RELEASES + "download/v1/a.apk?x=1",
            Updater.RELEASES + "download/v1/sub/a.apk",
            Updater.RELEASES + "download/../../../other/releases/download/v1/a.apk",
            Updater.RELEASES + "download/v1/a.zip",
            Updater.RELEASES + "download/v1/a%2F.apk",
            Updater.RELEASES + "v1/a.apk",
            Updater.RELEASES + "latest/a.apk",
            Updater.RELEASES + "latest/download/x/a.apk",
            Updater.RELEASES + "download/../download/a.apk",
            "",
        )) assertFalse(bad, Updater.urlOk(bad))
    }

    private val self = Updater.Pkg("org.newvpn", 114, setOf("aa"))

    @Test
    fun verifiesPackageCertificateAndVersions() {
        assertNull(Updater.verify(self, Updater.Pkg("org.newvpn", 115, setOf("aa")), 115))
        // Key rotation: the new APK proves the old key in its history.
        assertNull(Updater.verify(self, Updater.Pkg("org.newvpn", 115, setOf("aa", "bb")), 115))

        assertEquals("not an apk", Updater.verify(self, null, 115))
        assertEquals("package", Updater.verify(self, Updater.Pkg("org.evil", 115, setOf("aa")), 115))
        assertEquals("certificate", Updater.verify(self, Updater.Pkg("org.newvpn", 115, setOf("bb")), 115))
        assertEquals("certificate", Updater.verify(self, Updater.Pkg("org.newvpn", 115, emptySet()), 115))
        assertEquals("certificate", Updater.verify(Updater.Pkg("org.newvpn", 114, emptySet()), Updater.Pkg("org.newvpn", 115, emptySet()), 115))
        assertEquals("not newer", Updater.verify(self, Updater.Pkg("org.newvpn", 114, setOf("aa")), 114))
        assertEquals("older than latest", Updater.verify(self, Updater.Pkg("org.newvpn", 115, setOf("aa")), 116))
    }

    @Test
    fun followsTheRedirectToTheCdn() {
        val body = ByteArray(200_000) { it.toByte() }
        val base = serve { path, s ->
            if (path == "/release") s.reply("HTTP/1.1 302 Found\r\nLocation: /cdn\r\nContent-Length: 0")
            else s.reply("HTTP/1.1 200 OK\r\nContent-Length: ${body.size}", body)
        }
        val shown = mutableListOf<Int>()
        assertNull(Updater.download("$base/release", dest, progress = { shown += it }))
        assertArrayEquals(body, dest.readBytes())
        // Each percent once, in order, up to the whole file.
        assertEquals(shown.sorted().distinct(), shown)
        assertEquals(100, shown.last())
    }

    @Test
    fun httpErrorLeavesNoFile() {
        val base = serve { _, s -> s.reply("HTTP/1.1 404 Not Found\r\nContent-Length: 0") }
        val f = Updater.download("$base/x", dest)!!
        assertEquals(Updater.Kind.HTTP, f.kind)
        assertEquals("HTTP 404", f.detail)
        assertFalse(dest.exists())
    }

    @Test
    fun cutOffBodyIsBroken() {
        val base = serve { _, s -> s.reply("HTTP/1.1 200 OK\r\nContent-Length: 1000", ByteArray(10)) }
        val f = Updater.download("$base/x", dest)!!
        assertEquals(Updater.Kind.BROKEN, f.kind)
        assertEquals(10L, f.bytes)
        assertFalse(dest.exists())
    }

    @Test
    fun silentServerIsATimeout() {
        val base = serve { _, _ -> Thread.sleep(5_000) }
        val f = Updater.download("$base/x", dest, readMs = 300)!!
        assertEquals(Updater.Kind.TIMEOUT, f.kind)
        assertTrue(f.ms >= 300)
        assertFalse(dest.exists())
    }

    @Test
    fun refusedConnectionIsBroken() {
        val port = ServerSocket(0).use { it.localPort }
        assertEquals(Updater.Kind.BROKEN, Updater.download("http://127.0.0.1:$port/x", dest)!!.kind)
    }

    /** Crashlytics groups by frames: one issue per kind, not one for every failure. */
    @Test
    fun eachKindIsItsOwnIssue() {
        val a = Updater.failure(Updater.Failure(Updater.Kind.HTTP, "HTTP 404", 0, 12))
        val b = Updater.failure(Updater.Failure(Updater.Kind.HTTP, "HTTP 503", 5, 40))
        val c = Updater.failure(Updater.Failure(Updater.Kind.TIMEOUT, "timeout", 5, 40))
        assertEquals(a.stackTrace.toList(), b.stackTrace.toList())
        assertFalse(a.stackTrace.toList() == c.stackTrace.toList())
        assertTrue(a.message!!.contains("HTTP 404") && a.message!!.contains("12 ms"))
    }

    @Test
    fun debugBuildRefuses() {
        Versions.debug = true
        assertEquals(Updater.Result.DISABLED, Updater.update(app, Versions(latest = Long.MAX_VALUE, apk = url)))
    }

    @Test
    fun asksForThePermissionBeforeDownloading() {
        Versions.debug = false
        shadowOf(app.packageManager).setCanRequestPackageInstalls(false)
        assertEquals(Updater.Result.NEED_PERMISSION, Updater.update(app, Versions(latest = Long.MAX_VALUE, apk = url)))
        assertEquals(Updater.Result.UP_TO_DATE, Updater.update(app, Versions(latest = BuildConfig.VERSION_CODE.toLong(), apk = url)))
    }

    /** Reported: Remote Config carries a URL the app will not take. */
    @Test
    fun badUrlIsNotAFailureToOffer() {
        Versions.debug = false
        shadowOf(app.packageManager).setCanRequestPackageInstalls(true)
        assertEquals(Updater.Result.BAD_URL, Updater.update(app, Versions(latest = Long.MAX_VALUE, apk = "https://example.org/a.apk")))
    }

    @Test
    fun secondCallWhileDownloadingIsBusy() {
        Versions.debug = false
        shadowOf(app.packageManager).setCanRequestPackageInstalls(true)
        Updater.busy.set(true)
        assertEquals(Updater.Result.BUSY, Updater.update(app, Versions(latest = Long.MAX_VALUE, apk = url)))
        assertTrue("a refused call must not free the other's slot", Updater.busy.get())
    }

    private fun status(st: Int, confirm: Intent? = null, session: Int = 7) = Intent().putExtra(PackageInstaller.EXTRA_STATUS, st)
        .putExtra(PackageInstaller.EXTRA_SESSION_ID, session)
        .apply { if (confirm != null) putExtra(Intent.EXTRA_INTENT, confirm) }

    private val confirm = Intent("android.content.pm.action.CONFIRM_INSTALL").setPackage("com.android.packageinstaller")

    @Test
    fun confirmationOpensOverTheApp() {
        Updater.visible = { true }
        Updater.onStatus(app, status(PackageInstaller.STATUS_PENDING_USER_ACTION, confirm))
        assertEquals(confirm.action, shadowOf(app).nextStartedActivity?.action)
        assertTrue(notifications().isEmpty())
        assertNull(Updater.takePending(app))
    }

    /**
     * From the background Android 10+ drops the dialog silently: it waits for
     * the app to come back, and in a notification meanwhile.
     */
    @Test
    fun confirmationInTheBackgroundWaitsForTheApp() {
        Updater.visible = { false }
        Updater.onStatus(app, status(PackageInstaller.STATUS_PENDING_USER_ACTION, confirm))
        assertNull(shadowOf(app).nextStartedActivity)
        val n = notifications().single()
        assertEquals(confirm.action, shadowOf(n.contentIntent).savedIntent.action)
        assertEquals(confirm.action, Updater.takePending(app)?.action)
        assertTrue(notifications().isEmpty())
        assertNull(Updater.takePending(app))
    }

    @Test
    fun answeredFromTheNotificationIsNotAskedAgain() {
        Updater.visible = { false }
        Updater.onStatus(app, status(PackageInstaller.STATUS_PENDING_USER_ACTION, confirm, session = 7))
        // An older session abandoned before this one says nothing about it.
        Updater.onStatus(app, status(PackageInstaller.STATUS_FAILURE_ABORTED, session = 6))
        Updater.onStatus(app, status(PackageInstaller.STATUS_FAILURE_ABORTED, session = 7))
        assertNull(Updater.takePending(app))
    }

    /** The extras come through a mutable PendingIntent: anything but the installer's dialog stays unstarted. */
    @Test
    fun startsNothingButTheInstallerDialog() {
        Updater.visible = { true }
        Updater.onStatus(app, status(PackageInstaller.STATUS_PENDING_USER_ACTION, Intent(Intent.ACTION_VIEW)))
        Updater.onStatus(app, status(PackageInstaller.STATUS_PENDING_USER_ACTION))
        assertNull(shadowOf(app).nextStartedActivity)
        assertEquals(2, reported().size)
    }

    /** The install killed the app; a tap on the notification reopens it, opening it otherwise clears it. */
    @Test
    fun installedOffersToOpenTheApp() {
        Updater.visible = { true }
        Updater.onStatus(app, status(PackageInstaller.STATUS_SUCCESS))
        assertTrue(notifications().isEmpty())

        Updater.visible = { false }
        Updater.onStatus(app, status(PackageInstaller.STATUS_SUCCESS))
        val n = notifications().single()
        assertEquals(MainActivity::class.java.name, shadowOf(n.contentIntent).savedIntent.component?.className)
        assertNull(Updater.takePending(app))
        assertTrue(notifications().isEmpty())
    }

    @Test
    fun failedInstallIsReportedCancelIsNot() {
        Updater.onStatus(app, status(PackageInstaller.STATUS_FAILURE_ABORTED))
        assertTrue(reported().isEmpty())
        Updater.onStatus(app, status(PackageInstaller.STATUS_FAILURE_CONFLICT))
        assertTrue(reported().single().throwable.message!!.contains("update install: status"))
    }

    private fun notifications() = shadowOf(app.getSystemService(NotificationManager::class.java)).allNotifications

    private fun committed(session: Int) =
        app.getSharedPreferences(Services.PREFS, Context.MODE_PRIVATE).edit().putInt(Updater.KEY_SESSION, session).commit()

    private fun steps() = events.map { listOfNotNull(it["step"], it["detail"]).joinToString(":") }

    // The dialog came up over the app, so nothing was pending: the session
    // committed is what tells the user's cancel.
    @Test
    fun cancelOfOurSessionOnScreenIsCancelled() {
        Updater.visible = { true }
        committed(7)
        Updater.onStatus(app, status(PackageInstaller.STATUS_PENDING_USER_ACTION, confirm, session = 7))
        Updater.onStatus(app, status(PackageInstaller.STATUS_FAILURE_ABORTED, session = 6))
        Updater.onStatus(app, status(PackageInstaller.STATUS_FAILURE_ABORTED, session = 7))
        assertEquals(listOf("cancelled"), steps())
        // Final: a late duplicate is nobody's.
        Updater.onStatus(app, status(PackageInstaller.STATUS_FAILURE_ABORTED, session = 7))
        assertEquals(listOf("cancelled"), steps())
    }

    // A new install abandons the session committed before: its ABORTED is
    // no cancel of the user's.
    @Test
    fun abandonedEarlierSessionIsNoCancel() {
        Versions.debug = false
        committed(5)
        val apk = File(app.cacheDir, "u.apk").apply { writeBytes(ByteArray(16)) }
        runCatching {
            org.robolectric.util.ReflectionHelpers.callInstanceMethod<Unit>(
                Updater, "install",
                org.robolectric.util.ReflectionHelpers.ClassParameter.from(Context::class.java, app),
                org.robolectric.util.ReflectionHelpers.ClassParameter.from(File::class.java, apk),
            )
        }
        Updater.onStatus(app, status(PackageInstaller.STATUS_FAILURE_ABORTED, session = 5))
        assertTrue(steps().isEmpty())
    }

    @Test
    fun installerAnswersAreSteps() {
        Updater.visible = { true }
        committed(7)
        Updater.onStatus(app, status(PackageInstaller.STATUS_SUCCESS, session = 7))
        Updater.onStatus(app, status(PackageInstaller.STATUS_FAILURE_CONFLICT, session = 8))
        Updater.onStatus(app, status(PackageInstaller.STATUS_PENDING_USER_ACTION, Intent(Intent.ACTION_VIEW), session = 9))
        assertEquals(listOf("installed", "failed:install", "failed:confirm"), steps())
    }

    // AppLog.e with a throwable is what Crash.record files as a non-fatal.
    private fun reported() = ShadowLog.getLogsForTag(AppLog.TAG).filter { it.type == Log.ERROR && it.throwable != null }

    private val vpnPrefs get() = app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)

    @Test
    fun resumesTheTunnelAfterAnUpdateOnlyWhenWanted() {
        val replaced = Intent(Intent.ACTION_MY_PACKAGE_REPLACED)
        vpnPrefs.edit().putBoolean(TunnelVpnService.KEY_WANTED, false).commit()
        BootReceiver().onReceive(app, replaced)
        assertNull(shadowOf(app).nextStartedService)

        vpnPrefs.edit().putBoolean(TunnelVpnService.KEY_WANTED, true).commit()
        BootReceiver().onReceive(app, replaced)
        assertEquals(TunnelVpnService.ACTION_START, shadowOf(app).nextStartedService?.action)
    }

    /** min_version dropped wanted: the update it asked for brings the tunnel back, once. */
    @Test
    fun resumesAfterTheUpdateThatMinVersionAskedFor() {
        vpnPrefs.edit().putBoolean(TunnelVpnService.KEY_WANTED, false).putBoolean(TunnelVpnService.KEY_RESUME_AFTER_UPDATE, true).commit()
        BootReceiver().onReceive(app, Intent(Intent.ACTION_BOOT_COMPLETED))
        assertNull(shadowOf(app).nextStartedService)

        BootReceiver().onReceive(app, Intent(Intent.ACTION_MY_PACKAGE_REPLACED))
        assertEquals(TunnelVpnService.ACTION_START, shadowOf(app).nextStartedService?.action)
        assertFalse(vpnPrefs.getBoolean(TunnelVpnService.KEY_RESUME_AFTER_UPDATE, true))
        BootReceiver().onReceive(app, Intent(Intent.ACTION_MY_PACKAGE_REPLACED))
        assertNull(shadowOf(app).nextStartedService)
    }

    // Mirrors (apk_urls).

    private val gh = "https://bakuridzeaslan025-max.github.io/split-vpn-dl/"
    private val gl = "https://split-vpn-dl-547fcb.gitlab.io/"

    private fun mirrorsJson(vararg m: Pair<String, Boolean>) =
        JSONArray(m.map { JSONObject().put("base", it.first).put("probe", it.second) }).toString()

    /** rc/tool refuses what the app drops: one vector for both. */
    @Test
    fun parsesApkUrlsAsRcToolChecksThem() {
        val cases = JSONArray(File(System.getProperty("vectors.apkUrls")!!).readText())
        assertTrue(cases.length() > 5)
        for (k in 0 until cases.length()) {
            val c = cases.getJSONObject(k)
            val name = c.getString("name")
            val raw = if (c.has("raw")) c.getString("raw") else c.get("in").toString()
            val got = Updater.parseMirrors(raw)
            if (c.optBoolean("error")) {
                assertNull(name, got)
                continue
            }
            assertNotNull(name, got)
            val keep = c.getJSONArray("keep")
            assertEquals(name, (0 until keep.length()).map { keep.getJSONObject(it).let { m -> m.getString("base") to m.getBoolean("probe") } },
                got!!.list.map { it.base to it.probe })
            val dropped = c.getJSONArray("dropped")
            assertEquals(name, (0 until dropped.length()).map { dropped.getInt(it) }, got.dropped)
            // Places in RC's list, for self_update's detail.
            assertEquals(name, (1..keep.length() + dropped.length()).filter { it !in got.dropped }, got.list.map { it.index })
        }
    }

    /** The page and self-update choose alike: docs/test/download.test.mjs runs the same vectors. */
    @Test
    fun choosesAsTheDownloadPage() {
        val vec = JSONObject(File(System.getProperty("vectors.pick")!!).readText())
        val ms = vec.getJSONArray("mirrors").let { a ->
            (0 until a.length()).map { a.getJSONObject(it).let { m -> Updater.Mirror(m.getString("base"), m.getBoolean("probe"), it + 1) } }
        }
        val cases = vec.getJSONArray("cases")
        assertTrue(cases.length() > 10)
        for (k in 0 until cases.length()) {
            val c = cases.getJSONObject(k)
            val name = c.getString("name")
            val versioned = if (c.has("versioned")) c.optString("versioned").takeUnless { c.isNull("versioned") } else vec.getString("versioned")
            val probes = c.getJSONArray("probes").let { a ->
                (0 until a.length()).map { j ->
                    val p = a.getJSONObject(j)
                    val h = p.getJSONObject("heads")
                    val heads = buildMap {
                        for ((key, file) in listOf("v" to versioned, "f" to Updater.FIXED)) {
                            if (file == null || !h.has(key)) continue
                            put(file, h.get(key).let { v -> if (v == "net") Updater.NET else v as Int })
                        }
                    }
                    Updater.Probed(heads, if (p.isNull("passed")) null else p.getBoolean("passed"), p.getBoolean("http"), p.getLong("bytes"), p.getBoolean("alive"))
                }
            }
            val got = Updater.decide(ms, versioned, probes)
            if (c.opt("want") == "pending") {
                assertNull(name, got)
                continue
            }
            val want = c.getJSONObject("want")
            assertNotNull(name, got)
            assertEquals(name, if (want.isNull("url")) null else want.getString("url"), got!!.url)
            assertEquals(name, want.getBoolean("checked"), got.checked)
            val failover = c.getJSONArray("failover")
            assertEquals(name, (0 until failover.length()).map { failover.getString(it) }, got.candidates.map { it.url })
            for (cand in got.candidates) assertEquals(name, ms.single { cand.url.startsWith(it.base) }.index, cand.mirror)
        }
    }

    @Test
    fun acceptsOnlyOurMirrors() {
        assertTrue(Updater.mirrorOk(gh, true))
        assertTrue(Updater.mirrorOk(gl, false))
        assertTrue(Updater.mirrorOk(Updater.RELEASES + "latest/download/", false))
        assertTrue(Updater.mirrorOk(Updater.RELEASES + "download/v0.12.3/", false))
        assertFalse("Releases has no probe.bin", Updater.mirrorOk(Updater.RELEASES + "latest/download/", true))
        assertFalse(Updater.mirrorOk("http://127.0.0.1:8080/", false))
        assertEquals("split-vpn-0.12.4.apk", Updater.versioned("0.12.4"))
        for (bad in listOf("", "0.12", "v0.12.4", "0.12.4-debug", "0.12.4/../x", "1.2.3?")) assertNull(bad, Updater.versioned(bad))
    }

    /** apk_urls blank: apk_url as before; [] off; no list at all: apk_url. */
    @Test
    fun apkUrlsOrApkUrl() {
        assertTrue(Updater.enabled(Versions(apk = url)))
        assertFalse(Updater.enabled(Versions()))
        assertFalse(Updater.enabled(Versions(apk = url, apks = "[]")))
        assertFalse(Updater.enabled(Versions(apk = url, apks = " [ ] ")))
        assertTrue(Updater.enabled(Versions(apks = mirrorsJson(gh to true))))
        assertTrue("all dropped is BAD_URL, not off", Updater.enabled(Versions(apks = mirrorsJson("https://example.org/" to true))))
        assertTrue(Updater.enabled(Versions(apk = url, apks = "garbage")))
        assertFalse(Updater.enabled(Versions(apks = "garbage")))

        Versions.debug = false
        shadowOf(app.packageManager).setCanRequestPackageInstalls(true)
        val latest = Long.MAX_VALUE
        assertEquals(Updater.Result.DISABLED, Updater.update(app, Versions(latest = latest, apk = url, apks = "[]")))
        assertEquals(Updater.Result.BAD_URL, Updater.update(app, Versions(latest = latest, apk = url, apks = mirrorsJson("https://example.org/" to true, gh + "x/" to true))))
        assertEquals("one report for the list", 1, reported().size)
        assertTrue(Updater.lastFailure!!.contains("apk_urls entries 1,2"))
        // Garbage is apk_url's: here a bad one.
        assertEquals(Updater.Result.BAD_URL, Updater.update(app, Versions(latest = latest, apk = "https://example.org/a.apk", apks = "garbage")))
        // A bad entry beside good ones: reported, the rest goes on.
        ShadowLog.clear()
        shadowOf(app.packageManager).setCanRequestPackageInstalls(false)
        assertEquals(Updater.Result.NEED_PERMISSION, Updater.update(app, Versions(latest = latest, apks = mirrorsJson(gh to true, "https://example.org/" to true))))
        assertTrue(reported().single().throwable.message!!.contains("apk_urls entries 2"))
    }

    private val v = "split-vpn-1.2.3.apk"
    // Wide margins around each event a test relies on: CI runners stall threads
    // for hundreds of ms. A test pays the deadline only when its claim needs it.
    private val wide = Updater.Timing(probeMs = 2_000, aliveMs = 300, connectMs = 5_000, readMs = 1_000, uncheckedReadMs = 4_000)

    // An APK per mirror, told apart by its bytes.
    private fun apkOf(m: String) = ByteArray(50_000) { m[0].code.toByte() }

    private fun Socket.ok(method: String, body: ByteArray) =
        reply("HTTP/1.1 200 OK\r\nContent-Length: ${body.size}", if (method == "HEAD") ByteArray(0) else body)

    // count chunks every `every` ms under a head of 32 KB, then silence.
    private fun Socket.drip(chunk: Int, every: Long, count: Int = Int.MAX_VALUE) {
        getOutputStream().apply {
            write("HTTP/1.1 200 OK\r\nContent-Length: 32768\r\nConnection: close\r\n\r\n".toByteArray())
            flush()
            repeat(count) {
                write(ByteArray(chunk))
                flush()
                Thread.sleep(every)
            }
        }
        Thread.sleep(10_000)
    }

    /**
     * Mirrors a, b (probed) and r (Releases) on one local server; [routes]
     * by path replace a well mirror's answers.
     */
    private fun mirrors(routes: Map<String, (String, Socket) -> Unit> = emptyMap()): List<Updater.Mirror> {
        val base = serveRequests { method, path, s ->
            val m = path.removePrefix("/").substringBefore('/')
            val file = path.substringAfterLast('/')
            when {
                path in routes -> routes.getValue(path)(method, s)
                file == "probe.bin" && m != "r" -> s.ok(method, ByteArray(32_768))
                file == v || file == Updater.FIXED -> s.ok(method, apkOf(m))
                else -> s.reply("HTTP/1.1 404 Not Found\r\nContent-Length: 0")
            }
        }
        return listOf(Updater.Mirror("$base/a/", true, 1), Updater.Mirror("$base/b/", true, 2), Updater.Mirror("$base/r/", false, 3))
    }

    private val hang: (String, Socket) -> Unit = { _, _ -> Thread.sleep(10_000) }
    private val http504: (String, Socket) -> Unit = { _, s -> s.reply("HTTP/1.1 504 Gateway Timeout\r\nContent-Length: 0") }

    private fun select(ms: List<Updater.Mirror>, versioned: String? = v, t: Updater.Timing = wide) = Updater.Probes(ms, versioned, t)

    @Test
    fun aProbeStalledAt16KbLosesAtTheDeadline() {
        val ms = mirrors(mapOf("/a/probe.bin" to { _, s -> s.drip(16_700, 0, 1) }))
        val t0 = System.nanoTime()
        val s = select(ms)
        // The 16 700 bytes came at once, 1.7 s of silence before the deadline: not alive.
        assertTrue((System.nanoTime() - t0) / 1_000_000 >= wide.probeMs)
        assertEquals(ms[1].base + v, s.decision.url)
        assertTrue(s.decision.checked)
        assertEquals(mapOf(1 to "stall"), s.tags())
    }

    /** A slow link: alive, short of a pass. The one with more bytes opens unchecked. */
    @Test
    fun aSlowLinkOpensTheAliveMirrorWithMoreBytes() {
        // A chunk every 40 ms is alive by any runner's hiccup within 1 s; b
        // sends ten times as much, short of the 24 KB pass in 1.5 s.
        val ms = mirrors(mapOf("/a/probe.bin" to { _, s -> s.drip(40, 40) }, "/b/probe.bin" to { _, s -> s.drip(400, 40) }))
        val s = select(ms, t = Updater.Timing(probeMs = 1_500, aliveMs = 1_000))
        assertEquals(ms[1].base + v, s.decision.url)
        assertFalse(s.decision.checked)
        assertEquals(listOf(Updater.Candidate(ms[1].base + v, 2, checked = false)), s.decision.candidates)
        assertEquals(mapOf(1 to "slow"), s.tags())
    }

    @Test
    fun http504AndATruncatedProbeLeaveReleasesUnchecked() {
        val ms = mirrors(mapOf(
            "/a/probe.bin" to http504, "/a/$v" to http504, "/a/split-vpn.apk" to http504,
            "/b/probe.bin" to { m, s -> s.ok(m, ByteArray(1_000)) },
        ))
        val t0 = System.nanoTime()
        val s = select(ms)
        assertTrue("decided without the deadline", (System.nanoTime() - t0) / 1_000_000 < wide.probeMs - 200)
        assertEquals(ms[2].base + Updater.FIXED, s.decision.url)
        assertFalse(s.decision.checked)
        assertEquals(mapOf(1 to "http504", 2 to "short"), s.tags())
    }

    /** A body cut off by a reset, as DPI does it, is no connection that failed. */
    @Test
    fun aProbeResetMidwayIsAReset() {
        val ms = mirrors(mapOf("/a/probe.bin" to { _, s ->
            s.getOutputStream().apply {
                write("HTTP/1.1 200 OK\r\nContent-Length: 32768\r\nConnection: close\r\n\r\n".toByteArray())
                write(ByteArray(5_000))
                flush()
            }
            // Time for the app to read the head: a reset before it is no 200.
            Thread.sleep(500)
            s.setSoLinger(true, 0)
            s.close()
        }))
        val s = select(ms)
        assertEquals(ms[1].base + v, s.decision.url)
        assertEquals(mapOf(1 to "reset"), s.tags())
    }

    @Test
    fun aHangingFirstMirrorWaitsForTheDeadline() {
        val ms = mirrors(mapOf("/a/probe.bin" to hang, "/a/$v" to hang, "/a/split-vpn.apk" to hang))
        val s = select(ms)
        assertEquals(ms[1].base + v, s.decision.url)
        assertTrue(s.decision.checked)
        assertEquals(mapOf(1 to "timeout"), s.tags())
    }

    /** The first pass decides at once; the rest run on for backups, up to the deadline and no further. */
    @Test
    fun theFirstPassDecidesTheRestEndAtTheDeadline() {
        val t0 = System.nanoTime()
        val closed = java.util.concurrent.ConcurrentLinkedQueue<Long>()
        // Answers nothing; notes when the app drops the connection.
        val silent: (String, Socket) -> Unit = { _, s ->
            s.soTimeout = 5_000
            runCatching { s.getInputStream().read() }
            closed += (System.nanoTime() - t0) / 1_000_000
        }
        val ms = mirrors(mapOf("/b/probe.bin" to silent, "/b/$v" to silent, "/b/split-vpn.apk" to silent))
        val s = select(ms)
        val decided = (System.nanoTime() - t0) / 1_000_000
        assertTrue("$decided ms", decided < wide.probeMs - 200)
        assertEquals(listOf(Updater.Candidate(ms[0].base + v, 1)), s.decision.candidates)
        assertTrue("nothing known against b yet", s.tags().isEmpty())
        assertEquals(listOf(Updater.Candidate(ms[0].base + v, 1)), s.backups())
        assertTrue((System.nanoTime() - t0) / 1_000_000 >= wide.probeMs)
        assertEquals(mapOf(2 to "timeout"), s.tags())
        val deadline = System.nanoTime() + 4_000_000_000L
        while (closed.size < 3 && System.nanoTime() < deadline) Thread.sleep(20)
        assertEquals(3, closed.size)
        assertTrue("dropped at the deadline: $closed", closed.all { it in wide.probeMs..wide.probeMs + 2_000 })
    }

    @Test
    fun withoutAVersionOnlyTheFixedName() {
        val ms = mirrors(mapOf("/a/split-vpn.apk" to { _, s -> s.reply("HTTP/1.1 404 Not Found\r\nContent-Length: 0") }))
        val s = select(ms, versioned = null)
        assertEquals(listOf(Updater.Candidate(ms[1].base + Updater.FIXED, 2)), s.decision.candidates)
        // The choice can come before a's 404 is recorded; backups() waits for every probe.
        s.backups()
        assertEquals(mapOf(1 to "head404"), s.tags())
    }

    private val installed = mutableListOf<Char>()

    private fun fakeApk(badCertOn: Char? = null) {
        Updater.checkApk = { _, f, _ -> if (f.readBytes().first().toInt().toChar() == badCertOn) "certificate" else null }
        Updater.installer = { _, f -> installed += f.readBytes().first().toInt().toChar() }
    }

    // The first mirror passes last: by the choice the second has passed too.
    // Probes still running then are dropped, and a mirror not known to pass
    // is no candidate.
    private val slowFirst: Pair<String, (String, Socket) -> Unit> = "/a/probe.bin" to { m, s ->
        Thread.sleep(200)
        s.ok(m, ByteArray(32_768))
    }

    private fun run(ms: List<Updater.Mirror>, progress: MutableList<Int> = mutableListOf()) =
        Updater.run(app, 200, Updater.Source.Mirrored(Updater.Mirrors(ms, emptyList()), v), { progress += it }, wide)

    @Test
    fun aDownloadStalledMidApkGoesToTheNextMirrorThatPassed() {
        fakeApk()
        val ms = mirrors(mapOf(slowFirst, "/a/$v" to { m, s ->
            if (m == "HEAD") s.ok(m, apkOf("a")) else s.getOutputStream().apply {
                write("HTTP/1.1 200 OK\r\nContent-Length: 50000\r\nConnection: close\r\n\r\n".toByteArray())
                write(ByteArray(16_000) { 'a'.code.toByte() })
                flush()
                Thread.sleep(10_000)
            }
        }))
        val shown = mutableListOf<Int>()
        assertEquals(Updater.Result.STARTED, run(ms, shown))
        assertEquals(listOf('b'), installed)
        assertEquals(2, Updater.lastMirror)
        assertEquals("1:get-stall", Updater.lastDetail)
        // Back to no percentage before the second mirror.
        assertEquals(-1, shown[shown.indexOfFirst { it == -1 }])
        assertTrue(shown.indexOf(-1) in 1 until shown.lastIndex)
        assertFalse(File(app.cacheDir, "update.apk").exists())
        assertTrue("not a failure of the update", reported().isEmpty())
    }

    /** No socket left in OkHttp's pool after an update: every request asks to close. */
    @Test
    fun everyRequestClosesItsConnection() {
        val ms = mirrors()
        select(ms).backups()
        assertNull(Updater.download(ms[0].base + v, dest))
        // apk_url's: the same function.
        assertNull(Updater.download(ms[2].base + v, dest))
        assertEquals(
            setOf("HEAD /a/$v", "HEAD /a/split-vpn.apk", "GET /a/probe.bin", "HEAD /b/$v", "HEAD /b/split-vpn.apk", "GET /b/probe.bin", "GET /a/$v", "GET /r/$v"),
            closing.toSet(),
        )
    }

    @Test
    fun aWrongCertificateGoesToTheNextMirror() {
        fakeApk(badCertOn = 'a')
        assertEquals(Updater.Result.STARTED, run(mirrors(mapOf(slowFirst))))
        assertEquals(listOf('b'), installed)
        assertEquals("1:cert", Updater.lastDetail)
    }

    /** Releases is only the unchecked last resort; when it fails too, the page. */
    @Test
    fun allFailedIsAFailure() {
        fakeApk(badCertOn = 'r')
        val ms = mirrors(mapOf("/a/probe.bin" to http504, "/b/probe.bin" to { _, s -> s.drip(16_700, 0, 1) }))
        assertEquals(Updater.Result.FAILED, run(ms))
        assertTrue(installed.isEmpty())
        assertEquals(0, Updater.lastMirror)
        assertEquals("1:http504,2:stall,3:cert", Updater.lastDetail)
        assertEquals(Updater.Kind.CHECK, Updater.lastKind)
        assertEquals(1, reported().size)
    }

    // The first mirror stalls mid-APK; the second had not passed yet when the first won.
    private fun stallOnA(bProbe: (String, Socket) -> Unit, getA: java.util.concurrent.CountDownLatch? = null) = mirrors(mapOf("/b/probe.bin" to bProbe, "/a/$v" to { m, s ->
        if (m == "HEAD") s.ok(m, apkOf("a")) else s.getOutputStream().apply {
            getA?.countDown()
            write("HTTP/1.1 200 OK\r\nContent-Length: 50000\r\nConnection: close\r\n\r\n".toByteArray())
            write(ByteArray(16_000) { 'a'.code.toByte() })
            flush()
            Thread.sleep(10_000)
        }
    }))

    @Test
    fun aMirrorPassingAfterTheChoiceIsABackup() {
        fakeApk()
        // b answers once a's APK is asked for: after the choice, whatever the runner's pace.
        val chosen = java.util.concurrent.CountDownLatch(1)
        val ms = stallOnA({ m, s ->
            chosen.await(5, java.util.concurrent.TimeUnit.SECONDS)
            s.ok(m, ByteArray(32_768))
        }, chosen)
        assertEquals(Updater.Result.STARTED, run(ms))
        assertEquals(listOf('b'), installed)
        assertEquals(2, Updater.lastMirror)
        assertEquals("1:get-stall", Updater.lastDetail)
    }

    @Test
    fun aMirrorNotPassedByTheDeadlineIsNoBackup() {
        fakeApk()
        assertEquals(Updater.Result.FAILED, run(stallOnA({ _, s -> s.drip(16_700, 0, 1) })))
        assertEquals("1:get-stall,2:stall", Updater.lastDetail)
    }

    /** A good first mirror does not wait for the others' probes. */
    @Test
    fun theDownloadDoesNotWaitForTheProbes() {
        fakeApk()
        val ms = mirrors(mapOf("/b/probe.bin" to hang, "/b/$v" to hang, "/b/split-vpn.apk" to hang))
        val t0 = System.nanoTime()
        assertEquals(Updater.Result.STARTED, run(ms))
        assertTrue((System.nanoTime() - t0) / 1_000_000 < wide.probeMs - 200)
        assertEquals(listOf('a'), installed)
    }

    /** Releases is slow to the first byte: an unchecked mirror gets longer than a checked one. */
    @Test
    fun anUncheckedMirrorWaitsLongerForTheAnswer() {
        fakeApk()
        // Over the checked readMs (1 s) by as much as under the unchecked one (4 s).
        val slow: (String, Socket) -> Unit = { m, s ->
            Thread.sleep(2_500)
            s.ok(m, apkOf("r"))
        }
        val ms = mirrors(mapOf("/a/probe.bin" to http504, "/b/probe.bin" to http504, "/r/split-vpn.apk" to slow))
        assertEquals(Updater.Result.STARTED, run(ms))
        assertEquals(listOf('r'), installed)
    }

    @Test
    fun noMirrorAnsweringIsAFailureWithoutADownload() {
        fakeApk()
        val port = ServerSocket(0).use { it.localPort }
        val ms = listOf(Updater.Mirror("http://127.0.0.1:$port/a/", true, 1), Updater.Mirror("http://127.0.0.1:$port/r/", false, 2))
        assertEquals(Updater.Result.FAILED, run(ms))
        assertEquals(Updater.Kind.PROBE, Updater.lastKind)
        assertEquals("1:net", Updater.lastDetail)
    }

    /** Firebase cuts values at 100 characters: whole entries only. */
    @Test
    fun detailFitsAnAnalyticsValue() {
        val d = Updater.detail((1..30).associateWith { "get-http504" }.toSortedMap())!!
        assertTrue(d.length <= 100)
        assertTrue(d.endsWith("get-http504"))
        assertNull(Updater.detail(emptyMap()))
    }
}
