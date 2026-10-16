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
 * Two screens on one surface: the enroll form (server + invite token, plus a
 * "Create invite token" button that hands off to the dashboard in the
 * browser — the `varde://enroll?server&code` deeplink on the invite card
 * carries the token back in) and, once enrolled, an anchor dashboard with
 * live status, node stats, the agent log tail, and start/stop controls.
 * The ASCII-cloud glyph field (same WebGL shader as the varde.games landing
 * page and the Windows installer) renders behind the content and draws the
 * cairn into the mark slot; taps emit the same Glød shockwave.
 *
 * Widgets mirror the HeroUI/mouve-dark spec: pill buttons (rounded-3xl,
 * h-10, text-sm font-medium, accent on accent-foreground) with the web
 * primary's shine — a 1px dark outer ring, an accent border inside it, a
 * white top-inset gradient, and a real drop shadow. Fields are
 * rounded-field, field-background + field-border, focus → accent border.
 * Typeface is bundled Inter at the same weights the site uses.
 */
class MainActivity : Activity() {

    private fun c(id: Int): Int = getColor(id)

    private val prefs by lazy { getSharedPreferences("varde", Context.MODE_PRIVATE) }
    private var statusView: TextView? = null
    private var statsView: LinearLayout? = null
    private var logsView: TextView? = null
    private var logScroll: ScrollView? = null
    private var serverField: EditText? = null
    private var tokenField: EditText? = null
    private lateinit var cloudView: CloudView
    private var fontRegular: Typeface? = null
    private var fontMedium: Typeface? = null
    private var fontBold: Typeface? = null

    private val statusReceiver = object : BroadcastReceiver() {
        override fun onReceive(ctx: Context, intent: Intent) {
            statusView?.text = intent.getStringExtra(AnchorService.EXTRA_STATUS)
            refreshLogs()
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
        applyDeeplink(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        applyDeeplink(intent)
    }

    override fun onResume() {
        super.onResume()
        statusView?.text = AnchorService.lastStatus
        refreshLogs()
        loadStats()
    }

    override fun onDestroy() {
        unregisterReceiver(statusReceiver)
        super.onDestroy()
    }

    /** varde://enroll?server=<cp>&code=<invite> from the dashboard invite card. */
    private fun applyDeeplink(intent: Intent?) {
        val u = intent?.data ?: return
        if (u.scheme != "varde" || u.host != "enroll") return
        if (prefs.getBoolean("enrolled", false)) return
        u.getQueryParameter("server")?.let { s ->
            prefs.edit().putString("server", s).apply()
            serverField?.setText(s)
        }
        (u.getQueryParameter("code") ?: u.getQueryParameter("token"))?.let {
            tokenField?.setText(it)
        }
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
            gravity = Gravity.CENTER_HORIZONTAL
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
            setPadding(0, 0, 0, dp(18))
        }
        root.addView(title)

        val enrolled = prefs.getBoolean("enrolled", false)
        val form = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        val child = if (!enrolled) enrollForm() else enrolledView()
        form.addView(child, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT,
            LinearLayout.LayoutParams.WRAP_CONTENT,
        ))
        // the enroll form sits centered like the web sign-in card; the
        // enrolled dashboard top-aligns so stats/logs stay visible
        val params = LinearLayout.LayoutParams(maxW, LinearLayout.LayoutParams.WRAP_CONTENT)
        if (!enrolled) {
            root.gravity = Gravity.CENTER
            params.gravity = Gravity.CENTER_HORIZONTAL
        } else {
            root.gravity = Gravity.CENTER_HORIZONTAL
        }
        root.addView(form, params)

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

    /**
     * The web primary button's shine: rgba(0,0,0,.4) 0 0 0 1px outer ring,
     * a 1px accent border inside it, and stacked white insets brightest at
     * the top edge (white .2 hard line, fading over ~8px). Elevation on the
     * button itself carries the layered drop shadow.
     */
    private fun shinePill(fill: Int, border: Int): LayerDrawable {
        val base = pill(fill, 0x66000000)
        val inner = GradientDrawable().apply {
            cornerRadius = dp(21).toFloat()
            setColor(0x00000000)
            setStroke(dp(1), border)
        }
        val shine = GradientDrawable().apply {
            cornerRadius = dp(20).toFloat()
            orientation = GradientDrawable.Orientation.TOP_BOTTOM
            setColors(intArrayOf(0x33FFFFFF, 0x14FFFFFF, 0x00FFFFFF))
        }
        return LayerDrawable(arrayOf(base, inner, shine)).apply {
            setLayerInset(1, dp(1), dp(1), dp(1), dp(1))
            setLayerInset(2, dp(2), dp(2), dp(2), dp(2))
        }
    }

    private fun styledButton(label: String, bg: Int, fg: Int, border: Int) = Button(this).apply {
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
            addState(
                intArrayOf(-android.R.attr.state_enabled),
                shinePill(c(R.color.field_bg), c(R.color.field_border)),
            )
            addState(intArrayOf(), shinePill(bg, border))
        }
        // opaque mask clips the ripple to the pill shape
        background = RippleDrawable(
            ColorStateList.valueOf(0x33ffffff), states, pill(-0x1000000),
        )
        elevation = dp(6).toFloat()
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

    private fun primaryButton(label: String) =
        styledButton(label, c(R.color.glod), c(R.color.accent_fg), c(R.color.glod))

    private fun secondaryButton(label: String) =
        styledButton(label, c(R.color.surface_300), c(R.color.text), c(R.color.field_border))

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

        val server = field("Control plane URL").apply {
            setText(prefs.getString("server", "https://cp.varde.games"))
        }
        serverField = server
        val token = field("Invite token")
        tokenField = token

        // hands off to the dashboard; the invite card links back via
        // varde://enroll so the token lands in the field below directly
        val create = secondaryButton("Create invite token")
        create.setOnClickListener {
            val s = server.text.toString().trim().removeSuffix("/")
                .ifEmpty { "https://cp.varde.games" }
            startActivity(Intent(Intent.ACTION_VIEW, Uri.parse("$s/machines/add")))
        }

        val err = TextView(this).apply {
            setTextColor(c(R.color.warn))
            textSize = 12f
            typeface = fontRegular
            setPadding(0, dp(8), 0, 0)
        }
        val btn = primaryButton("Enroll as anchor")

        btn.setOnClickListener {
            val s = server.text.toString().trim()
            // accept either the bare code or a pasted https://…/invite/<code>
            val t = token.text.toString().trim().substringAfterLast('/')
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
                        statusView?.text = AnchorService.lastStatus
                        startService(Intent(this, AnchorService::class.java))
                    } else {
                        err.text = "Enroll failed — check the token and server URL."
                    }
                }
            }.start()
        }
        col.addView(create, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT, dp(40),
        ).apply { bottomMargin = dp(14) })
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

    /** Surface bg card with a dim caps header, like the dashboard sections. */
    private fun card(label: String): LinearLayout {
        val v = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            background = pill(c(R.color.surface), c(R.color.surface_300), radius = 14)
            setPadding(dp(14), dp(10), dp(14), dp(12))
        }
        v.addView(TextView(this).apply {
            text = label.uppercase()
            textSize = 11f
            typeface = fontMedium
            setTextColor(c(R.color.text_dim))
            letterSpacing = 0.08f
            setPadding(0, 0, 0, dp(6))
        })
        return v
    }

    private fun statRow(k: String, v: String) = LinearLayout(this).apply {
        orientation = LinearLayout.HORIZONTAL
        addView(TextView(context).apply {
            text = k
            textSize = 12f
            typeface = fontRegular
            setTextColor(c(R.color.text_dim))
        }, LinearLayout.LayoutParams(dp(80), LinearLayout.LayoutParams.WRAP_CONTENT))
        addView(TextView(context).apply {
            text = v
            textSize = 12f
            typeface = fontRegular
            setTextColor(c(R.color.text))
        }, LinearLayout.LayoutParams(0, LinearLayout.LayoutParams.WRAP_CONTENT, 1f))
    }

    private fun enrolledView(): View {
        val col = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }

        val statusCard = card("Status")
        statusView = TextView(this).apply {
            text = AnchorService.lastStatus
            textSize = 14f
            typeface = fontMedium
            setTextColor(c(R.color.text))
        }
        statusCard.addView(statusView)
        col.addView(statusCard, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT,
        ).apply { bottomMargin = dp(12) })

        val statsCard = card("Node")
        statsView = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        statsCard.addView(statsView)
        col.addView(statsCard, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT,
        ).apply { bottomMargin = dp(12) })

        val logsCard = card("Logs")
        logScroll = ScrollView(this)
        logsView = TextView(this).apply {
            textSize = 11f
            typeface = Typeface.MONOSPACE
            setTextColor(c(R.color.text_dim))
        }
        logScroll?.addView(logsView)
        logsCard.addView(logScroll, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT, dp(160),
        ))
        col.addView(logsCard, LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT,
        ).apply { bottomMargin = dp(12) })

        col.addView(controlButtons())
        refreshLogs()
        loadStats()
        return col
    }

    private fun refreshLogs() {
        logsView?.text = AnchorService.logSnapshot().joinToString("\n")
        logScroll?.post { logScroll?.fullScroll(View.FOCUS_DOWN) }
    }

    // one shared ticker — each post replaces the last, so pause/resume
    // cycles never accumulate parallel exec loops
    private val statsTick = Runnable { loadStats() }

    /** `varde-agent status` + service uptime, refreshed on resume. */
    private fun loadStats() {
        val sv = statsView ?: return
        Thread {
            val rows = mutableListOf<Pair<String, String>>()
            val started = AnchorService.startedAt
            if (started > 0) {
                val s = (System.currentTimeMillis() - started) / 1000
                rows += "uptime" to if (s >= 3600) "${s / 3600}h ${s % 3600 / 60}m" else "${s / 60}m ${s % 60}s"
            }
            try {
                val p = AnchorService.procBuilder(
                    this, AnchorService.agentBin(this), "status",
                    "--data-dir", AnchorService.dataDir(this).absolutePath,
                ).redirectErrorStream(true).start()
                val out = p.inputStream.bufferedReader().readText()
                p.waitFor()
                val labels = mapOf(
                    "node_id" to "node",
                    "group_id" to "group",
                    "control_plane" to "server",
                    "anchor" to "anchor",
                )
                out.lines().filter { ':' in it }.forEach { l ->
                    val k = l.substringBefore(':')
                    val v = l.substringAfter(':').trim()
                    rows += (labels[k] ?: k) to v
                }
            } catch (_: Exception) {
            }
            runOnUiThread {
                if (sv !== statsView) return@runOnUiThread
                sv.removeAllViews()
                rows.forEach { (k, v) -> sv.addView(statRow(k, v)) }
                // uptime ticks while this view is attached; dies with it
                sv.removeCallbacks(statsTick)
                sv.postDelayed(statsTick, 10_000)
            }
        }.start()
    }

    private fun controlButtons(): View {
        val col = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
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
