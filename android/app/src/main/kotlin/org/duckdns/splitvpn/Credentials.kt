package org.duckdns.splitvpn

import android.content.Context
import android.util.Base64

/**
 * The device's relay credential (77 bytes from the issuer). Lives in the
 * :vpn process prefs. App-private storage is the protection here: it is a
 * per-device token with a 7-day life, revocable server-side, and a rooted
 * phone can hook the process anyway.
 */
object Credentials {
    private const val KEY = "cred"

    private fun prefs(ctx: Context) = ctx.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)

    // A test relay issues its own credentials: kept apart from the real one.
    private fun key(test: Boolean) = if (test) "$KEY.test" else KEY

    fun load(ctx: Context, test: Boolean = false): ByteArray? =
        prefs(ctx).getString(key(test), null)?.let { runCatching { Base64.decode(it, Base64.NO_WRAP) }.getOrNull() }

    fun save(ctx: Context, cred: ByteArray, test: Boolean = false) {
        prefs(ctx).edit().putString(key(test), Base64.encodeToString(cred, Base64.NO_WRAP)).apply()
    }

    fun clear(ctx: Context, test: Boolean = false) {
        prefs(ctx).edit().remove(key(test)).apply()
    }
}
