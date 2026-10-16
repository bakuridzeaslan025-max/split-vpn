package org.duckdns.splitvpn

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/**
 * Runs in :vpn. Resumes the tunnel after reboot, and after an update
 * (which kills the process), if the user left it on — or had it on until
 * min_version turned this version away.
 */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        val replaced = intent.action == Intent.ACTION_MY_PACKAGE_REPLACED
        if (intent.action != Intent.ACTION_BOOT_COMPLETED && !replaced) return
        val prefs = ctx.getSharedPreferences(TunnelVpnService.PREFS, Context.MODE_PRIVATE)
        val resume = replaced && prefs.getBoolean(TunnelVpnService.KEY_RESUME_AFTER_UPDATE, false)
        // One try: if this version is turned away too, startTunnel sets it again.
        if (resume) prefs.edit().putBoolean(TunnelVpnService.KEY_RESUME_AFTER_UPDATE, false).apply()
        if (!resume && !prefs.getBoolean(TunnelVpnService.KEY_WANTED, false)) return
        ctx.startForegroundService(Intent(ctx, TunnelVpnService::class.java).setAction(TunnelVpnService.ACTION_START))
    }
}
