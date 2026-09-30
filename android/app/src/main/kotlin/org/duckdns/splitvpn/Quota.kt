package org.duckdns.splitvpn

import android.content.Context

/**
 * Today's traffic through the relay against the daily limit (Remote
 * Config's daily_quota_mb). :vpn's prefs only: Go counts while the tunnel
 * runs, the service stores what it counted on each of Go's steps and on stop.
 * A day is local midnight to midnight.
 */
internal object Quota {
    const val KEY_LIMIT_MB = "rc.daily_quota_mb"
    private const val KEY_DAY = "quota.day"
    private const val KEY_USED = "quota.used"
    const val DEFAULT_MB = 2048L
    private const val MB = 1024L * 1024

    internal var today: () -> Long = { java.time.LocalDate.now().toEpochDay() }

    private fun prefs(ctx: Context) = ctx.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)

    /** Bytes; 0 is no limit. */
    fun limit(ctx: Context) = prefs(ctx).getLong(KEY_LIMIT_MB, DEFAULT_MB).coerceAtLeast(0) * MB

    /** RC's value; null (no key published) keeps the last one or the default. */
    fun setLimitMb(ctx: Context, mb: Long?) {
        if (mb != null) prefs(ctx).edit().putLong(KEY_LIMIT_MB, mb).apply()
    }

    /** The day [used] was stored on; -1 before any. */
    fun day(ctx: Context) = prefs(ctx).getLong(KEY_DAY, -1)

    /** Stored bytes of today; yesterday's count is none. */
    fun used(ctx: Context) = prefs(ctx).takeIf { day(ctx) == today() }?.getLong(KEY_USED, 0) ?: 0

    /** [day]: the one [used] was counted on, which by now may be over. */
    fun save(ctx: Context, used: Long, day: Long) = prefs(ctx).edit().putLong(KEY_DAY, day).putLong(KEY_USED, used).apply()

    fun exceeded(used: Long, limit: Long) = limit > 0 && used >= limit

    /** "350 МБ", "1,2 ГБ", "2 ГБ". */
    fun format(bytes: Long): String {
        if (bytes < 1024 * MB) return "${bytes / MB} МБ"
        val gb = "%.1f".format(java.util.Locale.ROOT, bytes.toDouble() / (1024 * MB)).removeSuffix(".0").replace('.', ',')
        return "$gb ГБ"
    }
}
