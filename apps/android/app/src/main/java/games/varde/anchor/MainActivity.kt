package games.varde.anchor

import android.Manifest
import android.app.Activity
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.content.res.ColorStateList
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.PowerManager
import android.provider.Settings
import android.view.Gravity
import android.view.View
import android.widget.Button
import android.widget.EditText
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView

/**
 * Single screen: enroll (server + invite token) then start/stop the anchor.
 * Deliberately plain — this is a set-and-forget box-in-a-drawer app.
 * Styled with the mouve-dark palette (see res/values/colors.xml).
 */
class MainActivity : Activity() {

    private fun c(id: Int): Int = getColor(id)

    private val prefs by lazy { getSharedPreferences("varde", Context.MODE_PRIVATE) }
    private lateinit var statusView: TextView

    private val statusReceiver = object : BroadcastReceiver() {
        override fun onReceive(ctx: Context, intent: Intent) {
            statusView.text = intent.getStringExtra(AnchorService.EXTRA_STATUS)
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        // drop the platform ActionBar — it overlays the content in this
        // theme and we draw our own branded header
        requestWindowFeature(android.view.Window.FEATURE_NO_TITLE)
        if (Build.VERSION.SDK_INT >= 33) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), 1)
        }
        if (Build.VERSION.SDK_INT >= 33) {
            registerReceiver(statusReceiver, IntentFilter(AnchorService.ACTION_STATUS), RECEIVER_NOT_EXPORTED)
        } else {
            @Suppress("UnspecifiedRegisterReceiverFlag")
            registerReceiver(statusReceiver, IntentFilter(AnchorService.ACTION_STATUS))
        }
        setContentView(buildView())
        statusView.text = AnchorService.lastStatus
    }

    override fun onDestroy() {
        unregisterReceiver(statusReceiver)
        super.onDestroy()
    }

    private fun buildView(): View {
        val pad = (24 * resources.displayMetrics.density).toInt()
        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            setPadding(pad, pad, pad, pad)
            setBackgroundColor(c(R.color.bg))
        }

        val mark = ImageView(this).apply {
            setImageResource(R.mipmap.ic_launcher)
            val s = (32 * resources.displayMetrics.density).toInt()
            layoutParams = LinearLayout.LayoutParams(s, s)
        }
        val title = TextView(this).apply {
            text = "Varde Anchor"
            textSize = 24f
            setTextColor(c(R.color.text))
            gravity = Gravity.CENTER_VERTICAL
        }
        val header = LinearLayout(this).apply {
            orientation = LinearLayout.HORIZONTAL
            gravity = Gravity.CENTER_VERTICAL
            addView(mark)
            addView(title, LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, 1f).apply {
                marginStart = (12 * resources.displayMetrics.density).toInt()
            })
        }
        root.addView(header)

        statusView = TextView(this).apply {
            textSize = 14f
            setPadding(0, pad / 2, 0, pad / 2)
            setTextColor(c(R.color.text_dim))
        }
        root.addView(statusView)

        if (!prefs.getBoolean("enrolled", false)) {
            root.addView(enrollForm())
        } else {
            root.addView(controlButtons())
        }
        return ScrollView(this).apply {
            addView(root)
            setBackgroundColor(c(R.color.bg))
        }
    }

    private fun primaryButton(label: String) = Button(this).apply {
        text = label
        setTextColor(c(R.color.bg))
        backgroundTintList = ColorStateList.valueOf(c(R.color.glod))
    }

    private fun secondaryButton(label: String) = Button(this).apply {
        text = label
        setTextColor(c(R.color.text))
        backgroundTintList = ColorStateList.valueOf(c(R.color.surface))
    }

    private fun field(hintText: String) = EditText(this).apply {
        hint = hintText
        setSingleLine()
        setTextColor(c(R.color.text))
        setHintTextColor(c(R.color.text_dim))
        backgroundTintList = ColorStateList.valueOf(c(R.color.glod))
    }

    private fun enrollForm(): View {
        val col = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }

        val server = field("Control plane URL").apply {
            setText(prefs.getString("server", "https://cp.varde.games"))
        }
        val token = field("Invite token")
        val hint = TextView(this).apply {
            text = "Create an invite in the Varde dashboard, then paste the token here. This phone will enroll as an always-on save anchor."
            setPadding(0, 8, 0, 8)
            setTextColor(c(R.color.text_dim))
        }
        val err = TextView(this).apply { setTextColor(c(R.color.warn)); textSize = 12f }
        val btn = primaryButton("Enroll as anchor")

        btn.setOnClickListener {
            val s = server.text.toString().trim()
            val t = token.text.toString().trim()
            if (t.isEmpty()) {
                err.text = "Paste an invite token — the device flow needs a browser."
                return@setOnClickListener
            }
            btn.isEnabled = false
            err.text = ""
            Thread {
                val ok = enroll(s, t)
                runOnUiThread {
                    btn.isEnabled = true
                    if (ok) {
                        prefs.edit()
                            .putBoolean("enrolled", true)
                            .putBoolean("enabled", true)
                            .putString("server", s)
                            .apply()
                        setContentView(buildView())
                        statusView.text = AnchorService.lastStatus
                        startService(Intent(this, AnchorService::class.java))
                    } else {
                        err.text = "Enroll failed — check the token and server URL."
                    }
                }
            }.start()
        }
        col.addView(hint)
        col.addView(server)
        col.addView(token)
        col.addView(btn)
        col.addView(err)
        return col
    }

    private fun enroll(server: String, token: String): Boolean = try {
        val args = mutableListOf(
            AnchorService.agentBin(this), "enroll",
            "--data-dir", AnchorService.dataDir(this).absolutePath,
            "--anchor",
        )
        if (server.isNotEmpty()) {
            args.addAll(listOf("--server", server))
        }
        // `-` => the agent reads the token from stdin; keeps the invite
        // secret out of argv, which is world-readable on Android.
        if (token.isNotEmpty()) {
            args.addAll(listOf("--token", "-"))
        }
        AnchorService.procBuilder(this, *args.toTypedArray())
            .redirectErrorStream(true).start().let { p ->
            if (token.isNotEmpty()) {
                p.outputStream.use { it.write((token + "\n").toByteArray()); it.flush() }
            }
            p.inputStream.bufferedReader().readText() // drain output
            p.waitFor()
            val ok = p.exitValue() == 0
            // never log full output — server error bodies can echo secrets
            if (!ok) android.util.Log.e("Enroll", "enroll failed rc=${p.exitValue()}")
            ok
        }
    } catch (e: Exception) {
        android.util.Log.e("Enroll", "failed", e)
        false
    }

    private fun controlButtons(): View {
        val col = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            gravity = Gravity.CENTER_HORIZONTAL
        }
        val enabled = prefs.getBoolean("enabled", false)
        val toggle = primaryButton(if (enabled) "Stop anchor" else "Start anchor")
        toggle.setOnClickListener {
            val now = !prefs.getBoolean("enabled", false)
            prefs.edit().putBoolean("enabled", now).apply()
            if (now) {
                startService(Intent(this, AnchorService::class.java))
                toggle.text = "Stop anchor"
            } else {
                stopService(Intent(this, AnchorService::class.java))
                toggle.text = "Start anchor"
            }
        }
        col.addView(toggle)

        // Doze is the real anchor-killer; nudge the user to exempt the app.
        val pm = getSystemService(PowerManager::class.java)
        if (!pm.isIgnoringBatteryOptimizations(packageName)) {
            val bat = secondaryButton("Disable battery optimization")
            bat.setOnClickListener {
                startActivity(
                    Intent(
                        Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS,
                        Uri.parse("package:$packageName")
                    )
                )
            }
            col.addView(bat)
        }
        return col
    }
}
