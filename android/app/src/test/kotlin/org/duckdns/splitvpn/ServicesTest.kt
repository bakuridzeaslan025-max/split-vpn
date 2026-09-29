package org.duckdns.splitvpn

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

class ServicesTest {

    private fun ipToInt(ip: String): Int =
        ip.split(".").map { it.toInt() }.fold(0) { acc, b -> (acc shl 8) or b }

    @Test
    fun idsAreUnique() {
        val ids = Services.ALL.map { it.id }
        assertEquals(ids.size, ids.toSet().size)
    }

    @Test
    fun routesAreValidCidrs() {
        for (svc in Services.ALL) for (r in svc.allRoutes) {
            val parts = r.address.split(".")
            assertEquals("${svc.id}: ${r.address}", 4, parts.size)
            assertTrue(parts.all { it.toIntOrNull() in 0..255 })
            assertTrue("${svc.id}: prefix ${r.prefix}", r.prefix in 1..32)
            val mask = if (r.prefix == 32) -1 else (-1 shl (32 - r.prefix))
            assertEquals("${svc.id}: ${r.address}/${r.prefix} has host bits set",
                ipToInt(r.address), ipToInt(r.address) and mask)
        }
    }

    @Test
    fun noRouteCapturesFakeDnsOrTunAddress() {
        for (svc in Services.ALL) for (r in svc.allRoutes) {
            val mask = if (r.prefix == 32) -1 else (-1 shl (32 - r.prefix))
            val net = ipToInt(r.address) and mask
            for (own in listOf("10.255.0.1", "10.255.0.2")) {
                assertTrue("${svc.id}: ${r.address}/${r.prefix} covers $own",
                    (ipToInt(own) and mask) != net)
            }
        }
    }

    @Test
    fun probeUrlsAreHttps() {
        for (svc in Services.ALL) {
            assertTrue(svc.id, svc.probeUrl.startsWith("https://"))
            assertTrue(svc.id, svc.title.isNotBlank())
        }
    }

    @Test
    fun telegramIsOnByDefaultOnly() {
        assertEquals(listOf("telegram"), Services.ALL.filter { it.defaultEnabled }.map { it.id })
    }

    @Test
    fun everyServiceHasDomains() {
        for (svc in Services.ALL) {
            assertTrue("${svc.id} has no domains", svc.domains.isNotEmpty())
            for (d in svc.domains) assertTrue("bad domain $d", Regex("^[a-z0-9.-]+\\.[a-z]+$").matches(d))
        }
    }
}
