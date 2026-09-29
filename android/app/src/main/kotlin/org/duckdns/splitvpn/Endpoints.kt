package org.duckdns.splitvpn

import android.content.Context
import org.json.JSONArray
import org.json.JSONException
import org.json.JSONObject
import java.io.File
import java.io.IOException

/**
 * Relay endpoints this build knows. [Crash.redact] masks everything in
 * [known], so a new source of endpoints goes through [remember].
 */
object Endpoints {
    /**
     * Baked in by gradle from local.properties, encrypted with the key only
     * the Go library has. Decrypting loads Go into the process.
     */
    val defaults: List<TunnelVpnService.Endpoint> by lazy { decode(BuildConfig.ENDPOINTS) }

    /**
     * Every endpoint this install has ever had: the defaults, the cache,
     * each list from Remote Config. :vpn writes the file, the UI process
     * only reads it — it never sees Remote Config and does not load Go.
     */
    const val KNOWN_FILE = "known_endpoints.json"
    private const val KEY_CACHE = "endpoints"
    private const val KEY_WORKING = "endpoint.working"

    @Volatile private var knownFile: File? = null
    // Masked even if the file cannot be written.
    @Volatile private var added: List<TunnelVpnService.Endpoint> = emptyList()
    private var readStamp = 0L to 0L
    private var read: List<TunnelVpnService.Endpoint> = emptyList()
    private var readOk = true

    fun attach(ctx: Context) {
        knownFile = File(ctx.filesDir, KNOWN_FILE)
    }

    // Set by remember: :vpn writes the file and holds all of it in added, so
    // only the UI process stats and re-reads it on every masked line.
    @Volatile private var writer = false

    val known: List<TunnelVpnService.Endpoint> get() = if (writer) added else (added + fromFile()).distinct()

    // Re-read when the file changes: :vpn can learn a list while the UI runs.
    // The file only grows, so its length tells two writes in one tick apart.
    @Synchronized
    private fun fromFile(): List<TunnelVpnService.Endpoint> {
        val f = knownFile ?: return emptyList()
        val stamp = f.lastModified() to f.length()
        if (stamp != readStamp) {
            val r = if (f.exists()) runCatching { parse(f.readText()) } else Result.success(emptyList())
            read = r.getOrDefault(emptyList())
            readOk = r.isSuccess
            readStamp = stamp
        }
        return read
    }

    /** :vpn only. Before the list is used, so nothing logs it unmasked. */
    @Synchronized
    fun remember(list: List<TunnelVpnService.Endpoint>) {
        writer = true
        added = (added + list).distinct()
        val f = knownFile ?: return
        val onDisk = fromFile()
        // A file that does not parse is replaced by all this process knows,
        // never by just the new list.
        val all = (onDisk + added).distinct()
        added = all
        if (readOk && all.size == onDisk.size) return
        try {
            val tmp = File(f.path + ".tmp")
            tmp.writeText(toJson(all))
            if (!tmp.renameTo(f)) tmp.delete()
        } catch (_: IOException) {
        }
    }

    /** Unit tests share one process: what an earlier test remembered must not mask for the next. */
    @Synchronized
    internal fun resetForTests() {
        knownFile = null
        writer = false
        added = emptyList()
        readStamp = 0L to 0L
        read = emptyList()
        readOk = true
    }

    private fun prefs(ctx: Context) = ctx.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)

    // The instrumented tests' lists, as with Credentials: kept apart from the real ones.
    private fun key(k: String, test: Boolean) = if (test) "$k.test" else k

    /** The last list from Remote Config that decrypted; null before the first one. */
    fun cached(ctx: Context, test: Boolean = false): List<TunnelVpnService.Endpoint>? =
        prefs(ctx).getString(key(KEY_CACHE, test), null)?.let { runCatching { parse(it) }.getOrNull() }?.takeIf { it.isNotEmpty() }

    fun cache(ctx: Context, list: List<TunnelVpnService.Endpoint>, test: Boolean = false) {
        prefs(ctx).edit().putString(key(KEY_CACHE, test), toJson(list)).apply()
    }

    /** The last endpoint the relay answered on; by content, a new list may reorder. */
    fun working(ctx: Context, test: Boolean = false): TunnelVpnService.Endpoint? =
        prefs(ctx).getString(key(KEY_WORKING, test), null)?.let { runCatching { parse(it) }.getOrNull() }?.firstOrNull()

    fun setWorking(ctx: Context, ep: TunnelVpnService.Endpoint?, test: Boolean = false) {
        val k = key(KEY_WORKING, test)
        prefs(ctx).edit().apply { if (ep == null) remove(k) else putString(k, toJson(listOf(ep))) }.apply()
    }

    /**
     * Why [defaults] came out empty, for the start error. Not logged from
     * here: logging passes [Crash.redact], which asks for [known] again.
     */
    @Volatile var failure: String? = null
        private set

    internal fun decode(blob: String, decrypt: (String) -> String = tunnel.Tunnel::decryptEndpoints): List<TunnelVpnService.Endpoint> {
        if (blob.isEmpty()) {
            failure = "none baked in at build time"
            return emptyList()
        }
        return open(blob, decrypt).onSuccess { failure = null }.getOrElse {
            failure = it.message
            emptyList()
        }
    }

    /** The list in [blob], or a failure whose message names no address. */
    internal fun open(blob: String, decrypt: (String) -> String): Result<List<TunnelVpnService.Endpoint>> =
        try {
            Result.success(parse(decrypt(blob)))
        } catch (e: Throwable) { // UnsatisfiedLinkError too: no Go under unit tests
            // org.json quotes the offending value, and that may be an address.
            Result.failure(Exception(if (e is JSONException) "bad JSON" else e.message?.removePrefix("endpoints: ") ?: e.javaClass.name))
        }

    internal fun parse(json: String): List<TunnelVpnService.Endpoint> {
        val a = JSONArray(json)
        return List(a.length()) { i ->
            a.getJSONObject(i).run { TunnelVpnService.Endpoint(getString("host"), getString("ip"), getInt("port"), getString("path")) }
        }
    }

    private fun toJson(list: List<TunnelVpnService.Endpoint>) = JSONArray(
        list.map { JSONObject().put("host", it.host).put("ip", it.ip).put("port", it.port).put("path", it.path) },
    ).toString()
}
