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
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.Shadows.shadowOf
import org.robolectric.shadows.ShadowLog
import java.io.File
import java.net.ServerSocket
import java.net.Socket
import kotlin.concurrent.thread

/** Self-update: what may be installed, and how a failed download ends. */
@RunWith(RobolectricTestRunner::class)
class UpdaterTest {

    private val app get() = RuntimeEnvironment.getApplication()
    private val dest get() = File(app.cacheDir, "test.apk")
    private var server: ServerSocket? = null
    private val realVisible = Updater.visible

    @After
    fun tearDown() {
        server?.close()
        Versions.debug = true
        Updater.busy.set(false)
        Updater.takePending(app)
        Updater.visible = realVisible
        ShadowLog.clear()
    }

    /** Serves one canned response per connection, whatever the request. */
    private fun serve(respond: (path: String, Socket) -> Unit): String {
        val s = ServerSocket(0).also { server = it }
        thread(isDaemon = true) {
            while (!s.isClosed) {
                val c = try { s.accept() } catch (_: Exception) { break }
                thread(isDaemon = true) {
                    c.use {
                        // Read the whole request: closing on unread bytes sends a RST.
                        val req = it.getInputStream().bufferedReader()
                        val path = req.readLine().orEmpty().split(' ').getOrElse(1) { "" }
                        while (!req.readLine().isNullOrEmpty()) Unit
                        try { respond(path, it) } catch (_: Exception) {}
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
        assertEquals(Updater.Result.DISABLED, Updater.update(app, Long.MAX_VALUE, url))
    }

    @Test
    fun asksForThePermissionBeforeDownloading() {
        Versions.debug = false
        shadowOf(app.packageManager).setCanRequestPackageInstalls(false)
        assertEquals(Updater.Result.NEED_PERMISSION, Updater.update(app, Long.MAX_VALUE, url))
        assertEquals(Updater.Result.UP_TO_DATE, Updater.update(app, BuildConfig.VERSION_CODE.toLong(), url))
    }

    /** Reported: Remote Config carries a URL the app will not take. */
    @Test
    fun badUrlIsNotAFailureToOffer() {
        Versions.debug = false
        shadowOf(app.packageManager).setCanRequestPackageInstalls(true)
        assertEquals(Updater.Result.BAD_URL, Updater.update(app, Long.MAX_VALUE, "https://example.org/a.apk"))
    }

    @Test
    fun secondCallWhileDownloadingIsBusy() {
        Versions.debug = false
        shadowOf(app.packageManager).setCanRequestPackageInstalls(true)
        Updater.busy.set(true)
        assertEquals(Updater.Result.BUSY, Updater.update(app, Long.MAX_VALUE, url))
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
}
