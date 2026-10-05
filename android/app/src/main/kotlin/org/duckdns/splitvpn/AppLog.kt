package org.duckdns.splitvpn

import android.content.Context
import android.util.Log
import java.io.File
import java.io.FileNotFoundException
import java.io.FileOutputStream
import java.io.IOException
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/**
 * logcat + file. On MIUI the 2 MB logcat buffer is gone within minutes, so
 * this is the only way to read what happened during a network switch or a
 * "clear all" kill. Owned by the :vpn process; the UI only reads the files.
 */
object AppLog {
    const val TAG = "SplitVpn"
    const val DIR = "logs"
    const val FILE = "vpn.log"
    const val ROTATED = "vpn.log.1"
    const val MAX_BYTES = 1L shl 20
    const val SHARE_DIR = "share"

    /** Held for the lifetime of the process: Go keeps calling it from its own
     * goroutines, long after whoever passed it in has returned. */
    val goLogger: tunnel.Logger = object : tunnel.Logger {
        override fun log(line: String) = go(line)
    }

    private var dir: File? = null
    private val fmt = SimpleDateFormat("MM-dd HH:mm:ss.SSS", Locale.US)

    fun dir(ctx: Context) = File(ctx.filesDir, DIR)

    /** Existing log files, newest first. */
    fun files(ctx: Context): List<File> =
        listOf(FILE, ROTATED).map { File(dir(ctx), it) }.filter { it.exists() }

    /**
     * Both files as one, oldest line first, under [header]. Sent as two, they
     * reach a messenger as "vpn.log (1)" and "vpn.log.1 (2)" with no hint
     * which is older. Null when nothing has been logged yet.
     */
    fun export(ctx: Context, header: String, now: Date = Date()): File? {
        val parts = files(ctx).reversed()
        if (parts.isEmpty()) return null
        val out = File(ctx.cacheDir, SHARE_DIR)
        out.deleteRecursively()
        out.mkdirs()
        val name = SimpleDateFormat("yyyy-MM-dd-HHmm", Locale.US).format(now)
        return File(out, "split-vpn-$name.log").also { f ->
            f.outputStream().use { o ->
                o.write("$header\n".toByteArray())
                for (p in parts) {
                    // :vpn may be renaming vpn.log to vpn.log.1 right now.
                    try {
                        p.inputStream().use { it.copyTo(o) }
                    } catch (_: FileNotFoundException) {
                    }
                }
            }
        }
    }

    @Synchronized
    fun init(ctx: Context) {
        dir = dir(ctx).also { it.mkdirs() }
    }

    fun i(msg: String) {
        Log.i(TAG, msg)
        append('I', msg)
        Crash.log(msg)
    }

    /**
     * [expected] marks the failures that are part of normal life — no network,
     * no VPN permission — and that the UI already shows. They stay a
     * breadcrumb: filing them as non-fatals would fill the crash dashboard
     * with a commuter's trip through the metro.
     */
    fun e(msg: String, t: Throwable? = null, expected: Boolean = false) {
        Log.e(TAG, msg, t)
        append('E', if (t == null) msg else "$msg\n${Log.getStackTraceString(t)}".trimEnd())
        if (expected) Crash.log(msg) else Crash.record(msg, t)
    }

    /** Lines from the Go stack; they already went to logcat via gomobile. */
    fun go(line: String) {
        append('G', line)
        when {
            // A recovered panic carries its own stack and deserves a report of
            // its own: the tunnel survived, so nothing else would ever file one.
            line.startsWith(tunnel.Tunnel.PanicPrefix) -> Crash.record(line, null)
            Crash.namesSites(line) -> Unit // stays on the device
            else -> Crash.log(line)
        }
    }

    @Synchronized
    private fun append(level: Char, msg: String) {
        val d = dir ?: return
        val f = File(d, FILE)
        try {
            if (f.length() > MAX_BYTES) f.renameTo(File(d, ROTATED))
            FileOutputStream(f, true).bufferedWriter().use {
                it.write(fmt.format(Date()))
                it.write(" $level ")
                it.write(Crash.mask(msg))
                it.write("\n")
            }
        } catch (_: IOException) {
        }
    }
}
