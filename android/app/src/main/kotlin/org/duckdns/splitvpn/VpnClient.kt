package org.duckdns.splitvpn

import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.ServiceConnection
import android.os.Bundle
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.os.Message
import android.os.Messenger
import android.os.RemoteException

/**
 * UI-side handle to TunnelVpnService, which lives in the `:vpn` process.
 * Binds with a Messenger; the service pushes a full snapshot (state, error,
 * log, Remote Config versions, today's traffic) on register and on every change. The UI never
 * reads :vpn's prefs itself: their cache goes stale across processes.
 */
/** Relay traffic of [day] (epoch day) and the daily limit, bytes; limit 0 is none. */
data class Usage(val used: Long, val limit: Long, val day: Long) {
    val exceeded get() = Quota.exceeded(used, limit)
}

/** The last arg of a snapshot: direct YouTube on the current network, Go's verdict ("unknown", "testing", "works", "fails"). */
class VpnClient(private val ctx: Context, private val onSnapshot: (VpnState, String?, List<String>, Long, Waiting?, Versions?, Usage?, String?) -> Unit) {

    companion object {
        const val MSG_REGISTER = 1
        const val MSG_UNREGISTER = 2
        const val MSG_SNAPSHOT = 3
        const val KEY_STATE = "state"
        const val KEY_ERROR = "error"
        const val KEY_LOG = "log"
        const val KEY_SINCE = "since"
        const val KEY_WAITING = "waiting"
        const val KEY_MIN_VERSION = "min_version"
        const val KEY_LATEST_VERSION = "latest_version"
        const val KEY_UPDATE_URL = "update_url"
        const val KEY_USED = "used"
        const val KEY_LIMIT = "limit"
        const val KEY_DAY = "day"
        const val KEY_DIRECT = "direct"

        fun snapshot(state: VpnState, error: String?, log: List<String>, connectedAt: Long, waiting: Waiting?, versions: Versions, usage: Usage, direct: String): Message =
            Message.obtain(null, MSG_SNAPSHOT).apply {
                data = Bundle().apply {
                    putInt(KEY_STATE, state.ordinal)
                    putString(KEY_ERROR, error)
                    putStringArrayList(KEY_LOG, ArrayList(log))
                    putLong(KEY_SINCE, connectedAt)
                    putInt(KEY_WAITING, waiting?.ordinal ?: -1)
                    putLong(KEY_MIN_VERSION, versions.min)
                    putLong(KEY_LATEST_VERSION, versions.latest)
                    putString(KEY_UPDATE_URL, versions.url)
                    putLong(KEY_USED, usage.used)
                    putLong(KEY_LIMIT, usage.limit)
                    putLong(KEY_DAY, usage.day)
                    putString(KEY_DIRECT, direct)
                }
            }
    }

    private var service: Messenger? = null
    private val incoming = Messenger(Handler(Looper.getMainLooper()) { msg ->
        if (msg.what == MSG_SNAPSHOT) {
            onSnapshot(
                VpnState.values()[msg.data.getInt(KEY_STATE)],
                msg.data.getString(KEY_ERROR),
                msg.data.getStringArrayList(KEY_LOG) ?: emptyList(),
                msg.data.getLong(KEY_SINCE),
                Waiting.values().getOrNull(msg.data.getInt(KEY_WAITING, -1)),
                Versions(msg.data.getLong(KEY_MIN_VERSION), msg.data.getLong(KEY_LATEST_VERSION), msg.data.getString(KEY_UPDATE_URL).orEmpty()),
                Usage(msg.data.getLong(KEY_USED), msg.data.getLong(KEY_LIMIT), msg.data.getLong(KEY_DAY)),
                msg.data.getString(KEY_DIRECT),
            )
        }
        true
    })

    private val conn = object : ServiceConnection {
        override fun onServiceConnected(name: ComponentName, binder: IBinder) {
            service = Messenger(binder).also { send(it, MSG_REGISTER) }
        }

        override fun onServiceDisconnected(name: ComponentName) {
            // :vpn process died. Until it comes back, the tunnel is down;
            // the versions and traffic it last told stay true.
            service = null
            onSnapshot(VpnState.DISCONNECTED, null, emptyList(), 0, null, null, null, null)
        }
    }

    fun bind() {
        val intent = Intent(ctx, TunnelVpnService::class.java).setAction(TunnelVpnService.ACTION_BIND)
        ctx.bindService(intent, conn, Context.BIND_AUTO_CREATE)
    }

    fun unbind() {
        service?.let { send(it, MSG_UNREGISTER) }
        service = null
        runCatching { ctx.unbindService(conn) }
    }

    private fun send(to: Messenger, what: Int) {
        try {
            to.send(Message.obtain(null, what).apply { replyTo = incoming })
        } catch (_: RemoteException) {
        }
    }
}
