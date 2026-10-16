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
 * Self-update from GitHub releases: download the APK named by Remote
 * Config's `apk_url`, check it, hand it to PackageInstaller. Runs in the UI
 * process: the confirmation dialog is an activity, and :vpn has no window to
 * start it from.
 */
object Updater {
    const val RELEASES = "https://github.com/bakuridzeaslan025-max/split-vpn/releases/"
    private const val MAX_BYTES = 64L shl 20
    private const val FILE = "update.apk"
    private const val CHANNEL_ID = "update"
    // 1 and 2 are TunnelVpnService's.
    private const val NOTIFICATION_ID = 3
    // Hidden API: CONFIRM_INSTALL since Android 10, CONFIRM_PERMISSIONS before.
    private val confirmActions = setOf("android.content.pm.action.CONFIRM_INSTALL", "android.content.pm.action.CONFIRM_PERMISSIONS")

    enum class Result { STARTED, NEED_PERMISSION, UP_TO_DATE, DISABLED, BUSY, BAD_URL, FAILED }

    /** Grouped on the dashboard by kind; the details go into the message. */
    enum class Kind(val tag: String) { TIMEOUT("timeout"), HTTP("http"), BROKEN("broken"), CHECK("check"), INSTALL("install") }

    internal class Failure(val kind: Kind, val detail: String, val bytes: Long = 0, val ms: Long = 0)

    internal class Pkg(val name: String, val versionCode: Long, val certs: Set<String>)

    internal val busy = AtomicBoolean()
    // The installer's dialog that came while the app was off screen: the
    // next onResume starts it, from the activity, where nothing blocks it.
    @Volatile
    private var pending: Pair<Int, Intent>? = null
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
     * waits for [takePending] and in a notification when the app is no longer
     * on screen. [Result.BUSY]: another call is still downloading. Whatever
     * else, the UI's way out is the download page, never [apkUrl] itself.
     */
    fun update(ctx: Context, latestVersion: Long, apkUrl: String, progress: (Int) -> Unit = {}): Result {
        // A debug build shares the package and the key with release: updating it
        // would put release over debug without anyone asking.
        if (Versions.debug) return Result.DISABLED
        if (latestVersion <= BuildConfig.VERSION_CODE) return Result.UP_TO_DATE
        if (!urlOk(apkUrl)) {
            report(Failure(Kind.CHECK, "url"))
            return Result.BAD_URL
        }
        if (!canInstall(ctx)) return Result.NEED_PERMISSION
        if (!busy.compareAndSet(false, true)) return Result.BUSY
        val apk = File(ctx.cacheDir, FILE)
        lastFailure = null
        val t0 = System.nanoTime()
        try {
            download(apkUrl, apk, progress = progress)?.let {
                report(it)
                return Result.FAILED
            }
            verify(self(ctx), archive(ctx, apk), latestVersion)?.let {
                report(Failure(Kind.CHECK, it, apk.length(), (System.nanoTime() - t0) / 1_000_000))
                return Result.FAILED
            }
            install(ctx, apk)
            return Result.STARTED
        } catch (e: Exception) {
            report(Failure(Kind.INSTALL, e.javaClass.simpleName, apk.length(), (System.nanoTime() - t0) / 1_000_000))
            return Result.FAILED
        } finally {
            // PackageInstaller has its own copy once the session is written.
            apk.delete()
            busy.set(false)
        }
    }

    // Charset excludes '/', '?', '#', '%', '@': exactly an asset of a tag, or
    // of the latest release, of our repo.
    private val asset = Regex("""(?:download/[A-Za-z0-9_.+-]+|latest/download)/[A-Za-z0-9_.+-]+\.apk""")

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

    /**
     * Null on success; on failure [dest] is gone. Follows GitHub's redirect to
     * its CDN. [progress]: percent, on each change, when the size is known.
     */
    internal fun download(url: String, dest: File, connectMs: Int = 15_000, readMs: Int = 30_000, progress: (Int) -> Unit = {}): Failure? {
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
                        var shown = -1
                        while (true) {
                            val n = inp.read(buf)
                            if (n < 0) break
                            out.write(buf, 0, n)
                            bytes += n
                            if (bytes > MAX_BYTES) return fail(Kind.CHECK, "too big")
                            if (len > 0) {
                                val pct = (bytes * 100 / len).toInt().coerceAtMost(100)
                                if (pct != shown) progress(pct.also { shown = it })
                            }
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

    /** The last [update]'s failure, for the user's log: the UI process has no file log of its own. */
    @Volatile
    var lastFailure: String? = null
        private set

    private fun report(f: Failure) {
        val e = failure(f)
        lastFailure = e.message
        AppLog.e("update failed", e)
    }

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
        pending = null
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
        val st = intent.getIntExtra(PackageInstaller.EXTRA_STATUS, PackageInstaller.STATUS_FAILURE)
        val session = intent.getIntExtra(PackageInstaller.EXTRA_SESSION_ID, -1)
        // Answered from the notification: coming back to the app must not ask
        // again. Not by an abandoned session's ABORTED, though.
        if (st != PackageInstaller.STATUS_PENDING_USER_ACTION && pending?.first == session) pending = null
        when (st) {
            PackageInstaller.STATUS_PENDING_USER_ACTION ->
                confirm(ctx, session, IntentCompat.getParcelableExtra(intent, Intent.EXTRA_INTENT, Intent::class.java))
            PackageInstaller.STATUS_SUCCESS -> {
                AppLog.i("update: installed")
                installed(ctx)
            }
            PackageInstaller.STATUS_FAILURE_ABORTED -> AppLog.i("update: cancelled by the user")
            else -> report(Failure(Kind.INSTALL, "status $st"))
        }
    }

    private fun confirm(ctx: Context, session: Int, confirm: Intent?) {
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
        pending = session to confirm
        // Without POST_NOTIFICATIONS this shows nothing; coming back to the app still does.
        notify(ctx, "Обновление готово", "Нажмите, чтобы установить", confirm)
    }

    // The install killed the app, and Android starts no activity from the
    // background: this status comes to a fresh process, the tap reopens the app.
    private fun installed(ctx: Context) {
        if (visible(ctx)) return
        // As the launcher's: getLaunchIntentForPackage sets the package, and on
        // Android 8-10 a task started by it makes the icon stack a second activity.
        val open = Intent(Intent.ACTION_MAIN).addCategory(Intent.CATEGORY_LAUNCHER)
            .setClass(ctx, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK)
        notify(ctx, "Split VPN обновлён до ${BuildConfig.VERSION_NAME}", "Нажмите, чтобы открыть", open)
    }

    private fun notify(ctx: Context, title: String, text: String, open: Intent) {
        val nm = ctx.getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(NotificationChannel(CHANNEL_ID, "Обновления", NotificationManager.IMPORTANCE_DEFAULT))
        val pi = PendingIntent.getActivity(ctx, 0, open, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT)
        nm.notify(
            NOTIFICATION_ID,
            Notification.Builder(ctx, CHANNEL_ID)
                .setContentTitle(title)
                .setContentText(text)
                .setSmallIcon(android.R.drawable.stat_sys_download_done)
                .setContentIntent(pi)
                .setAutoCancel(true)
                .build(),
        )
    }

    /**
     * For the app on screen: the dialog [confirm] put off, once. The update
     * notification goes either way, its job is done.
     */
    fun takePending(ctx: Context): Intent? {
        ctx.getSystemService(NotificationManager::class.java).cancel(NOTIFICATION_ID)
        val (_, i) = pending ?: return null
        pending = null
        return i
    }
}

/** PackageInstaller's answer to a commit from [Updater]. */
class InstallReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) = Updater.onStatus(ctx, intent)
}
