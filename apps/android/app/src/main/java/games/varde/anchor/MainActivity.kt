package games.varde.anchor

import android.Manifest
import android.app.Activity
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.PackageManager
import android.content.res.ColorStateList
import android.graphics.Typeface
import android.graphics.drawable.GradientDrawable
import android.graphics.drawable.LayerDrawable
import android.graphics.drawable.RippleDrawable
import android.graphics.drawable.StateListDrawable
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.PowerManager
import android.provider.Settings
import android.view.Gravity
import android.view.MotionEvent
import android.view.View
import android.view.ViewGroup
import android.widget.Button
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView

/**
 * Single screen: enroll (server + invite token) then start/stop the anchor.
 * The ASCII-cloud glyph field (same WebGL shader as the varde.games landing
 * page and the Windows installer) renders behind the content and draws the
 * cairn into the mark slot; taps emit the same Glød shockwave.
 *
 * Widgets mirror the HeroUI/mouve-dark spec: pill buttons (rounded-3xl,
 * h-10, text-sm font-medium, accent on accent-foreground), pill fields
 * (rounded-field, field-background + field-border, focus → accent border).
 * Typeface is bundled Inter at the same weights the site uses.
 */
class MainActivity : Activity() {

    private fun c(id: Int): Int = getColor(id)

    private val prefs by lazy { getSharedPreferences("varde", Context.MODE_PRIVATE) }
    private lateinit var statusView: TextView
    private lateinit var cloudView: CloudView
    private var fontRegular: Typeface? = null
    private var fontMedium: Typeface? = null
    private var fontBold: Typeface? = null

    private val statusReceiver = object : BroadcastReceiver() {
        override fun onReceive(ctx: Context, intent: Intent) {
            statusView.text = intent.getStringExtra(AnchorService.EXTRA_STATUS)
        }
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        fontRegular = Typeface.createFromAsset(assets, "fonts/inter_regular.ttf")
        fontMedium = Typeface.createFromAsset(assets, "fonts/inter_medium.ttf")
        fontBold = Typeface.createFromAsset(assets, "fonts/inter_bold.ttf")
        // platform ActionBar overlays content under this theme — drop it,
        // we draw the branded header inside the glyph field
        requestWindowFeature(android.view.Window.FEATURE_NO_TITLE)
        window.setBackgroundDrawable(backdrop())
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

    /** Landing-page base gradient behind the translucent field. */
    private fun backdrop() = LayerDrawable(arrayOf(
        GradientDrawable().apply { setColor(c(R.color.bg)) },
        GradientDrawable().apply {
            shape = GradientDrawable.RECTANGLE
            gradientType = GradientDrawable.RADIAL_GRADIENT
            setGradientCenter(0.7f, 0.2f)
            gradientRadius = dp(420).toFloat()
            setColors(intArrayOf(0x0DFF5B1F, 0x00FF5B1F))
        },
        GradientDrawable().apply {
            shape = GradientDrawable.RECTANGLE
            gradientType = GradientDrawable.RADIAL_GRADIENT
            setGradientCenter(0.2f, 0.8f)
            gradientRadius = dp(320).toFloat()
            setColors(intArrayOf(0x5938312F, 0x0038312F))
        },
    ))

    override fun dispatchTouchEvent(ev: MotionEvent): Boolean {
        if (ev.actionMasked == MotionEvent.ACTION_DOWN) {
            cloudView.addRipple(ev.x, cloudView.height - ev.y)
        }
        return super.dispatchTouchEvent(ev)
    }

    private fun dp(v: Int) = (v * resources.displayMetrics.density).toInt()

    private fun buildView(): View {
        val pad = dp(24)
        val maxW = dp(340)
        val frame = FrameLayout(this)
        cloudView = CloudView(this)
        frame.addView(cloudView, ViewGroup.LayoutParams(
            ViewGroup.LayoutParams.MATCH_PARENT, ViewGroup.LayoutParams.MATCH_PARENT,
        ))

        val root = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            gravity = Gravity.CENTER
            setPadding(pad, pad, pad, pad)
        }
        // keep content inside the status/nav bars and display cutouts
        root.setOnApplyWindowInsetsListener { v, insets ->
            v.setPadding(
                pad + insets.systemWindowInsetLeft,
                pad + insets.systemWindowInsetTop,
                pad + insets.systemWindowInsetRight,
                pad + insets.systemWindowInsetBottom,
            )
            insets
        }

        // the field draws the cairn into this slot — report its rect to the
        // shader in window/surface px (y from bottom), same contract as the web
        val markSlot = View(this)
        markSlot.addOnLayoutChangeListener { v, _, _, _, _, _, _, _, _ ->
            val pos = IntArray(2)
            v.getLocationInWindow(pos)
            cloudView.setMarkRect(
                pos[0].toFloat(),
                cloudView.height - (pos[1] + v.height).toFloat(),
                v.width.toFloat(),
                v.height.toFloat(),
            )
        }
        root.addView(markSlot, LinearLayout.LayoutParams(dp(140), dp(140)).apply {
            bottomMargin = dp(14)
        })

        val title = TextView(this).apply {
            text = "Varde Anchor"
            textSize = 26f
            typeface = fontBold
            setTextColor(c(R.color.text))
            gravity = Gravity.CENTER
        }
        root.addView(title)

        statusView = TextView(this).apply {
            textSize = 14f
            typeface = fontRegular
            setTextColor(c(R.color.text_dim))
            setPadding(0, dp(10), 0, dp(18))
            gravity = Gravity.CENTER
        }
        root.addView(statusView)

        val form = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        val child = if (!prefs.getBoolean("enrolled", false)) enrollForm() else controlButtons()
        form.addView(child, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT,
            LinearLayout.LayoutParams.WRAP_CONTENT,
        ))
        root.addView(form, LinearLayout.LayoutParams(maxW, LinearLayout.LayoutParams.WRAP_CONTENT))

        val scroll = ScrollView(this).apply {
            addView(root)
            isFillViewport = true
        }
        frame.addView(scroll)
        return frame
    }

    private fun pill(fill: Int, stroke: Int = 0, radius: Int = 22) =
        GradientDrawable().apply {
            setColor(fill)
            cornerRadius = dp(radius).toFloat()
            if (stroke != 0) setStroke(dp(1), stroke)
        }

    private fun styledButton(label: String, bg: Int, fg: Int) = Button(this).apply {
        text = label
        isAllCaps = false
        textSize = 14f
        typeface = fontMedium
        // HeroUI .button: disabled → status-disabled (muted, not tinted)
        setTextColor(ColorStateList(
            arrayOf(intArrayOf(-android.R.attr.state_enabled), intArrayOf()),
            intArrayOf(c(R.color.text_dim), fg),
        ))
        val states = StateListDrawable().apply {
            addState(intArrayOf(-android.R.attr.state_enabled), pill(c(R.color.field_bg), c(R.color.field_border)))
            addState(intArrayOf(), pill(bg))
        }
        // opaque mask clips the ripple to the pill shape
        background = RippleDrawable(
            ColorStateList.valueOf(0x33ffffff), states, pill(-0x1000000),
        )
        minimumHeight = 0
        minHeight = 0
        setPadding(0, 0, 0, 0)
        layoutParams = ViewGroup.LayoutParams(
            ViewGroup.LayoutParams.MATCH_PARENT, dp(40),
        )
        // web :active → transform: scale(0.97)
        setOnTouchListener { v, ev ->
            when (ev.actionMasked) {
                MotionEvent.ACTION_DOWN -> v.animate().scaleX(0.97f).scaleY(0.97f).setDuration(100).start()
                MotionEvent.ACTION_UP, MotionEvent.ACTION_CANCEL ->
                    v.animate().scaleX(1f).scaleY(1f).setDuration(250).start()
            }
            false
        }
    }

    private fun primaryButton(label: String) = styledButton(label, c(R.color.glod), c(R.color.accent_fg))

    private fun secondaryButton(label: String) = styledButton(label, c(R.color.surface_300), c(R.color.text))

    /** HeroUI .input: rounded-field, field bg + border, accent border on focus. */
    private fun field(hintText: String) = EditText(this).apply {
        hint = hintText
        setSingleLine()
        textSize = 14f
        typeface = fontRegular
        setTextColor(c(R.color.text))
        setHintTextColor(c(R.color.text_dim))
        val normal = pill(c(R.color.field_bg), c(R.color.field_border))
        val focused = pill(c(R.color.field_bg), c(R.color.glod))
        background = StateListDrawable().apply {
            addState(intArrayOf(android.R.attr.state_focused), focused)
            addState(intArrayOf(), normal)
        }
        setPadding(dp(14), 0, dp(14), 0)
        minHeight = dp(44)
        layoutParams = LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT,
            LinearLayout.LayoutParams.WRAP_CONTENT,
        )
    }

    private fun enrollForm(): View {
        val col = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }

        val hint = TextView(this).apply {
            text = "Create an invite in the Varde dashboard, then paste the token here. This phone will enroll as an always-on save anchor."
            textSize = 13f
            typeface = fontRegular
            setTextColor(c(R.color.text_dim))
            setPadding(0, 0, 0, dp(16))
        }
        val server = field("Control plane URL").apply {
            setText(prefs.getString("server", "https://cp.varde.games"))
        }
        val token = field("Invite token")
        val err = TextView(this).apply {
            setTextColor(c(R.color.warn))
            textSize = 12f
            typeface = fontRegular
            setPadding(0, dp(8), 0, 0)
        }
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
        col.addView(server, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT,
        ).apply { bottomMargin = dp(10) })
        col.addView(token, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT,
        ).apply { bottomMargin = dp(16) })
        col.addView(btn, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT, dp(40),
        ))
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
        col.addView(toggle, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT, dp(40),
        ))

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
            col.addView(bat, LinearLayout.LayoutParams(
                LinearLayout.LayoutParams.MATCH_PARENT, dp(40),
            ).apply { topMargin = dp(12) })
        }
        return col
    }
}
