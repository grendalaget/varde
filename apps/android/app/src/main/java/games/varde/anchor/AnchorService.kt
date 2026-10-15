package games.varde.anchor

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import android.util.Log
import java.io.File

/**
 * Runs `varde-agent run --anchor` as a child process forever. The agent in
 * turn spawns varde-mesh; both ship inside jniLibs and are exec'd straight
 * out of nativeLibraryDir.
 */
class AnchorService : Service() {

    companion object {
        const val ACTION_STATUS = "games.varde.anchor.STATUS"
        const val EXTRA_STATUS = "status"
        private const val CHANNEL = "anchor"
        private const val NOTIF_ID = 1
        private const val TAG = "AnchorService"

        @Volatile
        var lastStatus: String = "stopped"
            private set

        fun dataDir(ctx: Context): File = File(ctx.filesDir, "varde")
        fun agentBin(ctx: Context): String =
            ctx.applicationInfo.nativeLibraryDir + "/libagent.so"
        fun meshBin(ctx: Context): String =
            ctx.applicationInfo.nativeLibraryDir + "/libmesh.so"
    }

    private var proc: Process? = null
    private var wakeLock: PowerManager.WakeLock? = null
    @Volatile private var stopping = false
    private var watchdog: Thread? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        startForegroundWithNotification("starting")
        if (proc == null) {
            stopping = false
            spawnAgent()
        }
        return START_STICKY
    }

    override fun onDestroy() {
        stopping = true
        killProcessTree()
        wakeLock?.let { if (it.isHeld) it.release() }
        broadcast("stopped")
        super.onDestroy()
    }

    private fun startForegroundWithNotification(status: String) {
        val nm = getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(
            NotificationChannel(CHANNEL, getString(R.string.channel_name), NotificationManager.IMPORTANCE_LOW)
        )
        val open = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE
        )
        val n = Notification.Builder(this, CHANNEL)
            .setContentTitle(getString(R.string.notification_title))
            .setContentText(status)
            .setSmallIcon(android.R.drawable.stat_notify_sync)
            .setContentIntent(open)
            .setOngoing(true)
            .build()
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTIF_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_DATA_SYNC)
        } else {
            startForeground(NOTIF_ID, n)
        }
    }

    private fun spawnAgent() {
        val dir = dataDir(this).apply { mkdirs() }
        val agent = agentBin(this)
        val mesh = meshBin(this)
        if (!File(agent).canExecute() || !File(mesh).canExecute()) {
            broadcast("binaries missing — rebuild the app via `make android`")
            return
        }
        wakeLock = (getSystemService(PowerManager::class.java))
            .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "varde:anchor")
            .apply { acquire() }
        try {
            proc = ProcessBuilder(
                agent, "run",
                "--data-dir", dir.absolutePath,
                "--mesh-bin", mesh,
                "--anchor",
            ).redirectErrorStream(true).start()
        } catch (e: Exception) {
            Log.e(TAG, "spawn failed", e)
            broadcast("spawn failed: ${e.message}")
            return
        }
        broadcast("running")
        Thread(::drainOutput, "agent-out").start()
        watchdog = Thread(::watch, "agent-watch").also { it.start() }
    }

    private fun drainOutput() {
        try {
            proc?.inputStream?.bufferedReader()?.forEachLine { line ->
                Log.i(TAG, line)
                // surface the latest agent line as the notification text
                if (line.isNotBlank()) {
                    broadcast(line.take(80))
                    startForegroundWithNotification(line.take(80))
                }
            }
        } catch (_: Exception) {
        }
    }

    private fun watch() {
        var backoff = 5_000L
        while (!stopping) {
            val p = proc ?: return
            try {
                p.waitFor()
            } catch (_: InterruptedException) {
                return
            }
            if (stopping) return
            broadcast("agent exited — restarting")
            Thread.sleep(backoff)
            if (stopping) return
            try {
                proc = ProcessBuilder(
                    agentBin(this), "run",
                    "--data-dir", dataDir(this).absolutePath,
                    "--mesh-bin", meshBin(this),
                    "--anchor",
                ).redirectErrorStream(true).start()
                broadcast("running")
                Thread(::drainOutput, "agent-out").start()
                backoff = 5_000L
            } catch (e: Exception) {
                Log.e(TAG, "respawn failed", e)
                backoff = (backoff * 2).coerceAtMost(60_000L)
            }
        }
    }

    private fun killProcessTree() {
        try {
            proc?.destroyForcibly()
        } catch (e: Exception) {
            Log.w(TAG, "kill failed", e)
        }
        proc = null
        // Android has no ProcessHandle/descendants — sweep /proc for
        // grandchildren (e.g. the mesh child) by argv[0] instead.
        killByArg0(agentBin(this))
        killByArg0(meshBin(this))
    }

    private fun killByArg0(path: String) {
        File("/proc").listFiles { f -> f.name.all(Char::isDigit) }?.forEach { dir ->
            try {
                val bytes = File(dir, "cmdline").readBytes()
                val arg0 = if (bytes.isEmpty()) null else String(bytes).split('\u0000').firstOrNull()
                if (arg0 == path) {
                    android.os.Process.killProcess(dir.name.toInt())
                }
            } catch (_: Exception) {
            }
        }
    }

    private fun broadcast(status: String) {
        lastStatus = status
        sendBroadcast(
            Intent(ACTION_STATUS)
                .setPackage(packageName)
                .putExtra(EXTRA_STATUS, status)
        )
    }
}
