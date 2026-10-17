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
import java.net.HttpURLConnection
import java.net.URL
import org.json.JSONObject

/**
 * Two screens on one surface: enrollment and, once enrolled, an anchor
 * dashboard with live status, node stats, the agent log tail, and
 * start/stop controls.
 *
 * Enrollment mirrors the Windows desktop flow: "Sign in with Varde" spawns
 * the agent's device-link enroll (it registers a pending link and polls for
 * approval) and opens varde.games/auth/desktop in the browser; the site
 * signs the user in and bounces back through `varde://auth/callback?token`.
 * We exchange that token for a CP session (POST /v1/auth/external), let the
 * user confirm the identity, pick or create their group, then approve the
 * device link (POST /v1/device-links/approve, anchor=true) — the agent's
 * poll completes and writes config.toml. A "Custom server or invite token"
 * toggle reveals the manual server+token form for self-hosted control
 * planes (the sub-1% path; `varde://enroll?server&code` still prefills it).
 * The ASCII-cloud glyph field (same WebGL shader as the varde.games landing
 * page and the Windows installer) renders behind the content and draws the
 * cairn into the mark slot; taps emit the same Glød shockwave.
 *
 * Widgets mirror the HeroUI/mouve-dark spec: pill buttons (rounded-3xl,
 * h-10, text-sm font-medium, accent on accent-foreground) with the web
 * primary's shine — a 1px dark outer ring, an accent border inside it, and
 * white insets hugging the top edge only, plus a real drop shadow. Fields are
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
    private var formCol: LinearLayout? = null
    // federated sign-in state machine (idle → waiting → busy → confirm|pick → enrolled)
    private var stage = "idle"
    private var stageDetail = ""
    private var errMsg = ""
    private var advanced = false
    private var enrollProc: Process? = null
    private var deviceCode: String? = null
    private var fedSession = ""
    private var fedName = ""
    private var fedEmail = ""
    private var fedGroups: List<Pair<String, String>> = emptyList()
    // bumped on every new sign-in and on cancel/fail so workers from a
    // stale attempt bail instead of mutating the new flow's state
    @Volatile private var fedGen = 0
    // field values kept across renderStage() — the form fields are
    // rebuilt on every stage change; synced back from the live fields
    // before each rebuild so user edits survive too
    private var pendingToken: String? = null
    private var pendingServer: String? = null
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

    override fun onPause() {
        // stop the 10s stats ticker while backgrounded — its posted
        // callback survives on the still-attached stats view otherwise
        statsView?.removeCallbacks(statsTick)
        super.onPause()
    }

    override fun onDestroy() {
        statsView?.removeCallbacks(statsTick)
        unregisterReceiver(statusReceiver)
        super.onDestroy()
    }

    /** Routes varde:// deeplinks: enroll-token prefill and the auth callback. */
    private fun applyDeeplink(intent: Intent?) {
        val u = intent?.data ?: return
        if (u.scheme != "varde" || prefs.getBoolean("enrolled", false)) return
        when (u.host) {
            // varde://enroll?server=<cp>&code=<invite> — prefill the manual form
            "enroll" -> {
                advanced = true
                u.getQueryParameter("server")?.let { s ->
                    prefs.edit().putString("server", s).apply()
                    pendingServer = s
                    serverField?.setText(s)
                }
                (u.getQueryParameter("code") ?: u.getQueryParameter("token"))?.let {
                    pendingToken = it
                    tokenField?.setText(it)
                }
                renderStage()
            }
            // varde://auth/callback?token=<federation jwt> — the site's
            // sign-in page sends users back here, like the desktop app.
            // Only while a sign-in we started is pending: an unsolicited
            // callback could otherwise enroll into a foreign account.
            "auth" -> if (u.path == "/callback" && stage == "waiting") {
                u.getQueryParameter("token")?.let { fedExchange(it) }
            }
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
        formCol = form
        val child = if (!enrolled) stageView() else enrolledView()
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

    private fun shinePill(fill: Int, border: Int) =
        ShineButtonDrawable(fill, border, dp(22).toFloat(), resources.displayMetrics.density)

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

    // ---------- federated enroll (mirrors the desktop app flow) ----------

    private val hostedCp = "https://cp.varde.games"
    private val signInUrl = "https://varde.games/auth/desktop"

    private fun renderStage() {
        // keep the latest field text — the rebuild below recreates the
        // EditTexts and would otherwise discard what the user typed
        tokenField?.text?.toString()?.let { pendingToken = it }
        serverField?.text?.toString()?.let { pendingServer = it }
        val col = formCol ?: return
        col.removeAllViews()
        col.addView(stageView(), LinearLayout.LayoutParams(
            LinearLayout.LayoutParams.MATCH_PARENT,
            LinearLayout.LayoutParams.WRAP_CONTENT,
        ))
    }

    private fun stageView(): View {
        val col = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL }
        when (stage) {
            "waiting" -> {
                val btn = primaryButton("Sign in with Varde")
                btn.setOnClickListener { startFedSignIn() }
                col.addView(btn, btnLp(dp(12)))
                col.addView(statusLine("Finish signing in in your browser — we'll pick back up here."))
                col.addView(cancelLink())
            }
            "busy" -> {
                col.addView(statusLine(stageDetail))
                col.addView(cancelLink())
            }
            "confirm" -> {
                col.addView(heading("Link this device to your Varde account?"))
                val who = listOf(fedName, fedEmail).filter { it.isNotEmpty() }.joinToString(" · ")
                if (who.isNotEmpty()) col.addView(subLine(who))
                val btn = primaryButton("Continue")
                btn.setOnClickListener { fedGroups() }
                col.addView(btn, btnLp(dp(8)))
                col.addView(cancelLink())
            }
            "pick" -> {
                col.addView(heading("Choose a group"))
                for ((id, name) in fedGroups) {
                    val b = secondaryButton(name)
                    b.setOnClickListener { fedApprove(id) }
                    col.addView(b, btnLp(dp(10)))
                }
                col.addView(cancelLink())
            }
            else -> buildEnrollForm(col)
        }
        return col
    }

    private fun buildEnrollForm(col: LinearLayout) {
        val signIn = primaryButton("Sign in with Varde")
        signIn.setOnClickListener { startFedSignIn() }
        col.addView(signIn, btnLp(0))
        if (errMsg.isNotEmpty()) col.addView(errLine())

        // sub-1% path: self-hosted CP or a manually minted invite token —
        // kept small so the hosted flow is the obvious choice
        col.addView(TextView(this).apply {
            text = if (advanced) "Hide custom server options" else "Custom server or invite token"
            textSize = 12f
            typeface = fontRegular
            setTextColor(c(R.color.text_dim))
            gravity = Gravity.CENTER
            setPadding(0, dp(14), 0, 0)
            setOnClickListener { advanced = !advanced; renderStage() }
        })
        if (!advanced) return

        val server = field("Control plane URL").apply {
            setText(pendingServer ?: prefs.getString("server", "https://cp.varde.games"))
        }
        serverField = server
        val token = field("Invite token").apply {
            pendingToken?.let { setText(it) }
        }
        tokenField = token
        val btn = secondaryButton("Enroll with invite token")
        btn.setOnClickListener {
            val s = server.text.toString().trim()
            // accept either the bare code or a pasted https://…/invite/<code>
            val t = token.text.toString().trim().substringAfterLast('/')
            if (t.isEmpty()) {
                errMsg = "Paste an invite token."
                renderStage()
                return@setOnClickListener
            }
            btn.isEnabled = false
            Thread {
                val ok = enroll(s, t)
                runOnUiThread {
                    btn.isEnabled = true
                    if (ok) {
                        onEnrolled(s.ifEmpty { hostedCp })
                    } else {
                        errMsg = "Enroll failed — check the token and server URL."
                        renderStage()
                    }
                }
            }.start()
        }
        col.addView(server, wrapLp(dp(14)))
        col.addView(token, wrapLp(dp(16)))
        col.addView(btn, btnLp(0))
    }

    private fun btnLp(bottom: Int) = LinearLayout.LayoutParams(
        LinearLayout.LayoutParams.MATCH_PARENT, dp(40),
    ).apply { bottomMargin = bottom }

    private fun wrapLp(bottom: Int) = LinearLayout.LayoutParams(
        LinearLayout.LayoutParams.MATCH_PARENT, LinearLayout.LayoutParams.WRAP_CONTENT,
    ).apply { bottomMargin = bottom }

    private fun heading(t: String) = TextView(this).apply {
        text = t
        textSize = 16f
        typeface = fontMedium
        setTextColor(c(R.color.text))
        gravity = Gravity.CENTER
        setPadding(0, 0, 0, dp(6))
    }

    private fun subLine(t: String) = TextView(this).apply {
        text = t
        textSize = 13f
        typeface = fontRegular
        setTextColor(c(R.color.text_dim))
        gravity = Gravity.CENTER
        setPadding(0, 0, 0, dp(14))
    }

    private fun statusLine(t: String) = TextView(this).apply {
        text = t
        textSize = 13f
        typeface = fontRegular
        setTextColor(c(R.color.text_dim))
        gravity = Gravity.CENTER
        setPadding(0, 0, 0, dp(4))
    }

    private fun errLine() = TextView(this).apply {
        text = errMsg
        textSize = 12f
        typeface = fontRegular
        setTextColor(c(R.color.warn))
        gravity = Gravity.CENTER
        setPadding(0, dp(8), 0, 0)
    }

    private fun cancelLink() = TextView(this).apply {
        text = "Cancel"
        textSize = 12f
        typeface = fontRegular
        setTextColor(c(R.color.text_dim))
        gravity = Gravity.CENTER
        setPadding(0, dp(12), 0, 0)
        setOnClickListener { cancelFed() }
    }

    /** Open the site's sign-in page; the device link starts polling meanwhile. */
    private fun startFedSignIn() {
        fedGen++ // invalidate stragglers from any previous attempt
        stage = "waiting"
        errMsg = ""
        renderStage()
        startDeviceEnroll()
        try {
            startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(signInUrl)))
        } catch (e: Exception) {
            stage = "idle"
            errMsg = "No browser found to open varde.games."
            renderStage()
        }
    }

    /**
     * `varde-agent enroll` (no token) registers a device link and polls until
     * it's approved; stdout carries "enter code: <user_code>". We grab the
     * code so the federated session can approve it. On success the agent
     * writes config.toml — the same artifact the token path produces.
     */
    private fun startDeviceEnroll() {
        enrollProc?.destroy()
        deviceCode = null
        val p = try {
            AnchorService.procBuilder(
                this,
                AnchorService.agentBin(this), "enroll",
                "--data-dir", AnchorService.dataDir(this).absolutePath,
                "--anchor",
            ).redirectErrorStream(true).start()
        } catch (e: Exception) {
            android.util.Log.e("Enroll", "device enroll spawn failed", e)
            stage = "idle"
            errMsg = "Couldn't start the agent."
            renderStage()
            return
        }
        enrollProc = p
        Thread {
            var ok = false
            try {
                p.inputStream.bufferedReader().forEachLine { line ->
                    // "Visit <url> and enter code: ABCD-EFGH"
                    Regex("enter code:\\s*(\\S+)").find(line)?.let {
                        deviceCode = it.groupValues[1]
                    }
                }
                val rc = p.waitFor()
                ok = rc == 0 &&
                    java.io.File(AnchorService.dataDir(this), "config.toml").exists()
            } catch (_: Exception) {}
            val succeeded = ok
            runOnUiThread {
                if (p !== enrollProc) return@runOnUiThread // superseded/cancelled
                if (succeeded) {
                    onEnrolled(hostedCp)
                } else if (stage != "idle") {
                    stage = "idle"
                    errMsg = "Sign-in didn't finish — the link may have expired. Try again."
                    renderStage()
                }
            }
        }.start()
    }

    /** Site JWT → hosted-CP session, exactly like the desktop's fed_exchange. */
    private fun fedExchange(token: String) {
        // a fresh process may have lost its device link — restart it so an
        // approval always has something pending to land on
        if (enrollProc == null) startDeviceEnroll()
        stage = "busy"
        stageDetail = "Signing you in…"
        renderStage()
        val gen = fedGen
        Thread {
            try {
                val r = httpJson(
                    "POST", "$hostedCp/v1/auth/external",
                    "{\"token\":${JSONObject.quote(token)}}", null,
                )
                // check BEFORE touching fedSession — a stale attempt must
                // not overwrite the session of a sign-in started after it
                if (gen != fedGen) return@Thread
                fedSession = r.getString("token")
                val u = r.getJSONObject("user")
                fedName = u.optString("display_name").ifEmpty { u.optString("email") }
                fedEmail = u.optString("email")
                runOnUiThread {
                    if (gen == fedGen) {
                        stage = "confirm"
                        renderStage()
                    }
                }
            } catch (e: Exception) {
                android.util.Log.e("Enroll", "fed exchange failed", e)
                runOnUiThread {
                    if (gen == fedGen) {
                        failFed("Sign-in failed — the link may have expired. Try again.")
                    }
                }
            }
        }.start()
    }

    /** Resolve the group like the desktop: none → create, one → use, many → pick. */
    private fun fedGroups() {
        stage = "busy"
        stageDetail = "Looking up your groups…"
        renderStage()
        val gen = fedGen
        Thread {
            try {
                val gl = httpJson("GET", "$hostedCp/v1/groups", null, fedSession)
                val arr = gl.optJSONArray("groups")
                when {
                    arr == null || arr.length() == 0 -> {
                        if (gen != fedGen) return@Thread
                        val g = httpJson(
                            "POST", "$hostedCp/v1/groups",
                            "{\"name\":${JSONObject.quote("$fedName's group")}}", fedSession,
                        )
                        fedApprove(g.getString("id"), gen)
                    }
                    arr.length() == 1 -> fedApprove(arr.getJSONObject(0).getString("id"), gen)
                    else -> {
                        if (gen != fedGen) return@Thread
                        fedGroups = (0 until arr.length()).map {
                            arr.getJSONObject(it).let { o ->
                                o.getString("id") to o.optString("name").ifEmpty { "group" }
                            }
                        }
                        runOnUiThread {
                            if (gen == fedGen) {
                                stage = "pick"
                                renderStage()
                            }
                        }
                    }
                }
            } catch (e: Exception) {
                android.util.Log.e("Enroll", "group fetch failed", e)
                runOnUiThread {
                    if (gen == fedGen) failFed("Couldn't fetch your groups.")
                }
            }
        }.start()
    }

    /** Approve our pending device link as an anchor node in the group. */
    private fun fedApprove(groupId: String, gen: Int = fedGen) {
        runOnUiThread {
            stage = "busy"
            stageDetail = "Approving this device…"
            renderStage()
        }
        Thread {
            try {
                // the enroll proc may still be printing its code — wait for it
                var code = deviceCode
                var waited = 0
                while (code == null && waited < 120 && gen == fedGen) {
                    Thread.sleep(100)
                    code = deviceCode
                    waited++
                }
                // a cancelled sign-in must not post an approval — the CP
                // would create a node that can never finish enrolling
                if (gen != fedGen) return@Thread
                if (code == null) throw IllegalStateException("no device link pending")
                val dl = httpJson("GET", "$hostedCp/v1/device-links/$code", null, fedSession)
                val name = dl.optString("hostname").ifEmpty { "Android anchor" }
                // final gate right before the POST — a cancel landing
                // between the code wait and here must not create a node
                if (gen != fedGen) return@Thread
                httpJson(
                    "POST", "$hostedCp/v1/device-links/approve",
                    "{\"user_code\":${JSONObject.quote(code)}," +
                        "\"group_id\":${JSONObject.quote(groupId)}," +
                        "\"name\":${JSONObject.quote(name)},\"anchor\":true}",
                    fedSession,
                )
                runOnUiThread {
                    if (gen == fedGen) {
                        stage = "busy"
                        stageDetail = "Finishing the link — the agent is picking it up…"
                        renderStage()
                    }
                }
                // the agent's poll sees the approval and writes config.toml;
                // the watcher thread in startDeviceEnroll calls onEnrolled
            } catch (e: Exception) {
                android.util.Log.e("Enroll", "link approve failed", e)
                runOnUiThread {
                    if (gen == fedGen) failFed("Couldn't approve the device link.")
                }
            }
        }.start()
    }

    private fun cancelFed() {
        fedGen++
        enrollProc?.let {
            enrollProc = null
            it.destroy()
        }
        deviceCode = null
        fedSession = ""
        fedGroups = emptyList()
        stage = "idle"
        stageDetail = ""
        errMsg = ""
        renderStage()
    }

    private fun failFed(msg: String) {
        fedGen++
        enrollProc?.let {
            enrollProc = null
            it.destroy()
        }
        errMsg = msg
        stage = "idle"
        renderStage()
    }

    private fun onEnrolled(server: String) {
        prefs.edit()
            .putBoolean("enrolled", true)
            .putBoolean("enabled", true)
            .putString("server", server)
            .apply()
        stage = "idle"
        stageDetail = ""
        errMsg = ""
        deviceCode = null
        fedSession = ""
        setContentView(buildView())
        statusView?.text = AnchorService.lastStatus
        startService(Intent(this, AnchorService::class.java))
    }

    /** Minimal JSON HTTP call — the app ships no dependencies. */
    private fun httpJson(
        method: String,
        url: String,
        body: String?,
        bearer: String?,
    ): JSONObject {
        val conn = URL(url).openConnection() as HttpURLConnection
        try {
            conn.requestMethod = method
            conn.connectTimeout = 15_000
            conn.readTimeout = 15_000
            conn.setRequestProperty("Content-Type", "application/json")
            bearer?.let { conn.setRequestProperty("Authorization", "Bearer $it") }
            if (body != null) {
                conn.doOutput = true
                conn.outputStream.use { it.write(body.toByteArray()) }
            }
            val code = conn.responseCode
            val stream = if (code in 200..299) conn.inputStream else conn.errorStream
            val text = stream?.bufferedReader()?.readText() ?: ""
            if (code !in 200..299) throw java.io.IOException("$method $url → $code")
            return if (text.isEmpty()) JSONObject() else JSONObject(text)
        } finally {
            conn.disconnect()
        }
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
