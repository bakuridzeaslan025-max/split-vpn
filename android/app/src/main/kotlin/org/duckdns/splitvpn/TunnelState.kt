package org.duckdns.splitvpn

import android.os.Handler
import android.os.Looper

enum class VpnState { DISCONNECTED, CONNECTING, CONNECTED, DISCONNECTING, ERROR }

/** CONNECTED, but the relay does not answer; the tunnel reconnects by itself. */
enum class Waiting { NO_NETWORK, NO_SERVER }

object TunnelState {
    @Volatile
    var state: VpnState = VpnState.DISCONNECTED
        private set

    @Volatile
    var lastError: String? = null
        private set

    /** SystemClock.elapsedRealtime() of the last switch to CONNECTED; the clock is shared across processes. */
    @Volatile
    var connectedAt: Long = 0
        private set

    @Volatile
    var waiting: Waiting? = null
        private set

    private val listeners = mutableSetOf<(VpnState, String?) -> Unit>()
    private val logListeners = mutableSetOf<(List<String>) -> Unit>()
    private val ui = Handler(Looper.getMainLooper())

    private val logLines = ArrayDeque<String>()
    private val LOG_TIME = java.time.format.DateTimeFormatter.ofPattern("HH:mm:ss")

    @Synchronized
    fun logLines(): List<String> = logLines.toList()

    @Synchronized
    fun log(line: String) {
        logLines.addLast("${java.time.LocalTime.now().format(LOG_TIME)}  $line")
        while (logLines.size > 20) logLines.removeFirst()
        val snapshot = logLines.toList()
        val ls = logListeners.toList()
        ui.post { ls.forEach { it(snapshot) } }
    }

    @Synchronized
    fun clearLog() {
        logLines.clear()
        val ls = logListeners.toList()
        ui.post { ls.forEach { it(emptyList()) } }
    }

    @Synchronized
    fun subscribeLog(listener: (List<String>) -> Unit) {
        logListeners += listener
        val snapshot = logLines.toList()
        ui.post { listener(snapshot) }
    }

    @Synchronized
    fun unsubscribeLog(listener: (List<String>) -> Unit) {
        logListeners -= listener
    }

    @Synchronized
    fun set(next: VpnState, error: String? = null) {
        if (next == VpnState.CONNECTED && state != VpnState.CONNECTED) connectedAt = android.os.SystemClock.elapsedRealtime()
        state = next
        if (next != VpnState.CONNECTED) waiting = null
        Crash.state(next)
        if (next == VpnState.ERROR) lastError = error
        else if (next == VpnState.DISCONNECTED) lastError = null
        val snapshot = listeners.toList()
        val err = lastError
        ui.post { snapshot.forEach { it(next, err) } }
    }

    /** False when nothing changed, or the tunnel is no longer CONNECTED. */
    @Synchronized
    fun setWaiting(w: Waiting?): Boolean {
        if (w == waiting || state != VpnState.CONNECTED) return false
        waiting = w
        val snapshot = listeners.toList()
        val cur = state
        val err = lastError
        ui.post { snapshot.forEach { it(cur, err) } }
        return true
    }

    @Synchronized
    fun subscribe(listener: (VpnState, String?) -> Unit) {
        listeners += listener
        val cur = state
        val err = lastError
        ui.post { listener(cur, err) }
    }

    @Synchronized
    fun unsubscribe(listener: (VpnState, String?) -> Unit) {
        listeners -= listener
    }
}
