package org.duckdns.splitvpn

import org.junit.After
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.shadows.ShadowLooper

@RunWith(RobolectricTestRunner::class)
class TunnelStateTest {

    private val stateListeners = mutableListOf<(VpnState, String?) -> Unit>()
    private val logListeners = mutableListOf<(List<String>) -> Unit>()

    private fun onState(l: (VpnState, String?) -> Unit) = l.also { stateListeners += it; TunnelState.subscribe(it) }
    private fun onLog(l: (List<String>) -> Unit) = l.also { logListeners += it; TunnelState.subscribeLog(it) }

    @Before
    fun reset() {
        TunnelState.set(VpnState.DISCONNECTED)
        TunnelState.clearLog()
        ShadowLooper.idleMainLooper()
    }

    @After
    fun unsubscribe() {
        stateListeners.forEach(TunnelState::unsubscribe)
        logListeners.forEach(TunnelState::unsubscribeLog)
    }

    @Test
    fun setUpdatesStateSynchronouslyAndNotifiesOnMainLooper() {
        val got = mutableListOf<Pair<VpnState, String?>>()
        onState { s, e -> got += s to e }
        ShadowLooper.idleMainLooper()
        got.clear()

        TunnelState.set(VpnState.CONNECTING)
        assertEquals(VpnState.CONNECTING, TunnelState.state)
        assertTrue("delivered before looper ran", got.isEmpty())
        ShadowLooper.idleMainLooper()
        assertEquals(listOf(VpnState.CONNECTING to null), got)
    }

    @Test
    fun subscribeDeliversSnapshot() {
        TunnelState.set(VpnState.ERROR, "boom")
        val got = mutableListOf<Pair<VpnState, String?>>()
        onState { s, e -> got += s to e }
        ShadowLooper.idleMainLooper()
        assertEquals(listOf(VpnState.ERROR to "boom"), got)
    }

    @Test
    fun errorKeptUntilDisconnected() {
        TunnelState.set(VpnState.ERROR, "boom")
        TunnelState.set(VpnState.CONNECTING)
        assertEquals("boom", TunnelState.lastError)
        TunnelState.set(VpnState.DISCONNECTED)
        assertNull(TunnelState.lastError)
    }

    @Test
    fun unsubscribedListenerGetsNothing() {
        var calls = 0
        val l = onState { _, _ -> calls++ }
        ShadowLooper.idleMainLooper()
        TunnelState.unsubscribe(l)
        TunnelState.set(VpnState.CONNECTED)
        ShadowLooper.idleMainLooper()
        assertEquals(1, calls)
    }

    @Test
    fun logKeepsLast20Lines() {
        for (i in 1..25) TunnelState.log("line $i")
        val lines = TunnelState.logLines()
        assertEquals(20, lines.size)
        assertTrue(lines.first(), lines.first().endsWith("  line 6"))
        assertTrue(lines.last(), lines.last().endsWith("  line 25"))
    }

    @Test
    fun logListenerGetsSnapshotThenUpdatesInOrder() {
        TunnelState.log("a")
        val got = mutableListOf<List<String>>()
        onLog { got += it }
        TunnelState.log("b")
        TunnelState.clearLog()
        ShadowLooper.idleMainLooper()
        assertEquals(listOf(listOf("a"), listOf("a", "b"), emptyList()), got.map { ls -> ls.map { it.substringAfter("  ") } })
        assertTrue(TunnelState.logLines().isEmpty())
    }

    @Test
    fun unsubscribedLogListenerGetsNothing() {
        val got = mutableListOf<List<String>>()
        val l = onLog { got += it }
        ShadowLooper.idleMainLooper()
        TunnelState.unsubscribeLog(l)
        TunnelState.log("x")
        ShadowLooper.idleMainLooper()
        assertEquals(1, got.size)
    }
}
