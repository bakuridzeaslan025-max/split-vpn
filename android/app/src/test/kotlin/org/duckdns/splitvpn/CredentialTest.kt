package org.duckdns.splitvpn

import android.content.Context
import android.content.Intent
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.Robolectric
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.Shadows.shadowOf
import org.robolectric.android.controller.ServiceController
import org.robolectric.shadows.ShadowLooper
import org.robolectric.shadows.ShadowNetworkCapabilities

/** Credential lifecycle in the service: register, store, renew, fall back. */
@RunWith(RobolectricTestRunner::class)
class CredentialTest {

    private lateinit var controller: ServiceController<TunnelVpnService>
    private val app get() = RuntimeEnvironment.getApplication()
    private val svc get() = controller.get()
    private val be = FakeBackend()
    private val now get() = System.currentTimeMillis() / 1000

    @Before
    fun setUp() {
        TunnelState.set(VpnState.DISCONNECTED)
        ShadowLooper.idleMainLooper()
        app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE).edit().clear().commit()
        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
        svc.backend = be
    }

    @After
    fun tearDown() {
        be.release()
        controller.destroy()
        ShadowLooper.idleMainLooper()
    }

    private fun start(invite: String? = null) {
        val i = Intent(TunnelVpnService.ACTION_START).putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram"))
        if (invite != null) i.putExtra(TunnelVpnService.EXTRA_INVITE, invite)
        svc.onStartCommand(i, 0, 1)
    }

    private fun awaitState(want: VpnState) {
        val deadline = System.currentTimeMillis() + 10_000
        while (TunnelState.state != want && System.currentTimeMillis() < deadline) {
            ShadowLooper.idleMainLooper()
            Thread.sleep(20)
        }
        assertEquals(TunnelState.lastError ?: "", want, TunnelState.state)
    }

    private fun stored() = Credentials.load(app)

    private fun online(yes: Boolean) {
        val cm = app.getSystemService(android.net.ConnectivityManager::class.java)
        val caps = ShadowNetworkCapabilities.newInstance()
        if (yes) {
            shadowOf(caps).addCapability(android.net.NetworkCapabilities.NET_CAPABILITY_INTERNET)
            shadowOf(caps).addCapability(android.net.NetworkCapabilities.NET_CAPABILITY_VALIDATED)
        }
        shadowOf(cm).setNetworkCapabilities(cm.activeNetwork, caps)
    }

    @Test
    fun inviteRegistersStoresAndStarts() {
        val cred = fakeCred(now + 7 * 86400, kind = 2)
        be.registerResult = cred
        start(invite = " abc-123 ")
        awaitState(VpnState.CONNECTED)
        assertEquals(1, be.registered.size)
        assertEquals(tunnel.Tunnel.KindInvite.toInt(), be.registered[0].first)
        assertEquals("abc-123", String(be.registered[0].second))
        assertArrayEquals(cred, be.startedWith)
        assertArrayEquals(cred, stored())
    }

    @Test
    fun relayRejectsStoredCredential_reRegistersAndStarts() {
        val stale = fakeCred(now + 5 * 86400)
        val fresh = fakeCred(now + 7 * 86400)
        Credentials.save(app, stale)
        be.rejectCreds += stale.toList()
        be.token = "tok".toByteArray()
        be.registerResult = fresh
        start()
        awaitState(VpnState.CONNECTED)
        assertEquals(1, be.registered.size)
        assertArrayEquals(fresh, be.startedWith)
        assertArrayEquals(fresh, stored())
    }

    @Test
    fun freshCredentialSkipsRegistration() {
        val cred = fakeCred(now + 5 * 86400)
        Credentials.save(app, cred)
        start()
        awaitState(VpnState.CONNECTED)
        assertTrue(be.registered.isEmpty())
        assertArrayEquals(cred, be.startedWith)
    }

    @Test
    fun expiringCredentialRenewsViaIntegrity() {
        val old = fakeCred(now + 3600)
        val fresh = fakeCred(now + 7 * 86400)
        Credentials.save(app, old)
        be.token = "tok".toByteArray()
        be.registerResult = fresh
        start()
        awaitState(VpnState.CONNECTED)
        assertEquals(tunnel.Tunnel.KindIntegrity.toInt(), be.registered.single().first)
        val proof = be.registered.single().second
        assertEquals(32 + 3, proof.size)
        assertEquals("tok", String(proof.copyOfRange(32, 35)))
        assertArrayEquals(fresh, be.startedWith)
        assertArrayEquals(fresh, stored())
    }

    @Test
    fun renewalFailureKeepsUsableCredential() {
        val old = fakeCred(now + 3600)
        Credentials.save(app, old)
        be.token = "tok".toByteArray()
        be.registerError = Exception("registration rejected")
        start()
        awaitState(VpnState.CONNECTED)
        assertArrayEquals(old, be.startedWith)
    }

    @Test
    fun expiredCredentialWithoutProofNeedsCode() {
        online(true)
        Credentials.save(app, fakeCred(now - 10))
        start()
        awaitState(VpnState.ERROR)
        assertEquals(TunnelVpnService.ERR_NEED_CODE, TunnelState.lastError)
        assertTrue(be.events.isEmpty())
        assertNull("expired credential must be dropped", stored())
    }

    @Test
    fun integrityPreferredOverInviteAndInviteIsFallback() {
        val cred = fakeCred(now + 7 * 86400)
        be.token = "tok".toByteArray()
        be.registerResult = cred
        start(invite = "code")
        awaitState(VpnState.CONNECTED)
        assertEquals(listOf(tunnel.Tunnel.KindIntegrity.toInt()), be.registered.map { it.first })

        controller.destroy(); ShadowLooper.idleMainLooper()
        TunnelState.set(VpnState.DISCONNECTED)
        app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE).edit().clear().commit()
        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
        val be2 = FakeBackend().apply { token = "tok".toByteArray(); registerResult = null }
        svc.backend = be2
        // Integrity rejected → invite used.
        var calls = 0
        val rejecting = object : Backend by be2 {
            override fun register(addr: String, sni: String, path: String, kind: Int, proof: ByteArray): ByteArray {
                calls++
                if (kind == tunnel.Tunnel.KindIntegrity.toInt()) throw Exception("registration rejected")
                return cred
            }
        }
        svc.backend = rejecting
        start(invite = "code")
        awaitState(VpnState.CONNECTED)
        assertEquals(2, calls)
    }

    // Offline even Integrity fails: the fix is the network, not a code.
    @Test
    fun expiredCredentialOfflineSaysNoServer() {
        online(false)
        Credentials.save(app, fakeCred(now - 10))
        start()
        awaitState(VpnState.ERROR)
        assertEquals(TunnelVpnService.ERR_OFFLINE, TunnelState.lastError)
    }

    // ...and the network coming back is the retry, without a tap.
    @Test
    fun expiredCredentialOfflineRenewsWhenOnline() {
        online(false)
        Credentials.save(app, fakeCred(now - 10))
        start()
        awaitState(VpnState.ERROR)
        ShadowLooper.idleMainLooper()
        assertTrue("wanted must survive the wait",
            app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE).getBoolean(TunnelVpnService.KEY_WANTED, false))

        online(true)
        be.token = "tok".toByteArray()
        val cred = fakeCred(now + 7 * 86400)
        be.registerResult = cred
        val cm = app.getSystemService(android.net.ConnectivityManager::class.java)
        shadowOf(cm).networkCallbacks.toList().forEach { it.onAvailable(org.robolectric.shadows.ShadowNetwork.newInstance(7)) }
        awaitState(VpnState.CONNECTED)
        assertArrayEquals(cred, be.startedWith)
        assertEquals("retry callback leaked; only the tunnel's own two stay", 2, shadowOf(cm).networkCallbacks.size)
        val note = shadowOf(app.getSystemService(android.app.NotificationManager::class.java)).getNotification(1)
        assertEquals("VPN включён", note?.extras?.getString(android.app.Notification.EXTRA_TEXT))
    }

    @Test
    fun registrationUnreachableSaysNoServer() {
        online(true)
        be.token = "tok".toByteArray()
        be.registerError = Exception("relay reply: EOF")
        start()
        awaitState(VpnState.ERROR)
        assertEquals(TunnelVpnService.ERR_NO_SERVER, TunnelState.lastError)
    }

    // A 404 is also what a relay that is not an issuer says: ask the next one.
    @Test
    fun registrationRejectedByOneEndpointTriesTheNext() {
        online(true)
        be.token = "tok".toByteArray()
        be.endpoints = listOf(
            TunnelVpnService.Endpoint("a.example.org", "203.0.113.10", 443, "/a"),
            TunnelVpnService.Endpoint("b.example.org", "203.0.113.11", 443, "/b"),
        )
        val cred = fakeCred(now + 7 * 86400)
        val asked = mutableListOf<String>()
        svc.backend = object : Backend by be {
            override fun register(addr: String, sni: String, path: String, kind: Int, proof: ByteArray): ByteArray {
                asked += sni
                if (sni == "a.example.org") throw Exception("registration rejected")
                return cred
            }
        }
        start()
        awaitState(VpnState.CONNECTED)
        assertEquals(listOf("a.example.org", "b.example.org"), asked)
        assertArrayEquals(cred, stored())
    }

    @Test
    fun registrationRejectedStillNeedsCode() {
        online(true)
        be.token = "tok".toByteArray()
        be.registerError = Exception("registration rejected")
        start()
        awaitState(VpnState.ERROR)
        assertEquals(TunnelVpnService.ERR_NEED_CODE, TunnelState.lastError)
    }

    /** Whether a fresh process resumes the tunnel by itself, [failedAgoMs] after the failure. */
    private fun resumes(failedAgoMs: Long): Boolean {
        controller.destroy()
        app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE).edit()
            .putLong(TunnelVpnService.KEY_FAILED_AT, System.currentTimeMillis() - failedAgoMs).commit()
        TunnelState.set(VpnState.DISCONNECTED)
        ShadowLooper.idleMainLooper()
        while (shadowOf(app).nextStartedService != null) Unit
        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
        svc.backend = be
        return shadowOf(app).nextStartedService?.action == TunnelVpnService.ACTION_START
    }

    // A refusal keeps the tunnel wanted (it may be the issuer's outage or a
    // late GMS after boot), but a fresh process retries it hourly, not every minute.
    @Test
    fun registrationRejectedResumesHourlyAndTheCodeStillStarts() {
        online(true)
        be.token = "tok".toByteArray()
        be.registerError = Exception("registration rejected")
        start()
        awaitState(VpnState.ERROR)
        assertEquals(TunnelVpnService.ERR_NEED_CODE, TunnelState.lastError)
        val prefs = app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)
        assertTrue(prefs.getBoolean(TunnelVpnService.KEY_WANTED, false))
        assertFalse(resumes(2 * 60_000L))
        assertTrue(resumes(61 * 60_000L))

        val cred = fakeCred(now + 7 * 86400, kind = 2)
        be.registerError = null
        be.registerResult = cred
        start(invite = "abc-123")
        awaitState(VpnState.CONNECTED)
        assertArrayEquals(cred, stored())
        assertTrue(prefs.getBoolean(TunnelVpnService.KEY_WANTED, false))
        assertFalse(prefs.contains(TunnelVpnService.KEY_NEED_CODE))
    }

    @Test
    fun otherFailureResumesAfterAMinute() {
        online(true)
        be.token = "tok".toByteArray()
        be.registerError = Exception("relay reply: EOF")
        start()
        awaitState(VpnState.ERROR)
        assertEquals(TunnelVpnService.ERR_NO_SERVER, TunnelState.lastError)
        assertFalse(resumes(30_000L))
        assertTrue(resumes(2 * 60_000L))
    }
}
