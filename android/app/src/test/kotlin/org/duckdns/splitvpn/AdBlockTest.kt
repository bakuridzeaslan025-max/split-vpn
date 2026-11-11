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
import java.util.concurrent.TimeUnit

/** The ad blocking checkbox reaches Go before the tunnel starts, and survives restarts. */
@RunWith(RobolectricTestRunner::class)
class AdBlockTest {

    private lateinit var controller: ServiceController<TunnelVpnService>
    private val app get() = RuntimeEnvironment.getApplication()
    private val svc get() = controller.get()
    private val be = FakeBackend()
    private val vpnPrefs get() = app.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)

    @Before
    fun setUp() {
        TunnelState.set(VpnState.DISCONNECTED)
        TunnelState.clearLog()
        ShadowLooper.idleMainLooper()
        controller = Robolectric.buildService(TunnelVpnService::class.java).create()
        svc.backend = be
        Credentials.save(app, fakeCred(System.currentTimeMillis() / 1000 + 5 * 86400))
        ShadowLooper.idleMainLooper()
    }

    @After
    fun tearDown() {
        be.release()
        awaitServiceThreads()
        controller.destroy()
        ShadowLooper.idleMainLooper()
    }

    private fun start(intent: Intent?) {
        svc.onStartCommand(intent, 0, 1)
        val deadline = System.currentTimeMillis() + 10_000
        while (TunnelState.state != VpnState.CONNECTED && System.currentTimeMillis() < deadline) {
            Thread.sleep(20)
            ShadowLooper.idleMainLooper(100, TimeUnit.MILLISECONDS)
        }
        assertEquals(TunnelState.lastError, VpnState.CONNECTED, TunnelState.state)
    }

    private fun startIntent() = Intent(TunnelVpnService.ACTION_START)
        .putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, arrayListOf("telegram"))

    @Test
    fun startWithAdBlockSetsGoBeforeStart() {
        start(startIntent().putExtra(TunnelVpnService.EXTRA_ADBLOCK, true))
        assertEquals(true, be.startedAdBlock)
        assertTrue(vpnPrefs.getBoolean(TunnelVpnService.KEY_ADBLOCK, false))
    }

    @Test
    fun startWithoutTheExtraIsOff() {
        start(startIntent())
        assertEquals(false, be.startedAdBlock)
    }

    private fun switchFromUi(on: Boolean) {
        val msg = android.os.Message.obtain(null, VpnClient.MSG_ADBLOCK).apply {
            data = android.os.Bundle().apply { putBoolean(VpnClient.KEY_ADBLOCK, on) }
        }
        android.os.Messenger(svc.onBind(Intent(TunnelVpnService.ACTION_BIND))!!).send(msg)
        ShadowLooper.idleMainLooper()
    }

    @Test
    fun switchWhileConnectedReachesGoWithoutARestart() {
        start(startIntent())
        switchFromUi(true)
        assertEquals(true, be.adBlock)
        assertTrue(vpnPrefs.getBoolean(TunnelVpnService.KEY_ADBLOCK, false))
        assertEquals("on", be.userProperties["adblock"])
        assertEquals(VpnState.CONNECTED, TunnelState.state)
        assertEquals(listOf("start"), be.events.filter { it == "start" || it == "stop" })
    }

    // The START carried the old value; the switch landed while it ran.
    @Test
    fun switchDuringTheStartIsNotUndone() {
        val inStart = java.util.concurrent.CountDownLatch(1)
        val go = java.util.concurrent.CountDownLatch(1)
        be.establishHook = { inStart.countDown(); go.await(5, TimeUnit.SECONDS) }
        svc.onStartCommand(startIntent().putExtra(TunnelVpnService.EXTRA_ADBLOCK, false), 0, 1)
        assertTrue(inStart.await(5, TimeUnit.SECONDS))
        switchFromUi(true)
        go.countDown()
        start(null)
        assertEquals(true, be.adBlock)
        assertTrue(vpnPrefs.getBoolean(TunnelVpnService.KEY_ADBLOCK, false))
    }

    // Sticky restart, boot, always-on: no extras, what ran last.
    @Test
    fun nullIntentRestartTakesTheSavedOne() {
        vpnPrefs.edit().putStringSet(TunnelVpnService.KEY_SERVICES, setOf("telegram"))
            .putBoolean(TunnelVpnService.KEY_ADBLOCK, true).commit()
        start(null)
        assertEquals(true, be.startedAdBlock)
    }
}
