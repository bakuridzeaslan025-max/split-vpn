package org.duckdns.splitvpn

import android.Manifest
import android.app.Activity
import android.app.Dialog
import android.app.NotificationManager
import android.content.ClipData
import android.content.ComponentName
import android.content.Intent
import android.content.SharedPreferences
import android.content.pm.ActivityInfo
import android.content.pm.PackageManager
import android.graphics.Bitmap
import android.graphics.Color
import android.graphics.drawable.ColorDrawable
import android.graphics.drawable.GradientDrawable
import android.net.Uri
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.Looper
import android.os.PowerManager
import android.os.SystemClock
import android.provider.Settings
import android.util.TypedValue
import android.view.Gravity
import android.view.View
import android.view.ViewGroup
import android.view.WindowManager
import android.view.accessibility.AccessibilityNodeInfo
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.ImageView
import android.widget.LinearLayout
import android.widget.PopupWindow
import android.widget.Switch
import android.widget.TextView
import android.widget.Toast
import androidx.core.content.FileProvider
import com.google.zxing.BarcodeFormat
import com.google.zxing.EncodeHintType
import com.google.zxing.qrcode.QRCodeWriter
import java.io.IOException

class MainActivity : Activity() {

    companion object {
        // Waiting for the network: a calm state, not the red of an error.
        private const val AMBER = 0xFFF9AB00.toInt()
        private const val AMBER_BG = 0xFFFEF7E0.toInt()
        private const val AMBER_INK = 0xFF7A4F01.toInt()

        private const val VPN_REQUEST_CODE = 1
        private const val NOTIFICATION_REQUEST_CODE = 2

        /** From the VPN notification: open the download page once the snapshot says which. */
        const val EXTRA_UPDATE = "update"

        // Set on the first auto-switch to the keep-alive tab so it never repeats.
        private const val KEY_KEEPALIVE_SHOWN = "keepalive_shown"
        private const val KEY_TAB = "tab"
        private const val KEY_INVITE = "invite"
        private const val KEY_ADBLOCK = "adblock"
        private const val KEY_DIRECT = "yt_direct"
        // latest_version whose banner was closed; a newer one shows again.
        private const val KEY_UPDATE_DISMISSED = "update_dismissed"

        private const val PRIVACY_URL = "https://bakuridzeaslan025-max.github.io/split-vpn-privacy/"

        private val PROBE_LINE = Regex("""^\S+\s+(.+?): (\d+ мс|ошибка)""")
    }

    // What the user sees; Service.title stays the short name used in the log.
    private val labels = mapOf(
        "youtube" to "YouTube / Google",
        "meta" to "WhatsApp / Instagram",
        "twitter" to "Twitter / X",
        "cloudflare" to "OpenAI / Discord",
    )

    private val icons = mapOf(
        "telegram" to listOf("TG"),
        "youtube" to listOf("YT"),
        "meta" to listOf("IG", "WA"),
        "slack" to listOf("SL"),
        "twitter" to listOf("X"),
        "cloudflare" to listOf("AI", "DC"),
    )

    private data class Vendor(val match: List<String>, val hint: String, val intents: List<ComponentName>)

    // Vendor ROMs (MIUI, EMUI, ColorOS, One UI…) kill foreground services on
    // "clear all" or in battery saver. Nothing in code prevents that; the user
    // has to whitelist the app. The keep-alive tab walks them through it.
    private val vendors = listOf(
        Vendor(
            listOf("xiaomi", "redmi", "poco"),
            "Автозапуск → включить для Split VPN. Экономия энергии → «Без ограничений». " +
                "В недавних приложениях: зажать карточку → закрепить (замок), иначе «Закрыть всё» убьёт VPN.",
            listOf(ComponentName("com.miui.securitycenter", "com.miui.permcenter.autostart.AutoStartManagementActivity")),
        ),
        Vendor(
            listOf("huawei", "honor"),
            "Запуск приложений → Split VPN → «Управлять вручную» → включить все три пункта.",
            listOf(
                ComponentName("com.huawei.systemmanager", "com.huawei.systemmanager.startupmgr.ui.StartupNormalAppListActivity"),
                ComponentName("com.huawei.systemmanager", "com.huawei.systemmanager.optimize.process.ProtectActivity"),
            ),
        ),
        Vendor(
            listOf("oppo", "realme", "oneplus"),
            "Автозапуск → включить. Батарея → Split VPN → «Разрешить фоновую активность».",
            listOf(
                ComponentName("com.coloros.safecenter", "com.coloros.safecenter.permission.startup.StartupAppListActivity"),
                ComponentName("com.oppo.safe", "com.oppo.safe.permission.startup.StartupAppListActivity"),
                ComponentName("com.oneplus.security", "com.oneplus.security.chainlaunch.view.ChainLaunchAppListActivity"),
            ),
        ),
        Vendor(
            listOf("vivo", "iqoo"),
            "Автозапуск → включить. Батарея → «Высокое энергопотребление в фоне» → разрешить.",
            listOf(ComponentName("com.vivo.permissionmanager", "com.vivo.permissionmanager.activity.BgStartUpManagerActivity")),
        ),
        Vendor(
            listOf("samsung"),
            "Батарея → Split VPN → «Без ограничений». Убрать из «Спящих приложений».",
            listOf(ComponentName("com.samsung.android.lool", "com.samsung.android.sm.ui.battery.BatteryActivity")),
        ),
    )

    private lateinit var prefs: SharedPreferences
    private var tab = "vpn"
    private var invite: String? = null
    private var inviteDialog: Dialog? = null
    private var shareDialog: Dialog? = null
    private var menuPopup: PopupWindow? = null
    // Toasts queue up: ten taps would keep the hint on screen for twenty seconds.
    private var lockedToastAt: Long? = null

    private var state = VpnState.DISCONNECTED
    private var error: String? = null
    private var log: List<String> = emptyList()
    private var connectedAt = 0L
    private var waiting: Waiting? = null
    // Null until the first snapshot.
    private var versions: Versions? = null
    private var usage: Usage? = null
    // Go's verdict on direct YouTube, see VpnClient.
    private var direct: String? = null
    private var started = false
    private var updateWhenKnown = false

    private val ticker = Handler(Looper.getMainLooper())
    private val tick = object : Runnable {
        override fun run() {
            renderClock()
            if (state == VpnState.CONNECTED && !waitingNow) ticker.postDelayed(this, 1000)
        }
    }
    private val midnight = Runnable { render() }

    private val vpn = VpnClient(this) { st, err, lines, since, w, v, u, d ->
        if (st == VpnState.CONNECTED && state != VpnState.CONNECTED) {
            // The code is one-shot on the issuer: once we are in, forget it.
            invite = null
            if (!prefs.getBoolean(KEY_KEEPALIVE_SHOWN, false)) showTab("auto")
        }
        state = st
        error = err
        log = lines
        connectedAt = since
        waiting = w
        versions = v ?: versions
        usage = u ?: usage
        direct = d
        render()
        if (updateWhenKnown && v != null) {
            updateWhenKnown = false
            if (v.available) openDownloadPage()
        }
        ticker.removeCallbacks(tick)
        if (started) ticker.post(tick)
    }

    override fun onCreate(savedInstanceState: Bundle?) {
        Crash.init(this, "ui")
        // Smoke test for the crash reporting, debug only:
        //   adb shell am start -n org.newvpn/org.duckdns.splitvpn.MainActivity --es crash ui|vpn
        if (BuildConfig.DEBUG) when (intent?.getStringExtra("crash")) {
            "ui" -> Crash.smokeTest("ui")
            // Go lives in :vpn, and so does the log file it writes to.
            "vpn", "go" -> startService(
                Intent(this, TunnelVpnService::class.java)
                    .setAction(TunnelVpnService.ACTION_CRASH)
                    .putExtra("kind", intent.getStringExtra("crash")),
            )
        }
        prefs = getSharedPreferences(Services.PREFS, MODE_PRIVATE)
        super.onCreate(savedInstanceState)
        // Not in the manifest: there it implies the screen.portrait feature and
        // Play hides the app from head units and landscape-only tablets.
        // The request outlives the activity: reset it when a foldable is opened.
        requestedOrientation = if (resources.configuration.smallestScreenWidthDp < 600) {
            ActivityInfo.SCREEN_ORIENTATION_PORTRAIT
        } else {
            ActivityInfo.SCREEN_ORIENTATION_UNSPECIFIED
        }
        setContentView(R.layout.main_simple)

        findViewById<View>(R.id.root).setOnApplyWindowInsetsListener { v, insets ->
            // WindowInsets.Type is API 30 (and Insets 29); minSdk is 26.
            val bottom: Int
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                val bars = insets.getInsets(
                    android.view.WindowInsets.Type.systemBars() or
                        android.view.WindowInsets.Type.displayCutout()
                )
                v.setPadding(bars.left, bars.top, bars.right, 0)
                bottom = bars.bottom
            } else {
                @Suppress("DEPRECATION")
                v.setPadding(insets.systemWindowInsetLeft, insets.systemWindowInsetTop, insets.systemWindowInsetRight, 0)
                @Suppress("DEPRECATION")
                bottom = insets.systemWindowInsetBottom
            }
            // The gesture area continues the tab bar, not the page background.
            findViewById<View>(R.id.navPad).layoutParams.height = bottom
            findViewById<View>(R.id.navPad).requestLayout()
            insets
        }

        findViewById<View>(R.id.menuButton).setOnClickListener { showMenu(it) }
        findViewById<View>(R.id.shareLogButton).apply {
            background = box(Color.WHITE, radius = 24f)
            setOnClickListener { shareLog() }
        }
        findViewById<View>(R.id.toggleButton).setOnClickListener {
            if (quotaOut) return@setOnClickListener
            when (state) {
                VpnState.DISCONNECTED -> startVpn()
                VpnState.ERROR -> if (offlineRetry) stopVpn() else startVpn()
                VpnState.CONNECTED -> stopVpn()
                VpnState.CONNECTING, VpnState.DISCONNECTING -> Unit
            }
        }
        findViewById<View>(R.id.errorAction).setOnClickListener {
            when {
                updateRequired -> openDownloadPage()
                error == TunnelVpnService.ERR_NEED_CODE -> showInviteDialog()
                else -> startVpn()
            }
        }
        findViewById<View>(R.id.updateAction).setOnClickListener { openDownloadPage() }
        findViewById<View>(R.id.updateClose).setOnClickListener {
            versions?.let { prefs.edit().putLong(KEY_UPDATE_DISMISSED, it.latest).apply() }
            renderUpdate()
        }

        findViewById<View>(R.id.batteryButton).setOnClickListener {
            startActivity(
                Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:$packageName"))
            )
        }
        val vendor = vendors.firstOrNull { v -> v.match.any { Build.MANUFACTURER.lowercase().contains(it) } }
        if (vendor == null) {
            findViewById<View>(R.id.vendorBlock).visibility = View.GONE
        } else {
            findViewById<TextView>(R.id.vendorHint).text = vendor.hint
            findViewById<View>(R.id.vendorButton).setOnClickListener { openVendorSettings(vendor) }
        }

        invite = savedInstanceState?.getString(KEY_INVITE)
        showTab(savedInstanceState?.getString(KEY_TAB) ?: "vpn")
        render()
        if (savedInstanceState == null) onUpdateIntent(intent)
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        onUpdateIntent(intent)
    }

    private fun onUpdateIntent(intent: Intent?) {
        if (intent?.getBooleanExtra(EXTRA_UPDATE, false) != true) return
        showTab("vpn")
        val v = versions
        if (v == null) updateWhenKnown = true else if (v.available) openDownloadPage()
    }

    override fun onSaveInstanceState(outState: Bundle) {
        super.onSaveInstanceState(outState)
        outState.putString(KEY_TAB, tab)
        outState.putString(KEY_INVITE, invite)
    }

    override fun onDestroy() {
        inviteDialog?.dismiss()
        shareDialog?.dismiss()
        menuPopup?.dismiss()
        super.onDestroy()
    }

    override fun onStart() {
        super.onStart()
        vpn.bind()
        started = true
        ticker.post(tick)
    }

    override fun onStop() {
        super.onStop()
        vpn.unbind()
        started = false
        ticker.removeCallbacks(tick)
        ticker.removeCallbacks(midnight)
    }

    override fun onResume() {
        super.onResume()
        val ignoring = getSystemService(PowerManager::class.java).isIgnoringBatteryOptimizations(packageName)
        findViewById<TextView>(R.id.batteryStatus).text =
            if (ignoring) "✓ Оптимизация батареи отключена" else "Оптимизация батареи включена"
        findViewById<View>(R.id.batteryButton).visibility = if (ignoring) View.GONE else View.VISIBLE
    }

    private fun dp(v: Float) = TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_DIP, v, resources.displayMetrics)
    private fun dp(v: Int) = dp(v.toFloat()).toInt()

    private fun box(fill: Int, stroke: Int = Color.TRANSPARENT, radius: Float = 0f) = GradientDrawable().apply {
        setColor(fill)
        setStroke(dp(1), stroke)
        cornerRadius = dp(radius)
    }

    private val editable get() = state == VpnState.DISCONNECTED || state == VpnState.ERROR && !offlineRetry
    private val busy get() = state == VpnState.CONNECTING || state == VpnState.DISCONNECTING

    private fun enabled(svc: Service) = prefs.getBoolean(Services.prefKey(svc.id), svc.defaultEnabled)

    private fun showTab(next: String) {
        tab = next
        if (next == "auto") prefs.edit().putBoolean(KEY_KEEPALIVE_SHOWN, true).apply()
        findViewById<View>(R.id.pageVpn).visibility = if (next == "vpn") View.VISIBLE else View.GONE
        findViewById<View>(R.id.pageAuto).visibility = if (next == "auto") View.VISIBLE else View.GONE
        findViewById<View>(R.id.pageLog).visibility = if (next == "log") View.VISIBLE else View.GONE
        findViewById<TextView>(R.id.titleText).text =
            mapOf("vpn" to "Split VPN", "auto" to "Автозапуск", "log" to "Журнал")[next]
        renderTabs()
    }

    private fun renderTabs() {
        val bar = findViewById<LinearLayout>(R.id.tabBar)
        bar.removeAllViews()
        val tabs = listOf(Triple("vpn", "⇅", "Соединение"), Triple("auto", "↻", "Автозапуск"), Triple("log", "☰", "Журнал"))
        for ((key, icon, label) in tabs) {
            val active = key == tab
            val ink = getColor(if (active) R.color.simple_link else R.color.simple_dim)
            val item = LinearLayout(this).apply {
                orientation = LinearLayout.VERTICAL
                gravity = Gravity.CENTER
                setOnClickListener { showTab(key) }
            }
            val iconView = TextView(this).apply {
                text = icon
                textSize = 17f
                gravity = Gravity.CENTER
                includeFontPadding = false
                setTextColor(ink)
            }
            val labelView = TextView(this).apply {
                text = label
                includeFontPadding = false
                setTextColor(ink)
            }
            iconView.background = if (active) box(0xFFE8F0FE.toInt(), radius = 13f) else null
            labelView.textSize = 12f
            labelView.typeface = android.graphics.Typeface.create("sans-serif-medium", android.graphics.Typeface.NORMAL)
            item.addView(iconView, LinearLayout.LayoutParams(dp(56), dp(26)))
            item.addView(labelView, LinearLayout.LayoutParams(-2, -2).apply { topMargin = dp(5) })
            bar.addView(item, LinearLayout.LayoutParams(0, -1, 1f))
        }
    }

    private fun render() {
        renderServices()
        renderStatus()
        renderClock()
        renderError()
        renderUpdate()
        renderUsage()
        // The service pushes nothing on a used-up day, so the new one needs its own redraw.
        ticker.removeCallbacks(midnight)
        if (started && quotaOut) ticker.postDelayed(midnight, untilMidnight())
        findViewById<TextView>(R.id.logText).text =
            if (log.isEmpty()) "Журнал начат" else log.reversed().joinToString("\n")
    }

    private fun renderServices() {
        val pings = mutableMapOf<String, String>()
        for (line in log) {
            val m = PROBE_LINE.find(line) ?: continue
            pings[m.groupValues[1]] = m.groupValues[2]
        }
        val list = findViewById<LinearLayout>(R.id.serviceList)
        for ((i, svc) in Services.ALL.withIndex()) {
            val en = enabled(svc)
            val ping = pings[svc.title].takeIf { state == VpnState.CONNECTED && !waitingNow && en }
            val row = list.getChildAt(i) ?: newServiceRow(list, svc)
            val name = row.findViewById<TextView>(R.id.name)
            val note = row.findViewById<TextView>(R.id.note)
            name.text = labels[svc.id] ?: svc.title
            // Also while the server is down: direct YouTube does not need it.
            val status = if (svc.id == "youtube" && en && state == VpnState.CONNECTED) when (direct) {
                "works" -> "YouTube напрямую"
                "testing" -> "подбираю…"
                else -> null
            } else null
            note.text = status ?: ping
            note.setTextColor(getColor(if (status != null && direct == "works") R.color.simple_blue else R.color.simple_dim))
            renderSwitch(row.findViewById(R.id.seg), row.findViewById(R.id.knob), en)
        }
    }

    private fun renderSwitch(seg: View, knob: View, on: Boolean) {
        val track = if (!on) 0xFFDADCE0.toInt() else if (editable) getColor(R.color.simple_blue) else 0xFFBFCBD6.toInt()
        seg.background = box(track, radius = 16f)
        knob.layoutParams = (knob.layoutParams as FrameLayout.LayoutParams).apply {
            gravity = if (on) Gravity.END else Gravity.START
        }
    }

    private fun newServiceRow(list: LinearLayout, svc: Service): View {
        val row = layoutInflater.inflate(R.layout.row_service_simple, list, false)
        val badges = row.findViewById<LinearLayout>(R.id.icons)
        for ((i, ic) in (icons[svc.id] ?: emptyList()).withIndex()) {
            val badge = TextView(this).apply {
                text = ic
                textSize = 10f
                gravity = Gravity.CENTER
                setTextColor(0xFF4A4E52.toInt())
                setBackgroundResource(R.drawable.simple_icon_bg)
            }
            badges.addView(badge, i, LinearLayout.LayoutParams(dp(22), dp(22)).apply { marginEnd = dp(6) })
        }
        row.setOnClickListener {
            if (!editable) {
                lockedToast()
                return@setOnClickListener
            }
            prefs.edit().putBoolean(Services.prefKey(svc.id), !enabled(svc)).apply()
            renderServices()
            renderClock()
        }
        list.addView(row)
        return row
    }

    private fun lockedToast() {
        val now = SystemClock.elapsedRealtime()
        if (lockedToastAt.let { it == null || now - it > 2000 }) {
            lockedToastAt = now
            Toast.makeText(this, "Сначала выключите VPN", Toast.LENGTH_SHORT).show()
        }
    }

    private fun renderStatus() {
        val hint = findViewById<TextView>(R.id.hintText)
        val button = findViewById<View>(R.id.toggleButton)
        val dot = findViewById<View>(R.id.statusDot)
        val on = state == VpnState.CONNECTED
        hint.text = if (editable) "Выберите, что пойдёт через VPN" else "Чтобы изменить список, выключите VPN"
        hint.setTextColor(getColor(if (editable) R.color.simple_blue else R.color.simple_dim))
        findViewById<View>(R.id.chip).background = box(
            if (waitingNow) AMBER_BG else if (on) 0xFFE3F2E5.toInt() else 0xFFE4E6E8.toInt(), radius = 16f
        )
        findViewById<TextView>(R.id.statusText).setTextColor(
            if (waitingNow) AMBER_INK else if (on) 0xFF14682C.toInt() else 0xFF3C4043.toInt()
        )
        val green = 0xFF1E8E3E.toInt()
        val grey = 0xFF9AA0A6.toInt()
        // Not a failure but nothing to press until midnight: greyed out like busy.
        val inert = busy || quotaOut
        dot.background = box(if (waitingNow) AMBER else if (on) green else if (busy) grey else 0xFF5F6368.toInt(), radius = 4f)
        findViewById<View>(R.id.buttonDot).background =
            box(if (waitingNow) AMBER else if (on) green else if (inert) grey else Color.WHITE, radius = 5f)
        findViewById<TextView>(R.id.buttonLabel).apply {
            text = if (quotaOut) "Лимит на сегодня исчерпан" else when (state) {
                VpnState.CONNECTING -> "Подключение…"
                VpnState.DISCONNECTING -> "Отключение…"
                VpnState.CONNECTED -> "Выключить"
                VpnState.ERROR -> if (offlineRetry) "Выключить" else "Подключить"
                VpnState.DISCONNECTED -> "Подключить"
            }
            setTextColor(if (on) getColor(R.color.simple_ink) else if (inert) getColor(R.color.simple_dim) else Color.WHITE)
        }
        button.isEnabled = !inert
        button.background = when {
            on -> box(Color.WHITE, 0xFFDADCE0.toInt(), 30f)
            inert -> box(0xFFE8EAED.toInt(), radius = 30f)
            else -> box(getColor(R.color.simple_blue), radius = 30f)
        }
    }

    // The service waits for a network to renew the credential: the same calm
    // banner as Waiting, and the toggle stops that wait.
    private val offlineRetry get() = state == VpnState.ERROR && error == TunnelVpnService.ERR_OFFLINE
    private val waitingNow get() = state == VpnState.CONNECTED && waiting != null || offlineRetry

    private fun renderClock() {
        val clock = if (state == VpnState.CONNECTED && !waitingNow && connectedAt > 0) {
            val s = (SystemClock.elapsedRealtime() - connectedAt) / 1000
            "%02d:%02d:%02d".format(s / 3600, s / 60 % 60, s % 60)
        } else null
        findViewById<TextView>(R.id.statusText).text = clock ?: when {
            waitingNow && (waiting == Waiting.NO_NETWORK || offlineRetry) -> "Нет сети"
            waitingNow -> "Сервер недоступен"
            busy -> "Подождите"
            else -> "Выключен"
        }
    }

    // The service reports Go/Java error strings; the raw text stays in the file log.
    private fun errorText(): String {
        val e = error ?: ""
        return when {
            e.any { it in 'а'..'я' } -> e
            "unreachable" in e || "relay reply" in e -> "Сервер недоступен. Проверьте интернет и повторите"
            "rejected" in e -> "Сервер отклонил доступ"
            else -> "Не удалось подключиться"
        }
    }

    private fun renderError() {
        val view = findViewById<View>(R.id.errorBox)
        val text = findViewById<TextView>(R.id.errorText)
        val action = findViewById<TextView>(R.id.errorAction)
        if (waitingNow) {
            // Not an error: nothing to press, the tunnel is on and comes back by itself.
            view.visibility = View.VISIBLE
            action.visibility = View.GONE
            val msg = if (offlineRetry) "Нет интернета. VPN подключится сам, когда сеть появится — ничего делать не нужно."
            else if (waiting == Waiting.NO_NETWORK) "Нет интернета. VPN включён и заработает сам, когда сеть появится — ничего делать не нужно."
            else "Сервер пока не отвечает — возможно, его блокирует оператор. VPN включён и подключится сам, ничего делать не нужно."
            text.text = msg
            text.setTextColor(AMBER_INK)
            view.background = box(AMBER_BG, radius = 16f)
            return
        }
        action.visibility = View.VISIBLE
        // A used-up day is told by the toggle and the usage line: a normal state, not a failure.
        // A stale one is over. An update still shows: the one thing to do right now.
        if (!updateRequired && (state != VpnState.ERROR || quotaError)) {
            view.visibility = View.GONE
            return
        }
        view.visibility = View.VISIBLE
        val needCode = error == TunnelVpnService.ERR_NEED_CODE
        text.text = when {
            needCode -> "Не удалось проверить устройство через Google Play"
            updateRequired -> TunnelVpnService.ERR_UPDATE_REQUIRED
            else -> errorText()
        }
        action.text = when {
            needCode -> "Ввести код"
            updateRequired -> "Обновить"
            else -> "Повторить"
        }
        val ink = if (needCode) Color.WHITE else 0xFF8C1D18.toInt()
        text.setTextColor(ink)
        action.setTextColor(if (needCode) 0xFFA8C7FA.toInt() else ink)
        view.background = box(if (needCode) 0xFF322F2F.toInt() else 0xFFFCE8E6.toInt(), radius = 16f)
    }

    // Also with the app closed when it happened: the service may be gone, the count is not.
    // Past midnight the snapshot is yesterday's: the service would start again.
    private val quotaOut get() = usage?.day == Quota.today() &&
        (quotaError || state == VpnState.DISCONNECTED && usage?.exceeded == true)
    private val quotaError get() = state == VpnState.ERROR && error == TunnelVpnService.ERR_QUOTA

    private fun untilMidnight(): Long {
        val now = java.time.ZonedDateTime.now()
        return java.time.Duration.between(now, now.toLocalDate().plusDays(1).atStartOfDay(now.zone)).toMillis() + 1000
    }

    private fun renderUsage() {
        val u = usage?.takeIf { it.limit > 0 }
        findViewById<TextView>(R.id.usageText).apply {
            visibility = if (u == null) View.GONE else View.VISIBLE
            val used = u?.takeIf { it.day == Quota.today() }?.used ?: 0
            if (u != null) text = "Сегодня через VPN: ${Quota.format(used)} из ${Quota.format(u.limit)}" +
                if (quotaOut) ", снова после 00:00" else ""
        }
    }

    // Also before any try to connect: the service would only refuse.
    private val updateRequired get() = state == VpnState.ERROR && error == TunnelVpnService.ERR_UPDATE_REQUIRED ||
        state == VpnState.DISCONNECTED && versions?.required == true

    // The calm banner of Waiting: news, not an error. A required update is
    // the error box's business instead.
    private fun renderUpdate() {
        val view = findViewById<View>(R.id.updateBox)
        val v = versions
        val show = v != null && v.available && !updateRequired &&
            v.latest > prefs.getLong(KEY_UPDATE_DISMISSED, 0)
        view.visibility = if (show) View.VISIBLE else View.GONE
        if (v == null || !show) return
        view.background = box(AMBER_BG, radius = 16f)
        findViewById<TextView>(R.id.updateText).apply {
            text = "Доступна новая версия"
            setTextColor(AMBER_INK)
        }
        findViewById<TextView>(R.id.updateAction).setTextColor(AMBER_INK)
        findViewById<TextView>(R.id.updateClose).setTextColor(AMBER_INK)
    }

    private fun openDownloadPage() = openUrl((versions ?: Versions()).page)

    private fun openUrl(url: String) {
        runCatching { startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(url))) }
    }

    private fun showInviteDialog() {
        val dialog = Dialog(this)
        dialog.setContentView(R.layout.dialog_invite_simple)
        dialog.window?.apply {
            setBackgroundDrawable(ColorDrawable(Color.TRANSPARENT))
            setLayout(resources.displayMetrics.widthPixels - dp(48), ViewGroup.LayoutParams.WRAP_CONTENT)
            setSoftInputMode(
                WindowManager.LayoutParams.SOFT_INPUT_STATE_VISIBLE or WindowManager.LayoutParams.SOFT_INPUT_ADJUST_RESIZE
            )
        }
        val input = dialog.findViewById<EditText>(R.id.inviteInput)
        dialog.findViewById<View>(R.id.cancel).setOnClickListener { dialog.dismiss() }
        dialog.findViewById<View>(R.id.apply).setOnClickListener {
            val code = input.text.toString().trim()
            if (code.isEmpty()) return@setOnClickListener
            invite = code
            dialog.dismiss()
            if (editable) startVpn()
        }
        inviteDialog = dialog
        dialog.show()
        input.requestFocus()
    }

    private fun showShareDialog() {
        val url = (versions ?: Versions()).page
        val dialog = Dialog(this)
        dialog.setContentView(R.layout.dialog_share_simple)
        dialog.window?.apply {
            setBackgroundDrawable(ColorDrawable(Color.TRANSPARENT))
            setLayout(resources.displayMetrics.widthPixels - dp(48), ViewGroup.LayoutParams.WRAP_CONTENT)
        }
        dialog.findViewById<ImageView>(R.id.qr).setImageBitmap(qr(url, dp(220)))
        dialog.findViewById<View>(R.id.close).setOnClickListener { dialog.dismiss() }
        dialog.findViewById<View>(R.id.send).setOnClickListener {
            val send = Intent(Intent.ACTION_SEND).apply {
                type = "text/plain"
                putExtra(Intent.EXTRA_TEXT, "Split VPN — скачать: $url")
            }
            startActivity(Intent.createChooser(send, "Отправить ссылку"))
        }
        shareDialog = dialog
        dialog.show()
    }

    private fun qr(text: String, size: Int): Bitmap {
        val m = QRCodeWriter().encode(text, BarcodeFormat.QR_CODE, size, size, mapOf(EncodeHintType.MARGIN to 1))
        val px = IntArray(m.width * m.height) { if (m[it % m.width, it / m.width]) Color.BLACK else Color.WHITE }
        return Bitmap.createBitmap(px, m.width, m.height, Bitmap.Config.ARGB_8888)
    }

    private fun showMenu(anchor: View) {
        val ink = getColor(R.color.simple_ink)
        val dim = 0xFF80868B.toInt()
        val side = dp(20)
        val menu = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            background = box(Color.WHITE, radius = 16f)
            setPadding(0, dp(12), 0, dp(6))
            elevation = dp(8f)
        }
        val popup = PopupWindow(menu, dp(264), ViewGroup.LayoutParams.WRAP_CONTENT, true)
        menu.addView(switchItem("Блокировать рекламу", null, KEY_ADBLOCK, false, ink, dim, side))
        menu.addView(switchItem("YouTube напрямую", "Напрямую, с обходом замедления у провайдера. Не тратит лимит", KEY_DIRECT, true, ink, dim, side))
        menu.addView(notificationItem(ink, dim, side, popup))
        menu.addView(TextView(this).apply {
            text = "Поделиться приложением"
            textSize = 14f
            gravity = Gravity.CENTER_VERTICAL
            setTextColor(ink)
            setPadding(side, 0, side, 0)
            setOnClickListener {
                popup.dismiss()
                showShareDialog()
            }
        }, LinearLayout.LayoutParams(-1, dp(46)))
        menu.addView(TextView(this).apply {
            text = "Политика конфиденциальности ↗"
            textSize = 14f
            gravity = Gravity.CENTER_VERTICAL
            setTextColor(ink)
            setPadding(side, 0, side, 0)
            setOnClickListener {
                popup.dismiss()
                startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(PRIVACY_URL)))
            }
        }, LinearLayout.LayoutParams(-1, dp(46)))
        menu.addView(TextView(this).apply {
            text = "Версия ${BuildConfig.VERSION_NAME} (${BuildConfig.VERSION_CODE})"
            textSize = 10f
            setTextColor(dim)
            setPadding(side, dp(8), side, dp(6))
        })
        menuPopup = popup
        popup.elevation = dp(8f)
        popup.showAsDropDown(anchor, -dp(264 - 44 + 6), 0)
    }

    // Go reads these at start only, so like the services they are locked
    // while the VPN is on: the grey switch says so.
    private fun switchItem(title: String, caption: String?, key: String, default: Boolean, ink: Int, dim: Int, side: Int): View {
        val knob = View(this).apply {
            setBackgroundResource(R.drawable.simple_knob)
            elevation = dp(2f)
        }
        val seg = FrameLayout(this).apply {
            setPadding(dp(3), dp(3), dp(3), dp(3))
            addView(knob, FrameLayout.LayoutParams(dp(26), dp(26)))
        }
        renderSwitch(seg, knob, prefs.getBoolean(key, default))
        val texts = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            addView(TextView(this@MainActivity).apply {
                text = title
                textSize = 14f
                setTextColor(if (editable) ink else dim)
            })
            // 10sp: at 11 a line holds a word or two less on 264dp.
            if (caption != null) addView(TextView(this@MainActivity).apply {
                text = caption
                textSize = 10f
                setTextColor(dim)
            })
        }
        return LinearLayout(this).apply {
            gravity = Gravity.CENTER_VERTICAL
            setPadding(side, dp(6), side, dp(6))
            minimumHeight = dp(46)
            addView(texts, LinearLayout.LayoutParams(0, -2, 1f).apply { marginEnd = dp(12) })
            addView(seg, LinearLayout.LayoutParams(dp(52), dp(32)))
            accessibilityDelegate = object : View.AccessibilityDelegate() {
                override fun onInitializeAccessibilityNodeInfo(host: View, info: AccessibilityNodeInfo) {
                    super.onInitializeAccessibilityNodeInfo(host, info)
                    info.className = Switch::class.java.name
                    info.isCheckable = true
                    info.isChecked = prefs.getBoolean(key, default)
                }
            }
            setOnClickListener {
                if (!editable) {
                    lockedToast()
                    return@setOnClickListener
                }
                val on = !prefs.getBoolean(key, default)
                prefs.edit().putBoolean(key, on).apply()
                renderSwitch(seg, knob, on)
            }
        }
    }

    // The service stays in the foreground either way; only the system can
    // hide its notification (the channel, or on 13+ the app's permission).
    private fun notificationItem(ink: Int, dim: Int, side: Int, popup: PopupWindow): View {
        val nm = getSystemService(NotificationManager::class.java)
        TunnelVpnService.createChannel(this)
        val appOn = nm.areNotificationsEnabled()
        val on = appOn && nm.getNotificationChannel(TunnelVpnService.CHANNEL_ID)?.importance != NotificationManager.IMPORTANCE_NONE
        return LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            gravity = Gravity.CENTER_VERTICAL
            setPadding(side, dp(6), side, dp(6))
            minimumHeight = dp(46)
            addView(TextView(this@MainActivity).apply {
                text = "Уведомление в шторке ↗"
                textSize = 14f
                setTextColor(ink)
            })
            addView(TextView(this@MainActivity).apply {
                text = if (on) "Включено · настроить в системе" else "Выключено · настроить в системе"
                textSize = 10f
                setTextColor(dim)
            })
            setOnClickListener {
                popup.dismiss()
                // With the app's notifications off the channel page is inert.
                // Trimmed ROMs may lack either page: fall through like openVendorSettings.
                val channel = Intent(Settings.ACTION_CHANNEL_NOTIFICATION_SETTINGS)
                    .putExtra(Settings.EXTRA_APP_PACKAGE, packageName)
                    .putExtra(Settings.EXTRA_CHANNEL_ID, TunnelVpnService.CHANNEL_ID)
                val app = Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS).putExtra(Settings.EXTRA_APP_PACKAGE, packageName)
                val details = Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:$packageName"))
                for (i in if (appOn) listOf(channel, app, details) else listOf(app, details)) {
                    runCatching { startActivity(i) }.onSuccess { return@setOnClickListener }
                }
            }
        }
    }

    // Vendor activities are undocumented and move between ROM versions;
    // fall back to the app's own settings page. Not resolveActivity(): without
    // <queries> it is always null on Android 11+, startActivity is not.
    private fun openVendorSettings(v: Vendor) {
        for (cn in v.intents) {
            runCatching { startActivity(Intent().setComponent(cn)) }.onSuccess { return }
        }
        startActivity(Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:$packageName")))
    }

    private fun shareLog() {
        val header = "Split VPN ${BuildConfig.VERSION_NAME} (${BuildConfig.VERSION_CODE}) · " +
            "${Build.MANUFACTURER} ${Build.MODEL} · Android ${Build.VERSION.RELEASE} (API ${Build.VERSION.SDK_INT})"
        val file = try {
            AppLog.export(this, header)
        } catch (_: IOException) {
            Toast.makeText(this, "Не удалось сохранить лог", Toast.LENGTH_SHORT).show()
            return
        }
        if (file == null) {
            Toast.makeText(this, "Лог пока пустой", Toast.LENGTH_SHORT).show()
            return
        }
        val uri = FileProvider.getUriForFile(this, "$packageName.logs", file)
        val send = Intent(Intent.ACTION_SEND).apply {
            type = "text/plain"
            putExtra(Intent.EXTRA_STREAM, uri)
            putExtra(Intent.EXTRA_SUBJECT, "Лог Split VPN")
            // createChooser only forwards the URI grant to the chooser (which
            // previews the file) when it is expressed as ClipData.
            clipData = ClipData.newRawUri("log", uri)
            addFlags(Intent.FLAG_GRANT_READ_URI_PERMISSION)
        }
        startActivity(Intent.createChooser(send, "Поделиться логом"))
    }

    private fun startVpn() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU &&
            checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED
        ) {
            requestPermissions(arrayOf(Manifest.permission.POST_NOTIFICATIONS), NOTIFICATION_REQUEST_CODE)
            return
        }
        prepareAndLaunch()
    }

    private fun prepareAndLaunch() {
        val intent = VpnService.prepare(this)
        if (intent != null) {
            startActivityForResult(intent, VPN_REQUEST_CODE)
        } else {
            launchService()
        }
    }

    private fun stopVpn() {
        val intent = Intent(this, TunnelVpnService::class.java).apply {
            action = TunnelVpnService.ACTION_STOP
        }
        startService(intent)
    }

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<String>, grantResults: IntArray) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        if (requestCode == NOTIFICATION_REQUEST_CODE) prepareAndLaunch()
    }

    override fun onActivityResult(requestCode: Int, resultCode: Int, data: Intent?) {
        super.onActivityResult(requestCode, resultCode, data)
        if (requestCode == VPN_REQUEST_CODE && resultCode == RESULT_OK) {
            launchService()
        }
    }

    private fun launchService() {
        val enabled = Services.ALL.filter { enabled(it) }.map { it.id }
        val intent = Intent(this, TunnelVpnService::class.java).apply {
            action = TunnelVpnService.ACTION_START
            putStringArrayListExtra(TunnelVpnService.EXTRA_SERVICES, ArrayList(enabled))
            putExtra(TunnelVpnService.EXTRA_ADBLOCK, prefs.getBoolean(KEY_ADBLOCK, false))
            putExtra(TunnelVpnService.EXTRA_DIRECT, prefs.getBoolean(KEY_DIRECT, true))
            invite?.let { putExtra(TunnelVpnService.EXTRA_INVITE, it) }
        }
        startForegroundService(intent)
    }
}
