package org.duckdns.splitvpn

import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner

/** The baked-in endpoint list: what Go hands back, and what a broken build gets. */
@RunWith(RobolectricTestRunner::class)
class EndpointsTest {

    @Test
    fun parsesWhatGoReturns() {
        Endpoints.decode("") { "" } // leaves a failure behind that success must clear
        val json = """[{"host":"cover.example.org","ip":"203.0.113.10","port":443,"path":"/test"},""" +
            """{"host":"backup.example.net","ip":"198.51.100.7","port":8443,"path":"/b"}]"""
        assertEquals(
            listOf(
                TunnelVpnService.Endpoint("cover.example.org", "203.0.113.10", 443, "/test"),
                TunnelVpnService.Endpoint("backup.example.net", "198.51.100.7", 8443, "/b"),
            ),
            Endpoints.decode("blob") { assertEquals("blob", it); json },
        )
        assertEquals(null, Endpoints.failure)
    }

    /** A build without rc.key / vds.endpoints: no list, and Go is not even asked. */
    @Test
    fun emptyDefaultWithoutKey() {
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), Endpoints.decode("") { error("Go called") })
        assertEquals("none baked in at build time", Endpoints.failure)
    }

    /** Go refusing the blob (no key in the AAR, another key) leaves an empty list and a reason. */
    @Test
    fun decryptionFailureIsEmptyWithAReason() {
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), Endpoints.decode("blob") { throw Exception("endpoints: no key in this build") })
        assertEquals("no key in this build", Endpoints.failure)
    }

    /** No Go under unit tests; the same goes for a missing .so on a device. */
    @Test
    fun missingGoLibraryIsNotACrash() {
        assertEquals(emptyList<TunnelVpnService.Endpoint>(), Endpoints.decode("blob") { throw UnsatisfiedLinkError("no gojni") })
    }

    @Test
    fun badJsonDoesNotQuoteTheValue() {
        Endpoints.decode("blob") { """[{"host":"cover.example.org","ip":"203.0.113.10","port":"x","path":"/test"}]""" }
        assertTrue(Endpoints.failure!!.let { "203.0.113" !in it && "cover" !in it })
    }
}
