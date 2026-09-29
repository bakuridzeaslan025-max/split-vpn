package org.duckdns.splitvpn

import android.content.Context
import java.io.File

/**
 * Subnets the Go resolver saw the listed sites on, one "site subnet unixSeconds"
 * per line (written by tunnel/routes.go). Read at establish and added to the
 * enabled services' routes, so a site that moved keeps working after a
 * rebuild without a new release.
 */
object RouteCache {
    const val FILE = "routes.txt"
    private const val TTL_S = 30L * 24 * 3600

    fun file(ctx: Context): File = File(ctx.filesDir, FILE)

    /** Cached /24s for sites of [enabled] services, minus what [known] already covers. */
    fun routesFor(ctx: Context, enabled: List<Service>, known: List<Route>, now: Long = System.currentTimeMillis() / 1000): List<Route> {
        val f = file(ctx)
        if (!f.exists()) return emptyList()
        val suffixes = enabled.flatMap { it.domains }
        val have = known.map { it.address to it.prefix }.toSet()
        val out = LinkedHashSet<Route>()
        f.forEachLine { line ->
            val p = line.trim().split(' ')
            if (p.size != 3) return@forEachLine
            val ts = p[2].toLongOrNull() ?: return@forEachLine
            if (ts < now - TTL_S) return@forEachLine
            val site = p[0]
            if (suffixes.none { site == it || site.endsWith(".$it") }) return@forEachLine
            val r = Route(p[1], 24)
            if ((r.address to r.prefix) !in have && !covered(r, known)) out += r
        }
        return out.toList()
    }

    private fun covered(r: Route, known: List<Route>): Boolean {
        val ip = toInt(r.address) ?: return false
        return known.any { k ->
            val kip = toInt(k.address) ?: return@any false
            val mask = if (k.prefix == 0) 0 else (-1 shl (32 - k.prefix))
            k.prefix <= 24 && (ip and mask) == (kip and mask)
        }
    }

    private fun toInt(a: String): Int? {
        val o = a.split('.').map { it.toIntOrNull() ?: return null }
        if (o.size != 4 || o.any { it !in 0..255 }) return null
        return (o[0] shl 24) or (o[1] shl 16) or (o[2] shl 8) or o[3]
    }
}
