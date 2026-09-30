package org.duckdns.splitvpn

import android.content.Context

/** Remote Config keys as fetched. */
/** [dailyQuotaMb]: null when RC has no such key. */
internal data class RcValues(val endpoints: String, val minVersion: Long, val latestVersion: Long, val updateUrl: String, val dailyQuotaMb: Long? = null)

/** The versions part of Remote Config; the UI gets it in VpnClient's snapshot. */
data class Versions(val min: Long = 0, val latest: Long = 0, val url: String = "") {
    val required get() = BuildConfig.VERSION_CODE < min
    // Not for debug: release would go on top of it.
    val available get() = !debug && latest > BuildConfig.VERSION_CODE

    /** The download page: Remote Config's, so it can move if github.io gets blocked. */
    val page get() = url.takeIf { it.startsWith("https://") } ?: DOWNLOAD_PAGE

    companion object {
        const val DOWNLOAD_PAGE = "https://bakuridzeaslan025-max.github.io/split-vpn/"
        internal var debug = BuildConfig.DEBUG
    }
}

/**
 * What the :vpn process keeps from Remote Config. The endpoint list is our
 * own cache, not RC's active value: a broken blob is active in RC the
 * moment it is fetched, and the cache must outlive it.
 */
internal object RemoteConfig {
    const val KEY_MIN_VERSION = "rc.min_version"
    const val KEY_LATEST_VERSION = "rc.latest_version"
    const val KEY_UPDATE_URL = "rc.update_url"
    private const val KEY_DROPPED = "rc.dropped_endpoints"

    fun versions(ctx: Context) = ctx.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE).let {
        Versions(it.getLong(KEY_MIN_VERSION, 0), it.getLong(KEY_LATEST_VERSION, 0), it.getString(KEY_UPDATE_URL, "").orEmpty())
    }

    /**
     * RC serves a broken blob on every fetch until someone publishes a fixed
     * one: a report for the first time this blob is seen, a breadcrumb after.
     */
    internal fun firstDrop(ctx: Context, blob: String): Boolean {
        val prefs = ctx.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)
        val hash = java.security.MessageDigest.getInstance("SHA-256").digest(blob.toByteArray()).joinToString("") { "%02x".format(it) }
        if (prefs.getString(KEY_DROPPED, null) == hash) return false
        prefs.edit().putString(KEY_DROPPED, hash).apply()
        return true
    }

    /** [test]: a list for the instrumented tests' stand, see [Endpoints.cached]; versions stay as they are. */
    fun apply(ctx: Context, v: RcValues, decrypt: (String) -> String, test: Boolean = false) {
        if (!test) {
            ctx.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE).edit()
                .putLong(KEY_MIN_VERSION, v.minVersion)
                .putLong(KEY_LATEST_VERSION, v.latestVersion)
                .putString(KEY_UPDATE_URL, v.updateUrl)
                .apply()
            Quota.setLimitMb(ctx, v.dailyQuotaMb)
        }
        // Not published for this build's condition: nothing to replace the cache with.
        if (v.endpoints.isEmpty()) return
        val list = Endpoints.open(v.endpoints, decrypt).getOrElse {
            AppLog.e("remote config: endpoints dropped, keeping the cache: ${it.message}", expected = !firstDrop(ctx, v.endpoints))
            return
        }
        if (list.isEmpty()) {
            AppLog.e("remote config: endpoints dropped, keeping the cache: empty list", expected = !firstDrop(ctx, v.endpoints))
            return
        }
        // The stand's addresses are no secret, and the known file only grows.
        if (!test) Endpoints.remember(list)
        Endpoints.cache(ctx, list, test)
        AppLog.i("remote config: ${list.size} endpoints")
    }
}
