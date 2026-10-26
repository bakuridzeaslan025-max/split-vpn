package org.duckdns.splitvpn

import android.content.Context
import android.os.Bundle
import com.google.firebase.analytics.FirebaseAnalytics

/**
 * Firebase Analytics for both processes. Names and values in latin, never
 * a site, an address or an error's text.
 */
internal object Analytics {
    // Swapped in unit tests of the UI process; :vpn's go through Backend.
    internal var event: (Context, String, Map<String, Any>) -> Unit = { ctx, name, params ->
        analytics(ctx) { logEvent(name, bundle(params)) }
    }

    fun userProperty(ctx: Context, name: String, value: String) = analytics(ctx) { setUserProperty(name, value) }

    // Swapped in unit tests: the real SDK there reports to the production property.
    internal var setCollection: (Context, Boolean) -> Unit = { ctx, on ->
        analytics(ctx) {
            setAnalyticsCollectionEnabled(on)
            // Crash.init set it while collection was off, and the SDK dropped it.
            if (on) Crash.userId(ctx)?.let(::setUserId)
        }
    }

    /**
     * Each process's start, from [Crash.init]. Release turns collection on
     * every time: a debug build, signed with the same key, installs over it
     * with the data and leaves the SDK's flag off, and that flag outranks
     * the manifest's. It did, for weeks, on a device that had run debug.
     */
    fun start(ctx: Context, process: String, debug: Boolean = BuildConfig.DEBUG) {
        if (!debug) return setCollection(ctx, true)
        analytics(ctx) { Crash.userId(ctx)?.let(::setUserId) }
        // The SDK's flag outlives the process and goes off seconds late, so a
        // test run's last START may leave it on for the next launch. Only
        // :vpn turns it on, by EXTRA_ANALYTICS.
        if (process == "ui") setCollection(ctx, false)
    }

    /** Numbers go as longs: GA sums them as custom metrics. */
    internal fun bundle(params: Map<String, Any>) = Bundle().apply {
        params.forEach { (k, v) ->
            when (v) {
                is Number -> putLong(k, v.toLong())
                is String -> putString(k, v)
                else -> AppLog.e("analytics: $k is a ${v.javaClass.simpleName}, dropped")
            }
        }
    }

    // Analytics must never be the thing that takes the tunnel down.
    private inline fun analytics(ctx: Context, body: FirebaseAnalytics.() -> Unit) {
        try {
            FirebaseAnalytics.getInstance(ctx).body()
        } catch (e: Exception) {
            AppLog.e("analytics", e)
        }
    }
}
