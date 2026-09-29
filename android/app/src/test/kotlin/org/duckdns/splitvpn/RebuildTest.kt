package org.duckdns.splitvpn

import android.content.Context
import android.content.Intent
import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.android.controller.ServiceController
import org.robolectric.shadows.ShadowLooper

/** T2 step 5: a stale-routes signal rebuilds the TUN with cached subnets, rate-limited. */
@RunWith(RobolectricTestRunner::class)
class RebuildTest {
    private lateinit var controller: ServiceController<TunnelVpnService>
    private val app get() = RuntimeEnvironment.getApplication()
    private val svc get() = controller.get()
    private val be = FakeBackend()
    private val now = System.currentTimeMillis() / 1000

    @Before
    fun setUp() {
        TunnelState.set(VpnState.DISCONNECTED)
        ShadowLooper.idleMainLooper()
        app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE).edit().clear().commit()
        RouteCache.file(app).delete()
        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
        svc.backend = be
        Credentials.save(app, fakeCred(now + 5 * 86400))
    }

    @After
    fun tearDown() {
        be.release()
        controller.destroy()
        ShadowLooper.idleMainLooper()
        RouteCache.file(app).delete()
    }

    private fun start() {
        svc.onStartCommand(Intent(TunnelVpnService.ACTION_START).putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("twitter")), 0, 1)
        awaitState(VpnState.CONNECTED)
    }

    private fun awaitState(want: VpnState) {
        val deadline = System.currentTimeMillis() + 10_000
        while (TunnelState.state != want && System.currentTimeMillis() < deadline) Thread.sleep(20)
        assertEquals(want, TunnelState.state)
    }

    private fun awaitEvents(n: Int) {
        val deadline = System.currentTimeMillis() + 10_000
        while (be.events.size < n && System.currentTimeMillis() < deadline) { ShadowLooper.idleMainLooper(); Thread.sleep(20) }
    }

    @Test
    fun cachedSubnetsGoIntoTunAndStaleTriggersOneRebuild() {
        RouteCache.file(app).writeText("x.com 203.0.113.0 $now\nchatgpt.com 198.51.100.0 $now\nx.com 172.66.0.0 $now\n")
        start()
        // Only x.com's subnet that the routes miss is added; 172.66.0.0 is already covered.
        assertEquals(listOf(Route("203.0.113.0", 24)), be.establishedExtra[0])
        assertTrue(be.startedRoutes!!.contains("203.0.113.0/24"))

        // Go learns another subnet and flags the routes stale.
        RouteCache.file(app).appendText("x.com 192.0.2.0 $now\n")
        be.startedHost!!.routesStale()
        awaitEvents(3)
        assertEquals(listOf("start", "stop", "start"), be.events.toList())
        // The new TUN was established before the old stack was stopped.
        assertEquals(2, be.establishedExtra.size)
        assertEquals(VpnState.CONNECTED, TunnelState.state)
        assertEquals(setOf(Route("203.0.113.0", 24), Route("192.0.2.0", 24)), be.establishedExtra[1].toSet())

        // Another signal right away: rate-limited, nothing happens.
        RouteCache.file(app).appendText("x.com 192.0.3.0 $now\n")
        be.startedHost!!.routesStale()
        ShadowLooper.idleMainLooper()
        Thread.sleep(200)
        assertEquals(3, be.events.size)
    }

    @Test
    fun staleWithNothingNewInCacheDoesNotRebuild() {
        start()
        be.startedHost!!.routesStale()
        ShadowLooper.idleMainLooper()
        Thread.sleep(200)
        assertEquals(listOf("start"), be.events.toList())
    }
}
