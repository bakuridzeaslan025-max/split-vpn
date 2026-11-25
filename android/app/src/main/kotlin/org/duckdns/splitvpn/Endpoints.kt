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
    private const val KEY_BY_NET = "endpoint.by_net"
    // Before by_net: one endpoint for every network. Removed, never read.
    private const val KEY_WORKING = "endpoint.working"
    private const val MAX_NETS = 16
    // As direct YouTube's noneFor: the carrier may let the first one through again.
    internal const val NET_TTL_MS = 7 * 86_400_000L

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

    /**
     * The endpoint the relay last answered on over network [net], as [list]'s
     * own entry: by content, a new list may reorder. One gone from [list] is
     * forgotten, and so is one past the first after [NET_TTL_MS].
     */
    @Synchronized
    fun forNet(ctx: Context, net: String, list: List<TunnelVpnService.Endpoint>, test: Boolean = false, now: Long = System.currentTimeMillis()): TunnelVpnService.Endpoint? {
        val all = byNet(ctx, test)
        val rec = all.optJSONObject(net) ?: return null
        val i = runCatching { list.indexOf(endpoint(rec.getJSONObject("ep"))) }.getOrDefault(-1)
        if (i == 0 || i > 0 && now - rec.optLong("at") < NET_TTL_MS) return list[i]
        all.remove(net)
        save(ctx, all, test)
        return null
    }

    // The same endpoint again keeps its at: the week counts from the first answer, or a
    // carrier used daily would never try the first one again.
    @Synchronized
    fun setForNet(ctx: Context, net: String, ep: TunnelVpnService.Endpoint, test: Boolean = false, now: Long = System.currentTimeMillis()) {
        val all = byNet(ctx, test)
        val old = all.optJSONObject(net)
        if (old != null && runCatching { endpoint(old.getJSONObject("ep")) == ep }.getOrDefault(false)) return
        all.put(net, JSONObject().put("ep", json(ep)).put("at", now))
        while (all.length() > MAX_NETS) all.remove(all.keys().asSequence().minBy { all.optJSONObject(it)?.optLong("at") ?: 0 })
        save(ctx, all, test)
    }

    @Synchronized
    fun clearByNet(ctx: Context, test: Boolean = false) {
        prefs(ctx).edit().remove(key(KEY_BY_NET, test)).remove(key(KEY_WORKING, test)).apply()
    }

    private fun byNet(ctx: Context, test: Boolean) =
        prefs(ctx).getString(key(KEY_BY_NET, test), null)?.let { runCatching { JSONObject(it) }.getOrNull() } ?: JSONObject()

    private fun save(ctx: Context, all: JSONObject, test: Boolean) {
        prefs(ctx).edit().putString(key(KEY_BY_NET, test), all.toString()).remove(key(KEY_WORKING, test)).apply()
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
        return List(a.length()) { i -> endpoint(a.getJSONObject(i)) }
    }

    private fun endpoint(o: JSONObject) =
        o.run { TunnelVpnService.Endpoint(getString("host"), getString("ip"), getInt("port"), getString("path"), optString("id")) }

    private fun json(e: TunnelVpnService.Endpoint) =
        JSONObject().put("host", e.host).put("ip", e.ip).put("port", e.port).put("path", e.path).apply { if (e.id.isNotEmpty()) put("id", e.id) }

    private fun toJson(list: List<TunnelVpnService.Endpoint>) = JSONArray(list.map(::json)).toString()
}
