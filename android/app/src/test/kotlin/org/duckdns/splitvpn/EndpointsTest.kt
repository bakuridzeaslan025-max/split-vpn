package org.duckdns.splitvpn

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment

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

    private val app get() = RuntimeEnvironment.getApplication()
    private val a = TunnelVpnService.Endpoint("a.example.org", "203.0.113.10", 443, "/a")
    private val b = TunnelVpnService.Endpoint("b.example.net", "198.51.100.7", 8443, "/b")
    private val ttl = Endpoints.NET_TTL_MS

    // The carrier may have let the first one through again; the first itself never goes stale.
    @Test
    fun recordPastTheFirstExpiresInAWeek() {
        Endpoints.setForNet(app, "cell:25020", b, now = 0)
        Endpoints.setForNet(app, "cell:25001", a, now = 0)
        assertEquals(b, Endpoints.forNet(app, "cell:25020", listOf(a, b), now = ttl - 1))
        assertNull(Endpoints.forNet(app, "cell:25020", listOf(a, b), now = ttl))
        assertNull("and gone", Endpoints.forNet(app, "cell:25020", listOf(a, b), now = 0))
        assertEquals(a, Endpoints.forNet(app, "cell:25001", listOf(a, b), now = ttl * 100))
    }

    // By content: the list's entry, with its label.
    @Test
    fun recordIsTheListsOwnEntry() {
        Endpoints.setForNet(app, "cell:25020", b)
        val labelled = TunnelVpnService.Endpoint(b.host, b.ip, b.port, b.path, "nl2")
        assertEquals("nl2", Endpoints.forNet(app, "cell:25020", listOf(a, labelled))!!.id)
    }

    @Test
    fun seventeenthNetworkPushesOutTheOldest() {
        for (i in 0 until 17) Endpoints.setForNet(app, "cell:250$i", a, now = 1_000L + if (i == 5) -500 else i)
        assertNull(Endpoints.forNet(app, "cell:2505", listOf(a)))
        for (i in 0 until 17) if (i != 5) assertEquals(a, Endpoints.forNet(app, "cell:250$i", listOf(a)))
    }

    @Test
    fun clearForgetsAllButTheOtherSet() {
        Endpoints.setForNet(app, "cell:25020", a)
        Endpoints.setForNet(app, "cell:25020", b, test = true)
        Endpoints.clearByNet(app, test = true)
        assertNull(Endpoints.forNet(app, "cell:25020", listOf(a, b), test = true))
        assertEquals(a, Endpoints.forNet(app, "cell:25020", listOf(a, b)))
        Endpoints.clearByNet(app)
        assertNull(Endpoints.forNet(app, "cell:25020", listOf(a, b)))
    }

    // Its one endpoint for every network is not read: the first network's answer starts the memory.
    @Test
    fun oldWorkingKeyGoes() {
        val prefs = app.getSharedPreferences(TunnelVpnService.PREFS, android.content.Context.MODE_PRIVATE)
        prefs.edit().putString("endpoint.working", "[]").commit()
        Endpoints.setForNet(app, "cell:25020", a)
        assertNull(prefs.getString("endpoint.working", null))
    }

    // A fixed week: answers every day on the same carrier do not push it on.
    @Test
    fun dailyAnswersDoNotExtendTheWeek() {
        for (day in 0 until 7) Endpoints.setForNet(app, "cell:25020", b, now = day * 86_400_000L)
        assertNull(Endpoints.forNet(app, "cell:25020", listOf(a, b), now = ttl))
        Endpoints.setForNet(app, "cell:25020", b, now = 0)
        Endpoints.setForNet(app, "cell:25020", a, now = ttl)
        Endpoints.setForNet(app, "cell:25020", b, now = ttl * 2)
        assertEquals("another endpoint starts a new week", b, Endpoints.forNet(app, "cell:25020", listOf(a, b), now = ttl * 3 - 1))
    }
}
