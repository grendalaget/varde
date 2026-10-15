package games.varde.anchor

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

/** Restarts the anchor after reboot when it was left enabled. */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED) return
        val prefs = ctx.getSharedPreferences("varde", Context.MODE_PRIVATE)
        if (prefs.getBoolean("enrolled", false) && prefs.getBoolean("enabled", false)) {
            ctx.startForegroundService(Intent(ctx, AnchorService::class.java))
        }
    }
}
