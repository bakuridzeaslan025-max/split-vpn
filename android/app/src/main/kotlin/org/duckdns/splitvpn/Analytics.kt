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

    fun setCollection(ctx: Context, on: Boolean) = analytics(ctx) {
        setAnalyticsCollectionEnabled(on)
        // Crash.init set it while collection was off, and the SDK dropped it.
        if (on) Crash.userId(ctx)?.let(::setUserId)
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
