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
        private val MESSAGE_RE =
            Regex("\"message\"\\s*:\\s*\"((?:[^\"\\\\]|\\\\.){0,80})")

        @Volatile
        var lastStatus: String = "stopped"
            private set

        fun dataDir(ctx: Context): File = File(ctx.filesDir, "varde")
        fun agentBin(ctx: Context): String =
            ctx.applicationInfo.nativeLibraryDir + "/libagent.so"
        fun meshBin(ctx: Context): String =
            ctx.applicationInfo.nativeLibraryDir + "/libmesh.so"

        // App-spawned processes inherit no HOME/TMPDIR and cwd=/ — the
        // agent writes temp files and resolves paths through them, and
        // every write to / fails EROFS on Android.
        fun procBuilder(ctx: Context, vararg args: String): ProcessBuilder {
            val pb = ProcessBuilder(*args)
            pb.directory(dataDir(ctx).apply { mkdirs() })
            val env = pb.environment()
            env["HOME"] = ctx.filesDir.absolutePath
            env["TMPDIR"] = ctx.cacheDir.absolutePath
            return pb
        }
    }

    private var proc: Process? = null
    private var wakeLock: PowerManager.WakeLock? = null
    @Volatile private var stopping = false
    private var watchdog: Thread? = null
    private val procLock = Any()

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
            .setSmallIcon(R.drawable.ic_stat_anchor)
            .setContentIntent(open)
            .setOngoing(true)
            .build()
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTIF_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        } else {
            startForeground(NOTIF_ID, n)
        }
    }

    private fun spawnAgent() {
        val dir = dataDir(this).apply { mkdirs() }
        val agent = agentBin(this)
        val mesh = meshBin(this)
        if (!File(agent).canExecute() || !File(mesh).canExecute()) {
            // unrecoverable until the APK is rebuilt — stop rather than sit
            // enabled doing nothing
            broadcast("binaries missing — rebuild the app via `make android`")
            stopSelf()
            return
        }
        val p = try {
            procBuilder(
                this, agent, "run",
                "--data-dir", dir.absolutePath,
                "--mesh-bin", mesh,
                "--anchor",
            ).redirectErrorStream(true).start()
        } catch (e: Exception) {
            Log.e(TAG, "spawn failed", e)
            broadcast("spawn failed: ${e.message}")
            stopSelf()
            return
        }
        synchronized(procLock) {
            if (stopping) {
                p.destroyForcibly()
                return
            }
            proc = p
        }
        // only hold the wake lock once a child actually exists — an empty
        // foreground service holding it would just drain the battery
        wakeLock = (getSystemService(PowerManager::class.java))
            .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "varde:anchor")
            .apply { acquire() }
        broadcast("running")
        Thread(::drainOutput, "agent-out").start()
        watchdog = Thread(::watch, "agent-watch").also { it.start() }
    }

    private fun drainOutput() {
        try {
            proc?.inputStream?.bufferedReader()?.forEachLine { line ->
                Log.i(TAG, line)
                // agent logs JSON lines; surface the message field, not raw JSON
                val msg = MESSAGE_RE.find(line)?.groupValues?.get(1)
                    ?.replace("\\\"", "\"")
                    ?.replace("\\n", " ")
                    ?.replace("\\t", " ")
                    ?.replace("\\\\", "\\")
                    ?: line.take(80)
                if (msg.isNotBlank()) {
                    broadcast(msg.take(80))
                    startForegroundWithNotification(msg.take(80))
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
            val np = try {
                procBuilder(
                    this, agentBin(this), "run",
                    "--data-dir", dataDir(this).absolutePath,
                    "--mesh-bin", meshBin(this),
                    "--anchor",
                ).redirectErrorStream(true).start()
            } catch (e: Exception) {
                Log.e(TAG, "respawn failed", e)
                backoff = (backoff * 2).coerceAtMost(60_000L)
                continue
            }
            // Stop can land between the pre-spawn check and the spawn:
            // killProcessTree takes the same lock, so the child either
            // exists before the kill sweep or is destroyed here.
            synchronized(procLock) {
                if (stopping) {
                    np.destroyForcibly()
                    return
                }
                proc = np
            }
            broadcast("running")
            Thread(::drainOutput, "agent-out").start()
            backoff = 5_000L
        }
    }

    private fun killProcessTree() {
        synchronized(procLock) {
            try {
                proc?.destroyForcibly()
            } catch (e: Exception) {
                Log.w(TAG, "kill failed", e)
            }
            proc = null
        }
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
