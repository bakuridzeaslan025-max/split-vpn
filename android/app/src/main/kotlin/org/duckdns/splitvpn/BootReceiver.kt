package org.duckdns.splitvpn

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/**
 * Runs in :vpn. Resumes the tunnel after reboot, and after a self-update
 * (which kills the process), if the user left it on.
 */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED && intent.action != Intent.ACTION_MY_PACKAGE_REPLACED) return
        val wanted = ctx.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)
            .getBoolean(TunnelVpnService.KEY_WANTED, false)
        if (!wanted) return
        ctx.startForegroundService(Intent(ctx, TunnelVpnService::class.java).setAction(TunnelVpnService.ACTION_START))
    }
}
