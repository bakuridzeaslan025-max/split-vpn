package org.duckdns.splitvpn

import android.app.ActivityManager
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.PackageInfo
import android.content.pm.PackageInstaller
import android.content.pm.PackageManager
import android.net.Uri
import android.os.Build
import android.provider.Settings
import androidx.core.content.IntentCompat
import java.io.File
import java.io.IOException
import java.net.HttpURLConnection
import java.net.SocketTimeoutException
import java.net.URL
import java.security.MessageDigest
import java.util.concurrent.atomic.AtomicBoolean

/**
 * Self-update from GitHub releases: download the
 * APK named by Remote Config's `update_url`, check it, hand it to
 * PackageInstaller. Runs in the UI process: the confirmation dialog is an
 * activity, and :vpn has no window to start it from.
 */
object Updater {
    const val RELEASES_PAGE = "https://github.com/bakuridzeaslan025-max/split-vpn/releases"
    const val RELEASES = "$RELEASES_PAGE/download/"
    private const val MAX_BYTES = 64L shl 20
    private const val FILE = "update.apk"
    private const val CHANNEL_ID = "update"
    private const val NOTIFICATION_ID = 2
    // Hidden API: CONFIRM_INSTALL since Android 10, CONFIRM_PERMISSIONS before.
    private val confirmActions = setOf("android.content.pm.action.CONFIRM_INSTALL", "android.content.pm.action.CONFIRM_PERMISSIONS")

    enum class Result { STARTED, NEED_PERMISSION, UP_TO_DATE, DISABLED, BUSY, BAD_URL, FAILED }

    /** Grouped on the dashboard by kind; the details go into the message. */
    enum class Kind(val tag: String) { TIMEOUT("timeout"), HTTP("http"), BROKEN("broken"), CHECK("check"), INSTALL("install") }

    internal class Failure(val kind: Kind, val detail: String, val bytes: Long = 0, val ms: Long = 0)

    internal class Pkg(val name: String, val versionCode: Long, val certs: Set<String>)

    // A debug build shares the package and the key with release: updating it
    // would put release over debug without anyone asking.
    internal var debug = BuildConfig.DEBUG
    internal val busy = AtomicBoolean()
    internal var visible: (Context) -> Boolean = {
        ActivityManager.RunningAppProcessInfo().also(ActivityManager::getMyMemoryState).importance ==
            ActivityManager.RunningAppProcessInfo.IMPORTANCE_FOREGROUND
    }

    fun canInstall(ctx: Context) = ctx.packageManager.canRequestPackageInstalls()

    /** For [Result.NEED_PERMISSION]: the system screen that grants it. */
    fun permissionIntent(ctx: Context) =
        Intent(Settings.ACTION_MANAGE_UNKNOWN_APP_SOURCES, Uri.parse("package:${ctx.packageName}"))

    /**
     * Blocks for the whole download: never on the main thread. [Result.STARTED]
     * means the system took the APK; the confirmation pops up by itself, or
     * comes as a notification when the app is no longer on screen.
     * [Result.FAILED]: the UI may offer [updateUrl] in the browser, it passed
     * [urlOk]. [Result.BAD_URL]: never open [updateUrl], at most [RELEASES_PAGE].
     * [Result.BUSY]: another call is still downloading.
     */
    fun update(ctx: Context, latestVersion: Long, updateUrl: String): Result {
        if (debug) return Result.DISABLED
        if (latestVersion <= BuildConfig.VERSION_CODE) return Result.UP_TO_DATE
        if (!urlOk(updateUrl)) {
            report(Failure(Kind.CHECK, "url"))
            return Result.BAD_URL
        }
        if (!canInstall(ctx)) return Result.NEED_PERMISSION
        if (!busy.compareAndSet(false, true)) return Result.BUSY
        val apk = File(ctx.cacheDir, FILE)
        try {
            download(updateUrl, apk)?.let {
                report(it)
                return Result.FAILED
            }
            verify(self(ctx), archive(ctx, apk), latestVersion)?.let {
                report(Failure(Kind.CHECK, it, apk.length()))
                return Result.FAILED
            }
            install(ctx, apk)
            return Result.STARTED
        } catch (e: Exception) {
            report(Failure(Kind.INSTALL, e.javaClass.simpleName, apk.length()))
            return Result.FAILED
        } finally {
            // PackageInstaller has its own copy once the session is written.
            apk.delete()
            busy.set(false)
        }
    }

    // Charset excludes '/', '?', '#', '%', '@': exactly <tag>/<file>.apk of our repo.
    private val asset = Regex("""[A-Za-z0-9_.+-]+/[A-Za-z0-9_.+-]+\.apk""")

    internal fun urlOk(url: String) =
        url.startsWith(RELEASES) && asset.matches(url.removePrefix(RELEASES)) && ".." !in url

    /** Null when [apk] may replace [self]; otherwise which check failed. */
    internal fun verify(self: Pkg, apk: Pkg?, latestVersion: Long): String? = when {
        apk == null -> "not an apk"
        apk.name != self.name -> "package"
        // The archive may carry a rotation history (apksigner --lineage);
        // our current key has to be in it.
        self.certs.isEmpty() || !apk.certs.containsAll(self.certs) -> "certificate"
        apk.versionCode <= self.versionCode -> "not newer"
        apk.versionCode < latestVersion -> "older than latest"
        else -> null
    }

    /** Null on success; on failure [dest] is gone. Follows GitHub's redirect to its CDN. */
    internal fun download(url: String, dest: File, connectMs: Int = 15_000, readMs: Int = 30_000): Failure? {
        val t0 = System.nanoTime()
        var bytes = 0L
        fun fail(kind: Kind, detail: String): Failure {
            dest.delete()
            return Failure(kind, detail, bytes, (System.nanoTime() - t0) / 1_000_000)
        }
        try {
            val c = URL(url).openConnection() as HttpURLConnection
            try {
                c.connectTimeout = connectMs
                c.readTimeout = readMs
                c.instanceFollowRedirects = true
                val code = c.responseCode
                if (code != HttpURLConnection.HTTP_OK) return fail(Kind.HTTP, "HTTP $code")
                val len = c.contentLengthLong
                if (len > MAX_BYTES) return fail(Kind.CHECK, "too big")
                c.inputStream.use { inp ->
                    dest.outputStream().use { out ->
                        val buf = ByteArray(64 shl 10)
                        while (true) {
                            val n = inp.read(buf)
                            if (n < 0) break
                            out.write(buf, 0, n)
                            bytes += n
                            if (bytes > MAX_BYTES) return fail(Kind.CHECK, "too big")
                        }
                    }
                }
                if (len >= 0 && bytes != len) return fail(Kind.BROKEN, "short body")
            } finally {
                c.disconnect()
            }
        } catch (_: SocketTimeoutException) {
            return fail(Kind.TIMEOUT, "timeout")
        } catch (e: IOException) {
            // The class only: messages of network errors name the host.
            return fail(Kind.BROKEN, e.javaClass.simpleName)
        }
        return null
    }

    /** Non-fatal with a frame of its own per kind, so each kind is its own issue. */
    internal fun failure(f: Failure) =
        RuntimeException("update ${f.kind.tag}: ${f.detail}, ${f.bytes} bytes in ${f.ms} ms").apply {
            stackTrace = arrayOf(StackTraceElement(Updater::class.java.name, f.kind.tag, "Updater.kt", 0))
        }

    private fun report(f: Failure) = AppLog.e("update failed", failure(f))

    @Suppress("DEPRECATION")
    private val signFlags =
        if (Build.VERSION.SDK_INT >= 28) PackageManager.GET_SIGNING_CERTIFICATES else PackageManager.GET_SIGNATURES

    private fun self(ctx: Context) =
        pkg(ctx.packageManager.getPackageInfo(ctx.packageName, signFlags), history = false)

    private fun archive(ctx: Context, apk: File) =
        ctx.packageManager.getPackageArchiveInfo(apk.path, signFlags)?.let { pkg(it, history = true) }

    @Suppress("DEPRECATION")
    private fun pkg(p: PackageInfo, history: Boolean): Pkg {
        val sigs = if (Build.VERSION.SDK_INT >= 28) {
            p.signingInfo?.let { s ->
                if (history && !s.hasMultipleSigners()) s.signingCertificateHistory else s.apkContentsSigners
            }
        } else {
            p.signatures
        }
        val sha = MessageDigest.getInstance("SHA-256")
        val certs = sigs.orEmpty().map { sig -> sha.digest(sig.toByteArray()).joinToString("") { "%02x".format(it) } }
        val code = if (Build.VERSION.SDK_INT >= 28) p.longVersionCode else p.versionCode.toLong()
        return Pkg(p.packageName, code, certs.toSet())
    }

    private fun install(ctx: Context, apk: File) {
        val installer = ctx.packageManager.packageInstaller
        val params = PackageInstaller.SessionParams(PackageInstaller.SessionParams.MODE_FULL_INSTALL).apply {
            setAppPackageName(ctx.packageName)
            setSize(apk.length())
            // Only once we are the installer of record: the first update still asks.
            if (Build.VERSION.SDK_INT >= 31) setRequireUserAction(PackageInstaller.SessionParams.USER_ACTION_NOT_REQUIRED)
        }
        // One left over from an unconfirmed attempt holds a copy of an older APK.
        // Their status comes back as STATUS_FAILURE_ABORTED, which is not reported.
        for (old in installer.mySessions) runCatching { installer.abandonSession(old.sessionId) }
        ctx.getSystemService(NotificationManager::class.java).cancel(NOTIFICATION_ID)
        val id = installer.createSession(params)
        installer.openSession(id).use { s ->
            try {
                s.openWrite("base.apk", 0, apk.length()).use { out ->
                    apk.inputStream().use { it.copyTo(out) }
                    s.fsync(out)
                }
                // Mutable: the system puts the status and the confirmation intent in.
                val flags = PendingIntent.FLAG_UPDATE_CURRENT or (if (Build.VERSION.SDK_INT >= 31) PendingIntent.FLAG_MUTABLE else 0)
                val status = PendingIntent.getBroadcast(ctx, id, Intent(ctx, InstallReceiver::class.java), flags)
                s.commit(status.intentSender)
            } catch (e: Exception) {
                s.abandon()
                throw e
            }
        }
        AppLog.i("update: session committed")
    }

    internal fun onStatus(ctx: Context, intent: Intent) {
        when (val st = intent.getIntExtra(PackageInstaller.EXTRA_STATUS, PackageInstaller.STATUS_FAILURE)) {
            PackageInstaller.STATUS_PENDING_USER_ACTION ->
                confirm(ctx, IntentCompat.getParcelableExtra(intent, Intent.EXTRA_INTENT, Intent::class.java))
            PackageInstaller.STATUS_SUCCESS -> AppLog.i("update: installed")
            PackageInstaller.STATUS_FAILURE_ABORTED -> AppLog.i("update: cancelled by the user")
            else -> report(Failure(Kind.INSTALL, "status $st"))
        }
    }

    private fun confirm(ctx: Context, confirm: Intent?) {
        // It arrives through a mutable PendingIntent: start only the system's own dialog.
        if (confirm == null || confirm.action !in confirmActions) {
            report(Failure(Kind.INSTALL, "confirm action ${confirm?.action}"))
            return
        }
        confirm.addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        // Android 10+ drops activity starts from the background without a word.
        if (visible(ctx)) {
            ctx.startActivity(confirm)
            return
        }
        // Without POST_NOTIFICATIONS this shows nothing; the next tap on "Обновить" asks again.
        val nm = ctx.getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(NotificationChannel(CHANNEL_ID, "Обновления", NotificationManager.IMPORTANCE_DEFAULT))
        val pi = PendingIntent.getActivity(ctx, 0, confirm, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        nm.notify(
            NOTIFICATION_ID,
            Notification.Builder(ctx, CHANNEL_ID)
                .setContentTitle("Обновление готово")
                .setContentText("Нажмите, чтобы установить")
                .setSmallIcon(android.R.drawable.stat_sys_download_done)
                .setContentIntent(pi)
                .setAutoCancel(true)
                .build(),
        )
    }
}

/** PackageInstaller's answer to a commit from [Updater]. */
class InstallReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) = Updater.onStatus(ctx, intent)
}
