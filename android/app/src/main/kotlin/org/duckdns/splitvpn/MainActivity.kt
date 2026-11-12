package org.duckdns.splitvpn

import android.Manifest
import android.app.Activity
import android.app.Dialog
import android.app.NotificationManager
import android.app.UiModeManager
import android.content.ClipData
import android.content.ComponentName
import android.content.Context
import android.content.Intent
import android.content.SharedPreferences
import android.content.pm.ActivityInfo
import android.content.pm.PackageManager
import android.content.res.ColorStateList
import android.content.res.Configuration
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
        private const val VPN_REQUEST_CODE = 1
        private const val NOTIFICATION_REQUEST_CODE = 2
        private const val INSTALL_PERMISSION_CODE = 3

        /** From the VPN notification: update once the snapshot says to what. */
        const val EXTRA_UPDATE = "update"

        // Swapped in unit tests.
        internal var update: (android.content.Context, Versions, (Int) -> Unit) -> Updater.Result = Updater::update
        internal var background: (Runnable) -> Unit = { Thread(it, "Update").start() }

        // The download outlives a recreated activity (theme, rotation, the
        // system's night mode): its state is the process's, shown by the live one.
        internal var updating = false
        // Percent of the download, -1 while the size is unknown.
        internal var progress = -1
        private var shown: java.lang.ref.WeakReference<MainActivity>? = null
        private val live get() = shown?.get()

        internal fun resetUpdateState() {
            updating = false
            progress = -1
            shown = null
        }

        private fun onMain(r: () -> Unit) {
            if (Looper.myLooper() == Looper.getMainLooper()) r() else Handler(Looper.getMainLooper()).post(r)
        }

        // Set on the first auto-switch to the keep-alive tab so it never repeats.
        private const val KEY_KEEPALIVE_SHOWN = "keepalive_shown"
        private const val KEY_TAB = "tab"
        private const val KEY_INVITE = "invite"
        private const val KEY_ADBLOCK = "adblock"
        private const val KEY_DIRECT = "yt_direct"
        // "light" or "dark"; absent follows the system. Not "theme": 0.5 kept
        // the long-gone terminal skins there, "light" among them.
        private const val KEY_THEME = "theme_mode"
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

    // battery: a MIUI power page, which reads the package from the battery request's
    // data and extras. Other pages get a bare component: Android 13+ drops an explicit
    // intent that matches none of the target's filters.
    private class Page(val component: ComponentName, val battery: Boolean = false)

    // pages: the first that opens wins; none listed means the app's own system page.
    private class Link(val label: String, val pages: List<Page> = emptyList())
    private class Step(val text: String, val link: Link? = null)
    private class Vendor(val match: List<String>, val steps: List<Step>)

    // Vendor ROMs (MIUI, EMUI, ColorOS, One UI…) kill foreground services on
    // "clear all" or in battery saver. Nothing in code prevents that; the user
    // has to whitelist the app. The settings tab walks them through it.
    private val vendors = listOf(
        Vendor(
            listOf("xiaomi", "redmi", "poco"),
            listOf(
                Step(
                    "Автозапуск → включить для Split VPN",
                    Link("Открыть автозапуск", listOf(Page(ComponentName("com.miui.securitycenter", "com.miui.permcenter.autostart.AutoStartManagementActivity")))),
                ),
                // MIUI's own power keeper, apart from Android's whitelist the battery card checks.
                Step(
                    "Экономия энергии → «Без ограничений»",
                    Link(
                        "Открыть экономию энергии",
                        listOf(
                            // HyperOS: where it routes ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS.
                            Page(ComponentName("com.miui.securitycenter", "com.miui.powercenter.legacypowerrank.PowerDetailActivity"), battery = true),
                            Page(ComponentName("com.miui.powerkeeper", "com.miui.powerkeeper.ui.HiddenAppsConfigActivity"), battery = true),
                        ),
                    ),
                ),
                Step("В недавних приложениях зажмите карточку Split VPN и закрепите замком — иначе «Закрыть всё» выключит VPN"),
            ),
        ),
        Vendor(
            listOf("huawei", "honor"),
            listOf(
                Step(
                    "Запуск приложений → Split VPN → «Управлять вручную»",
                    Link(
                        "Открыть запуск приложений",
                        listOf(
                            // MagicOS: Huawei's page under Honor's own package.
                            Page(ComponentName("com.hihonor.systemmanager", "com.hihonor.systemmanager.startupmgr.ui.StartupNormalAppListActivity")),
                            Page(ComponentName("com.huawei.systemmanager", "com.huawei.systemmanager.startupmgr.ui.StartupNormalAppListActivity")),
                            Page(ComponentName("com.huawei.systemmanager", "com.huawei.systemmanager.optimize.process.ProtectActivity")),
                        ),
                    ),
                ),
                Step("Включите все три переключателя: автозапуск, косвенный запуск, работа в фоне"),
            ),
        ),
        Vendor(
            listOf("oppo", "realme", "oneplus"),
            listOf(
                Step(
                    "Автозапуск → включить для Split VPN",
                    Link(
                        "Открыть автозапуск",
                        listOf(
                            Page(ComponentName("com.coloros.safecenter", "com.coloros.safecenter.permission.startup.StartupAppListActivity")),
                            Page(ComponentName("com.coloros.safecenter", "com.coloros.safecenter.startupapp.StartupAppListActivity")),
                            Page(ComponentName("com.oppo.safe", "com.oppo.safe.permission.startup.StartupAppListActivity")),
                            Page(ComponentName("com.oneplus.security", "com.oneplus.security.chainlaunch.view.ChainLaunchAppListActivity")),
                        ),
                    ),
                ),
                Step("Батарея → Split VPN → «Разрешить работу в фоне»"),
            ),
        ),
        Vendor(
            listOf("vivo", "iqoo"),
            listOf(
                Step(
                    "Автозапуск → включить для Split VPN",
                    Link(
                        "Открыть автозапуск",
                        listOf(
                            Page(ComponentName("com.vivo.permissionmanager", "com.vivo.permissionmanager.activity.BgStartUpManagerActivity")),
                            // iQOO and older Funtouch.
                            Page(ComponentName("com.iqoo.secure", "com.iqoo.secure.ui.phoneoptimize.BgStartUpManager")),
                            Page(ComponentName("com.iqoo.secure", "com.iqoo.secure.ui.phoneoptimize.AddWhiteListActivity")),
                        ),
                    ),
                ),
                Step("Батарея → Split VPN → «Разрешить высокое энергопотребление в фоне»"),
            ),
        ),
        // One UI keeps the app's battery mode on its app info page.
        Vendor(
            listOf("samsung"),
            listOf(
                Step("Батарея → «Без ограничений»", Link("Открыть батарею приложения")),
                Step("Там же отключите «Переводить в спящий режим»"),
            ),
        ),
    )

    // People know the phone by the brand on it (Redmi, POCO); MANUFACTURER says Xiaomi.
    private val brandNames = mapOf(
        "xiaomi" to "Xiaomi", "redmi" to "Redmi", "poco" to "POCO", "huawei" to "Huawei", "honor" to "Honor",
        "oppo" to "OPPO", "realme" to "realme", "oneplus" to "OnePlus", "vivo" to "vivo", "iqoo" to "iQOO", "samsung" to "Samsung",
    )

    private fun brandName(): String {
        val b = Build.BRAND?.takeIf { it.isNotBlank() } ?: Build.MANUFACTURER.orEmpty()
        return brandNames[b.lowercase()] ?: b.replaceFirstChar { it.uppercase() }
    }

    private lateinit var prefs: SharedPreferences
    private var tab = "vpn"
    private var invite: String? = null
    private var inviteDialog: Dialog? = null
    private var shareDialog: Dialog? = null
    private var menuPopup: PopupWindow? = null
    // Null until the first onResume asks the system.
    private var batteryOptimized: Boolean? = null

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
    private var installDialog: Dialog? = null
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
            if (!prefs.getBoolean(KEY_KEEPALIVE_SHOWN, false)) showTab("settings")
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
            if (v.available) startUpdate()
        }
        ticker.removeCallbacks(tick)
        if (started) ticker.post(tick)
    }

    // No AppCompat for setDefaultNightMode: the activity overrides its own uiMode.
    override fun attachBaseContext(base: Context) {
        super.attachBaseContext(base)
        val night = when (base.getSharedPreferences(Services.PREFS, MODE_PRIVATE).getString(KEY_THEME, null)) {
            "light" -> Configuration.UI_MODE_NIGHT_NO
            "dark" -> Configuration.UI_MODE_NIGHT_YES
            else -> return
        }
        applyOverrideConfiguration(Configuration().apply { uiMode = night })
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
        shown = java.lang.ref.WeakReference(this)
        Updater.onChange = { live?.renderBanner() }

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

        findViewById<View>(R.id.shareButton).setOnClickListener { showShareDialog() }
        findViewById<View>(R.id.shareLogButton).apply {
            background = box(getColor(R.color.surface_lowest), radius = 24f)
            setOnClickListener { shareLog() }
        }
        findViewById<View>(R.id.toggleButton).setOnClickListener {
            if (quotaOut || updateRequired) return@setOnClickListener
            when (state) {
                VpnState.DISCONNECTED -> startVpn()
                VpnState.ERROR -> if (offlineRetry) stopVpn() else startVpn()
                VpnState.CONNECTED -> stopVpn()
                VpnState.CONNECTING, VpnState.DISCONNECTING -> Unit
            }
        }
        findViewById<View>(R.id.bannerAction).setOnClickListener { banner()?.onAction?.invoke() }
        findViewById<View>(R.id.bannerClose).setOnClickListener {
            versions?.let { prefs.edit().putLong(KEY_UPDATE_DISMISSED, it.latest).apply() }
            renderBanner()
        }

        initSettings()

        invite = savedInstanceState?.getString(KEY_INVITE)
        showTab(savedInstanceState?.getString(KEY_TAB) ?: "vpn")
        render()
        if (savedInstanceState == null) {
            onUpdateIntent(intent)
            // Cleared app data wipes the pref but maybe not the system's copy.
            syncNightMode()
        }
    }

    private fun initSettings() {
        findViewById<View>(R.id.batteryWarn).background = box(getColor(R.color.warning_container), radius = 22f)
        findViewById<View>(R.id.batteryOk).background = box(getColor(R.color.surface_lowest), radius = 22f)
        findViewById<View>(R.id.batteryButton).apply {
            background = box(getColor(R.color.on_warning_container), radius = 18f)
            setOnClickListener { requestBatteryWhitelist() }
        }
        val vendor = vendors.firstOrNull { v -> v.match.any { Build.MANUFACTURER.lowercase().contains(it) } }
        val vendorBlock = findViewById<View>(R.id.vendorBlock)
        if (vendor == null) {
            vendorBlock.visibility = View.GONE
        } else {
            vendorBlock.background = box(getColor(R.color.surface_lowest), radius = 22f)
            findViewById<TextView>(R.id.vendorTitle).text = "Настройки ${brandName()}"
            val steps = findViewById<LinearLayout>(R.id.vendorSteps)
            for ((i, step) in vendor.steps.withIndex()) {
                steps.addView(stepRow(i + 1, step), LinearLayout.LayoutParams(-1, -2).apply { if (i > 0) topMargin = dp(14) })
            }
        }

        findViewById<View>(R.id.mainCard).apply {
            background = box(getColor(R.color.surface_lowest), radius = 22f)
            clipToOutline = true
        }
        val seg = findViewById<View>(R.id.adBlockSeg)
        val knob = findViewById<View>(R.id.adBlockKnob)
        renderSwitch(seg, knob, prefs.getBoolean(KEY_ADBLOCK, false))
        findViewById<View>(R.id.adBlockRow).apply {
            accessibilityDelegate = object : View.AccessibilityDelegate() {
                override fun onInitializeAccessibilityNodeInfo(host: View, info: AccessibilityNodeInfo) {
                    super.onInitializeAccessibilityNodeInfo(host, info)
                    info.className = Switch::class.java.name
                    info.isCheckable = true
                    info.isChecked = prefs.getBoolean(KEY_ADBLOCK, false)
                }
            }
            // Go checks it on every DNS query: no need to wait for the next start.
            setOnClickListener {
                val on = !prefs.getBoolean(KEY_ADBLOCK, false)
                prefs.edit().putBoolean(KEY_ADBLOCK, on).apply()
                renderSwitch(seg, knob, on)
                vpn.setAdBlock(on)
            }
        }
        findViewById<View>(R.id.notificationRow).setOnClickListener { openNotificationSettings() }
        val theme = prefs.getString(KEY_THEME, null)
        val themes = listOf(null to "Как в системе", "light" to "Светлая", "dark" to "Тёмная")
        val themeName = (themes.firstOrNull { it.first == theme } ?: themes[0]).second
        findViewById<TextView>(R.id.themeValue).text = themeName
        findViewById<View>(R.id.themeRow).contentDescription = "Тема: $themeName"
        findViewById<View>(R.id.themeRow).setOnClickListener { row ->
            showMenu(row, themes.map { (mode, title) -> MenuItem(title, null, mode == theme) { applyTheme(mode) } }, Gravity.END)
        }
        findViewById<View>(R.id.privacyLink).setOnClickListener { openUrl(PRIVACY_URL) }
        findViewById<TextView>(R.id.versionText).text = "Split VPN · версия ${BuildConfig.VERSION_NAME} (${BuildConfig.VERSION_CODE})"
    }

    private fun applyTheme(mode: String?) {
        if (mode == prefs.getString(KEY_THEME, null)) return
        prefs.edit().putString(KEY_THEME, mode).apply()
        syncNightMode()
        recreate()
    }

    // Also the splash screen on 12+, which starts before attachBaseContext.
    private fun syncNightMode() {
        if (Build.VERSION.SDK_INT < Build.VERSION_CODES.S) return
        getSystemService(UiModeManager::class.java)?.setApplicationNightMode(
            when (prefs.getString(KEY_THEME, null)) {
                "light" -> UiModeManager.MODE_NIGHT_NO
                "dark" -> UiModeManager.MODE_NIGHT_YES
                else -> UiModeManager.MODE_NIGHT_AUTO
            }
        )
    }

    private fun stepRow(n: Int, step: Step) = LinearLayout(this).apply {
        val ink = getColor(R.color.on_surface)
        addView(TextView(this@MainActivity).apply {
            text = "$n."
            textSize = 13f
            setTextColor(ink)
            setLineSpacing(0f, 1.45f)
            minWidth = dp(18)
        })
        val body = LinearLayout(this@MainActivity).apply { orientation = LinearLayout.VERTICAL }
        body.addView(TextView(this@MainActivity).apply {
            text = step.text
            textSize = 13f
            setTextColor(ink)
            setLineSpacing(0f, 1.45f)
        })
        // A page this phone lacks: no link at all rather than one that opens the wrong screen.
        val link = step.link?.takeIf { opens(it) }
        if (link != null) {
            val view = linkView(link)
            body.addView(view, LinearLayout.LayoutParams(-2, -2).apply { topMargin = dp(6) })
            // The whole step takes the tap (48dp+ without padding the link); the ripple stays on the link.
            // Only on a new size: a fresh delegate mid-gesture would drop the finger's UP.
            addOnLayoutChangeListener { row, l, t, r, b, ol, ot, or, ob ->
                if (r - l != or - ol || b - t != ob - ot || row.touchDelegate == null) {
                    row.touchDelegate = android.view.TouchDelegate(android.graphics.Rect(0, 0, r - l, b - t), view)
                }
            }
        }
        addView(body, LinearLayout.LayoutParams(0, -2, 1f))
    }

    private fun linkView(link: Link) = TextView(this).apply {
        val ink = getColor(R.color.primary)
        textSize = 14f
        typeface = android.graphics.Typeface.create("sans-serif-medium", android.graphics.Typeface.NORMAL)
        setTextColor(ink)
        val icon = getDrawable(R.drawable.ic_open_in_new)!!.mutate().apply { setTint(ink) }
        // 6dp after the last word, and never alone on a line: a word joiner holds it there.
        // 16dp at the 14sp of the label: it grows with the font scale like the text.
        val size = (textSize * 16 / 14).toInt()
        val gap = android.graphics.drawable.InsetDrawable(icon, dp(6), 0, 0, 0).apply { setBounds(0, 0, dp(6) + size, size) }
        text = android.text.SpannableString(link.label + "\u2060\uFFFC").apply {
            setSpan(android.text.style.ImageSpan(gap, android.text.style.ImageSpan.ALIGN_BOTTOM), length - 1, length, android.text.Spanned.SPAN_EXCLUSIVE_EXCLUSIVE)
        }
        contentDescription = "${link.label}, откроются настройки телефона"
        accessibilityDelegate = object : View.AccessibilityDelegate() {
            override fun onInitializeAccessibilityNodeInfo(host: View, info: AccessibilityNodeInfo) {
                super.onInitializeAccessibilityNodeInfo(host, info)
                info.className = android.widget.Button::class.java.name
            }
        }
        val ripple = TypedValue()
        theme.resolveAttribute(android.R.attr.selectableItemBackground, ripple, true)
        setBackgroundResource(ripple.resourceId)
        setOnClickListener { openLink(link) }
    }

    private fun opens(link: Link) = link.pages.isEmpty() || link.pages.any { canStart(it) }

    // Resolving finds non-exported activities too, which only throw on start.
    private fun canStart(page: Page) = packageManager.resolveActivity(Intent().setComponent(page.component), 0)
        ?.activityInfo?.let { it.exported && it.permission == null } == true

    private fun pageIntent(page: Page): Intent {
        if (!page.battery) return Intent().setComponent(page.component)
        return Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:$packageName"))
            .setComponent(page.component)
            .putExtra("package_name", packageName)
            .putExtra("package_label", "Split VPN")
    }

    private fun openLink(link: Link) {
        if (link.pages.isEmpty()) return openAppDetails()
        openVendorSettings(link.pages)
    }

    private fun openAppDetails() {
        runCatching { startActivity(Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:$packageName"))) }
    }

    override fun onNewIntent(intent: Intent) {
        super.onNewIntent(intent)
        onUpdateIntent(intent)
    }

    private fun onUpdateIntent(intent: Intent?) {
        if (intent?.getBooleanExtra(EXTRA_UPDATE, false) != true) return
        showTab("vpn")
        val v = versions
        if (v == null) updateWhenKnown = true else if (v.available) startUpdate()
    }

    override fun onSaveInstanceState(outState: Bundle) {
        super.onSaveInstanceState(outState)
        outState.putString(KEY_TAB, tab)
        outState.putString(KEY_INVITE, invite)
    }

    override fun onDestroy() {
        inviteDialog?.dismiss()
        shareDialog?.dismiss()
        installDialog?.dismiss()
        menuPopup?.dismiss()
        if (shown?.get() === this) shown = null
        super.onDestroy()
    }

    override fun onStart() {
        super.onStart()
        // A second instance (launcher, the update notification) may have taken it and gone.
        shown = java.lang.ref.WeakReference(this)
        renderBanner()
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

    // Both change in the system's own screens: asked again on every return.
    override fun onResume() {
        super.onResume()
        Updater.recheck(this)
        Updater.takePending(this)?.let { runCatching { startActivity(it) } }
        val optimized = !getSystemService(PowerManager::class.java).isIgnoringBatteryOptimizations(packageName)
        batteryOptimized = optimized
        findViewById<View>(R.id.batteryWarn).visibility = if (optimized) View.VISIBLE else View.GONE
        findViewById<View>(R.id.batteryOk).visibility = if (optimized) View.GONE else View.VISIBLE
        findViewById<TextView>(R.id.notificationState).text =
            if (notificationsOn()) "Включено · настраивается в системе" else "Выключено · настраивается в системе"
        renderTabs()
    }

    private fun dp(v: Float) = TypedValue.applyDimension(TypedValue.COMPLEX_UNIT_DIP, v, resources.displayMetrics)
    private fun dp(v: Int) = dp(v.toFloat()).toInt()

    private fun box(fill: Int, stroke: Int = Color.TRANSPARENT, radius: Float = 0f) = GradientDrawable().apply {
        setColor(fill)
        setStroke(dp(1), stroke)
        cornerRadius = dp(radius)
    }

    private fun tint(color: Int) = ColorStateList.valueOf(getColor(color))

    private val editable get() = state == VpnState.DISCONNECTED || state == VpnState.ERROR && !offlineRetry
    private val busy get() = state == VpnState.CONNECTING || state == VpnState.DISCONNECTING

    private fun enabled(svc: Service) = prefs.getBoolean(Services.prefKey(svc.id), svc.defaultEnabled)

    private fun showTab(next: String) {
        tab = next
        if (next == "settings") prefs.edit().putBoolean(KEY_KEEPALIVE_SHOWN, true).apply()
        findViewById<View>(R.id.pageVpn).visibility = if (next == "vpn") View.VISIBLE else View.GONE
        findViewById<View>(R.id.pageSettings).visibility = if (next == "settings") View.VISIBLE else View.GONE
        findViewById<View>(R.id.pageLog).visibility = if (next == "log") View.VISIBLE else View.GONE
        findViewById<TextView>(R.id.titleText).text =
            mapOf("vpn" to "Split VPN", "settings" to "Настройки", "log" to "Журнал")[next]
        renderTabs()
    }

    private fun renderTabs() {
        val bar = findViewById<LinearLayout>(R.id.tabBar)
        bar.removeAllViews()
        val tabs = listOf(
            Triple("vpn", R.drawable.ic_nav_connection, "Соединение"),
            Triple("settings", R.drawable.ic_nav_settings, "Настройки"),
            Triple("log", R.drawable.ic_nav_log, "Журнал"),
        )
        for ((key, icon, label) in tabs) {
            val active = key == tab
            val ink = if (active) R.color.on_primary_container else R.color.on_surface_variant
            // Only the battery: whether the firmware step is done, nothing can tell.
            val badge = key == "settings" && batteryOptimized == true
            val item = LinearLayout(this).apply {
                orientation = LinearLayout.VERTICAL
                gravity = Gravity.CENTER
                setOnClickListener { showTab(key) }
                isSelected = active
                if (badge) contentDescription = "$label, нужно действие"
            }
            val pill = FrameLayout(this).apply {
                background = if (active) box(getColor(R.color.primary_container), radius = 16f) else null
                addView(ImageView(this@MainActivity).apply {
                    setImageResource(icon)
                    imageTintList = tint(ink)
                    importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_NO
                }, FrameLayout.LayoutParams(dp(24), dp(24), Gravity.CENTER))
                if (badge) addView(View(this@MainActivity).apply {
                    background = box(getColor(R.color.warning), radius = 4f)
                }, FrameLayout.LayoutParams(dp(8), dp(8), Gravity.TOP or Gravity.END).apply {
                    topMargin = dp(3)
                    marginEnd = dp(13)
                })
            }
            val labelView = TextView(this).apply {
                text = label
                includeFontPadding = false
                textSize = 12f
                typeface = android.graphics.Typeface.create("sans-serif-medium", android.graphics.Typeface.NORMAL)
                setTextColor(getColor(ink))
            }
            item.addView(pill, LinearLayout.LayoutParams(dp(56), dp(32)))
            item.addView(labelView, LinearLayout.LayoutParams(-2, -2).apply { topMargin = dp(4) })
            bar.addView(item, LinearLayout.LayoutParams(0, -1, 1f))
        }
    }

    private fun render() {
        renderServices()
        renderStatus()
        renderClock()
        renderBanner()
        renderUsage()
        // The service pushes nothing on a used-up day, so the new one needs its own redraw.
        ticker.removeCallbacks(midnight)
        if (started && quotaOut) ticker.postDelayed(midnight, untilMidnight())
        findViewById<TextView>(R.id.logText).text =
            if (log.isEmpty()) "Журнал начат" else log.reversed().joinToString("\n")
    }

    private val directMode get() = prefs.getBoolean(KEY_DIRECT, true)

    private fun renderServices() {
        val pings = mutableMapOf<String, String>()
        for (line in log) {
            val m = PROBE_LINE.find(line) ?: continue
            pings[m.groupValues[1]] = m.groupValues[2].replace(' ', ' ')
        }
        val list = findViewById<LinearLayout>(R.id.serviceList)
        // Locked as a whole while the VPN is on: the switches keep their places.
        list.alpha = if (editable) 1f else 0.5f
        for ((i, svc) in Services.ALL.withIndex()) {
            val en = enabled(svc)
            val ping = pings[svc.title].takeIf { state == VpnState.CONNECTED && !waitingNow && en }
            val row = list.getChildAt(i) ?: newServiceRow(list, svc)
            row.isEnabled = editable
            row.findViewById<TextView>(R.id.name).text = labels[svc.id] ?: svc.title
            val note = row.findViewById<TextView>(R.id.note)
            if (svc.id == "youtube") {
                val mode = row.findViewById<TextView>(R.id.ytMode)
                mode.visibility = if (en) View.VISIBLE else View.GONE
                row.findViewWithTag<View>("badge").visibility = if (en) View.GONE else View.VISIBLE
                mode.text = if (directMode) "Напрямую" else "Через VPN"
                mode.isEnabled = editable
                mode.contentDescription = "Режим YouTube: ${mode.text}"
                // The chip names the mode; the probe pinged the relay, which direct
                // YouTube does not use. Also while the server is down.
                note.text = if (en && directMode && state == VpnState.CONNECTED) when (direct) {
                    "testing" -> "ищу обход…"
                    "works" -> null
                    else -> ping
                } else ping
            } else {
                note.text = ping
            }
            renderSwitch(row.findViewById(R.id.seg), row.findViewById(R.id.knob), en)
        }
    }

    private fun renderSwitch(seg: View, knob: View, on: Boolean) {
        seg.background = box(getColor(if (on) R.color.primary else R.color.surface_container_highest), radius = 16f)
        knob.backgroundTintList = tint(if (on) R.color.on_primary else R.color.switch_thumb_off)
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
                tag = "badge"
                textSize = 10f
                gravity = Gravity.CENTER
                setTextColor(getColor(R.color.on_surface_variant))
                setBackgroundResource(R.drawable.simple_icon_bg)
            }
            badges.addView(badge, i, LinearLayout.LayoutParams(dp(22), dp(22)).apply { marginEnd = dp(6) })
        }
        if (svc.id == "youtube") badges.addView(modeChip(), 0, LinearLayout.LayoutParams(-2, -2).apply { marginEnd = dp(8) })
        row.setOnClickListener {
            // performClick reaches a disabled view too.
            if (!editable) return@setOnClickListener
            prefs.edit().putBoolean(Services.prefKey(svc.id), !enabled(svc)).apply()
            renderServices()
            renderClock()
        }
        list.addView(row)
        return row
    }

    private fun modeChip() = TextView(this).apply {
        id = R.id.ytMode
        textSize = 12f
        typeface = android.graphics.Typeface.create("sans-serif-medium", android.graphics.Typeface.NORMAL)
        includeFontPadding = false
        gravity = Gravity.CENTER_VERTICAL
        setTextColor(getColor(R.color.on_primary_container))
        background = box(getColor(R.color.primary_container), radius = 6f)
        setPaddingRelative(dp(8), dp(3), dp(4), dp(3))
        minHeight = dp(22)
        val chevron = getDrawable(R.drawable.ic_expand_more)!!.mutate().apply {
            setBounds(0, 0, dp(14), dp(14))
            setTintList(tint(R.color.on_primary_container))
        }
        setCompoundDrawablesRelative(null, null, chevron, null)
        compoundDrawablePadding = dp(2)
        setOnClickListener { if (editable) showModeMenu(this) }
    }

    private class MenuItem(val title: String, val caption: String?, val selected: Boolean, val onPick: () -> Unit)

    private fun showModeMenu(anchor: View) {
        val current = directMode
        val modes = listOf(
            Triple(true, "Напрямую", "Обход замедления провайдера. Почти не тратит лимит."),
            Triple(false, "Через VPN", "Если напрямую скорости не хватает. Тратит лимит."),
        )
        showMenu(anchor, modes.map { (on, title, caption) ->
            MenuItem(title, caption, on == current) {
                if (editable) {
                    prefs.edit().putBoolean(KEY_DIRECT, on).apply()
                    renderServices()
                }
            }
        })
    }

    private fun showMenu(anchor: View, items: List<MenuItem>, align: Int = Gravity.START) {
        val menu = LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            background = box(getColor(R.color.menu_container), radius = 16f)
            // No shadow shows in the dark: the outline keeps the menu off the card.
            // A foreground, or the selected item's fill covers it.
            foreground = box(Color.TRANSPARENT, getColor(R.color.outline_variant), 16f)
            // The selected item's fill must not stick out of the corners.
            clipToOutline = true
            elevation = dp(3f)
        }
        val popup = PopupWindow(menu, dp(if (items.any { it.caption != null }) 288 else 220), ViewGroup.LayoutParams.WRAP_CONTENT, true)
        for (item in items) {
            menu.addView(LinearLayout(this).apply {
                gravity = Gravity.CENTER_VERTICAL
                setPadding(dp(16), dp(12), dp(16), dp(12))
                minimumHeight = dp(if (item.caption != null) 56 else 48)
                if (item.selected) setBackgroundColor(getColor(R.color.menu_item_selected))
                addView(LinearLayout(this@MainActivity).apply {
                    orientation = LinearLayout.VERTICAL
                    addView(TextView(this@MainActivity).apply {
                        text = item.title
                        textSize = 15f
                        setTextColor(getColor(R.color.on_surface))
                    })
                    if (item.caption != null) addView(TextView(this@MainActivity).apply {
                        text = item.caption
                        textSize = 12f
                        setTextColor(getColor(R.color.on_surface_variant))
                    })
                }, LinearLayout.LayoutParams(0, -2, 1f).apply { marginEnd = dp(12) })
                addView(ImageView(this@MainActivity).apply {
                    setImageResource(R.drawable.ic_check)
                    imageTintList = tint(R.color.primary)
                    visibility = if (item.selected) View.VISIBLE else View.INVISIBLE
                    importantForAccessibility = View.IMPORTANT_FOR_ACCESSIBILITY_NO
                }, LinearLayout.LayoutParams(dp(20), dp(20)))
                isSelected = item.selected
                setOnClickListener {
                    popup.dismiss()
                    item.onPick()
                }
            })
        }
        menuPopup = popup
        popup.elevation = dp(3f)
        popup.showAsDropDown(anchor, 0, dp(4), align)
    }

    private fun renderStatus() {
        val button = findViewById<View>(R.id.toggleButton)
        val dot = findViewById<View>(R.id.statusDot)
        val on = state == VpnState.CONNECTED
        findViewById<TextView>(R.id.hintText).text =
            if (editable) "Что пойдёт через VPN" else "Чтобы изменить список, выключите VPN"
        findViewById<View>(R.id.hintLock).visibility = if (editable) View.GONE else View.VISIBLE
        findViewById<View>(R.id.chip).background = box(
            getColor(if (waitingNow) R.color.warning_container else if (on) R.color.success_container else R.color.surface_container_high), radius = 16f
        )
        findViewById<TextView>(R.id.statusText).setTextColor(
            getColor(if (waitingNow) R.color.on_warning_container else if (on) R.color.on_success_container else R.color.on_surface)
        )
        val green = getColor(R.color.success)
        val grey = getColor(R.color.outline)
        // Not a failure but nothing to press until midnight: greyed out like busy.
        // Nor while min_version turns this version away: the banner's "Обновить" is the way.
        val inert = busy || quotaOut || updateRequired
        dot.background = box(if (waitingNow) getColor(R.color.warning) else if (on) green else if (busy) grey else getColor(R.color.on_surface_variant), radius = 4f)
        findViewById<View>(R.id.buttonDot).background =
            box(if (waitingNow) getColor(R.color.warning) else if (on) green else if (inert) grey else getColor(R.color.on_main_button), radius = 5f)
        findViewById<TextView>(R.id.buttonLabel).apply {
            text = if (quotaOut) "Лимит на сегодня исчерпан" else when (state) {
                VpnState.CONNECTING -> "Подключение…"
                VpnState.DISCONNECTING -> "Отключение…"
                VpnState.CONNECTED -> "Выключить"
                VpnState.ERROR -> if (offlineRetry) "Выключить" else "Подключить"
                VpnState.DISCONNECTED -> "Подключить"
            }
            setTextColor(getColor(if (on) R.color.on_surface else if (inert) R.color.on_surface_variant else R.color.on_main_button))
        }
        button.isEnabled = !inert
        button.background = when {
            on -> box(getColor(R.color.surface_lowest), getColor(R.color.outline), 30f)
            inert -> box(getColor(R.color.surface_container_high), radius = 30f)
            else -> box(getColor(R.color.main_button), radius = 30f)
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
            // The service's text is for its notification, read outside the app.
            e == TunnelVpnService.ERR_VPN_TAKEN -> "Разрешение VPN занято другим приложением"
            e.any { it in 'а'..'я' } -> e
            "unreachable" in e || "relay reply" in e -> "Сервер недоступен. Проверьте интернет и повторите"
            "rejected" in e -> "Сервер отклонил доступ"
            else -> "Не удалось подключиться"
        }
    }

    private enum class Level(val bg: Int, val ink: Int) {
        INFO(R.color.primary_container, R.color.on_primary_container),
        WARN(R.color.warning_container, R.color.on_warning_container),
        ERROR(R.color.error_container, R.color.on_error_container),
    }

    private class Banner(
        val level: Level, val icon: Int, val text: String,
        val action: String? = null, val onAction: (() -> Unit)? = null,
        val busy: Boolean = false, val closable: Boolean = false,
    )

    private val updateAvailable get() = versions.let { v ->
        v != null && v.available && !updateRequired && v.latest > prefs.getLong(KEY_UPDATE_DISMISSED, 0)
    }

    // One at a time, the most important: an error, then a warning, then news.
    private fun banner(): Banner? = when {
        updateRequired -> Banner(Level.ERROR, R.drawable.ic_system_update, "Эта версия больше не поддерживается", updateLabel, ::startUpdate, busy = updateBusy)
        quotaOut -> Banner(Level.ERROR, R.drawable.ic_error, "Дневной лимит трафика исчерпан. VPN снова заработает после 00:00")
        state == VpnState.ERROR && error == TunnelVpnService.ERR_NEED_CODE ->
            Banner(Level.WARN, R.drawable.ic_gpp_maybe, "Не удалось проверить устройство через Google Play", "Ввести код", ::showInviteDialog)
        // A stale used-up day is over: nothing to tell.
        state == VpnState.ERROR && !offlineRetry && !quotaError ->
            Banner(Level.ERROR, R.drawable.ic_error, errorText(), "Повторить", ::startVpn)
        waitingNow && (waiting == Waiting.NO_NETWORK || offlineRetry) ->
            Banner(Level.WARN, R.drawable.ic_wifi_off, "Нет интернета — VPN заработает, когда сеть появится")
        waitingNow -> Banner(Level.WARN, R.drawable.ic_schedule, "Сервер не отвечает — подключимся сами, как только он ответит")
        // While downloading: no way to hide the only sign of it.
        updateAvailable -> Banner(Level.INFO, R.drawable.ic_system_update, "Доступна новая версия", updateLabel, ::startUpdate, busy = updateBusy,
            closable = !updating && !Updater.installing)
        else -> null
    }

    private fun renderBanner() {
        val view = findViewById<View>(R.id.banner)
        val b = banner()
        view.visibility = if (b == null) View.GONE else View.VISIBLE
        if (b == null) return
        val ink = getColor(b.level.ink)
        view.background = box(getColor(b.level.bg), radius = 16f)
        findViewById<ImageView>(R.id.bannerIcon).apply {
            setImageResource(b.icon)
            imageTintList = tint(b.level.ink)
        }
        findViewById<TextView>(R.id.bannerText).apply {
            text = b.text
            setTextColor(ink)
        }
        findViewById<TextView>(R.id.bannerAction).apply {
            visibility = if (b.action == null) View.GONE else View.VISIBLE
            text = b.action
            setTextColor(ink)
            isEnabled = !b.busy
            alpha = if (b.busy) 0.6f else 1f
        }
        findViewById<ImageView>(R.id.bannerClose).apply {
            visibility = if (b.closable) View.VISIBLE else View.GONE
            imageTintList = tint(b.level.ink)
        }
        view.setPaddingRelative(dp(14), dp(12), if (b.action != null || b.closable) dp(6) else dp(14), dp(12))
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
            if (u != null) text = "Сегодня через VPN: ${Quota.format(used)} из ${Quota.format(u.limit)}"
        }
    }

    // Also before any try to connect: the service would only refuse.
    private val updateRequired get() = state == VpnState.ERROR && error == TunnelVpnService.ERR_UPDATE_REQUIRED ||
        state == VpnState.DISCONNECTED && versions?.required == true

    // Until the installer asks, there is no dialog to bring back.
    private val updateBusy get() = updating || Updater.installing && !Updater.canReopen

    private val updateLabel get() = when {
        !updating && Updater.installing -> if (Updater.canReopen) "Установить" else "Установка…"
        !updating -> "Обновить"
        progress >= 0 -> "Скачивается… $progress%"
        else -> "Скачивается…"
    }

    private fun openDownloadPage() = openUrl((versions ?: Versions()).page)

    private fun startUpdate() {
        val v = versions ?: return
        // Self-update turned off in Remote Config: no report, just the page.
        if (!Updater.enabled(v)) return openDownloadPage()
        if (updating) return
        if (Updater.installing) {
            // No dialog yet (the service notification's «Обновить»): it is on its way.
            if (!Updater.canReopen || Updater.reopen(this)) return
            Updater.awaiting = null
        }
        updating = true
        progress = -1
        render()
        // Not the activity: a recreated one would stay in memory for the whole download.
        val app = applicationContext
        background(Runnable {
            var r = Updater.Result.FAILED
            try {
                r = update(app, v) { p ->
                    onMain {
                        if (updating) {
                            progress = p
                            live?.renderBanner()
                        }
                    }
                }
                // Here, not in onUpdateResult: that one is skipped with no activity on screen.
                when (r) {
                    Updater.Result.STARTED -> Updater.event(app, "started", Updater.lastDetail, Updater.lastMirror)
                    Updater.Result.NEED_PERMISSION -> Updater.event(app, "need_permission")
                    Updater.Result.FAILED -> Updater.event(app, "failed", Updater.lastDetail ?: Updater.lastKind?.tag, Updater.lastMirror)
                    Updater.Result.BAD_URL -> Updater.event(app, "failed", "url")
                    Updater.Result.UP_TO_DATE, Updater.Result.DISABLED, Updater.Result.BUSY -> Unit
                }
            } finally {
                // Or "updating" sticks for the life of the process.
                onMain { live?.onUpdateResult(r, v) ?: run { updating = false } }
            }
        })
    }

    private fun onUpdateResult(r: Updater.Result, v: Versions) {
        updating = false
        val line = "update: $r" + (Updater.lastFailure?.takeIf { r == Updater.Result.FAILED || r == Updater.Result.BAD_URL }?.let { " ($it)" } ?: "") +
            (Updater.lastDetail?.takeIf { r == Updater.Result.FAILED || r == Updater.Result.STARTED }?.let { ", mirrors $it" } ?: "")
        AppLog.i(line)
        vpn.log(line)
        when (r) {
            Updater.Result.NEED_PERMISSION -> showInstallHint()
            // min_version raised, latest_version forgotten: nothing to download, but no way around it either.
            Updater.Result.UP_TO_DATE -> if (v.required) openDownloadPage()
            else prefs.edit().putLong(KEY_UPDATE_DISMISSED, v.latest).apply()
            Updater.Result.FAILED -> {
                Toast.makeText(this, "Не удалось скачать", Toast.LENGTH_SHORT).show()
                openDownloadPage()
            }
            Updater.Result.BAD_URL, Updater.Result.DISABLED -> openDownloadPage()
            Updater.Result.STARTED, Updater.Result.BUSY -> Unit
        }
        render()
    }

    // The system screen is bare and warns of danger: say first what it is for.
    private fun showInstallHint() {
        val dialog = Dialog(this)
        dialog.setContentView(R.layout.dialog_install_simple)
        dialog.window?.apply {
            setBackgroundDrawable(ColorDrawable(Color.TRANSPARENT))
            setDimAmount(resources.getFraction(R.fraction.scrim, 1, 1))
            setLayout(resources.displayMetrics.widthPixels - dp(48), ViewGroup.LayoutParams.WRAP_CONTENT)
        }
        dialog.findViewById<View>(R.id.cancel).setOnClickListener { dialog.dismiss() }
        dialog.findViewById<View>(R.id.apply).setOnClickListener {
            dialog.dismiss()
            // A ROM without the screen: the page, whose APK the browser installs.
            runCatching { startActivityForResult(Updater.permissionIntent(this), INSTALL_PERMISSION_CODE) }
                .onFailure { openDownloadPage() }
        }
        installDialog = dialog
        dialog.show()
    }

    private fun openUrl(url: String) {
        runCatching { startActivity(Intent(Intent.ACTION_VIEW, Uri.parse(url))) }
    }

    private fun showInviteDialog() {
        val dialog = Dialog(this)
        dialog.setContentView(R.layout.dialog_invite_simple)
        dialog.window?.apply {
            setBackgroundDrawable(ColorDrawable(Color.TRANSPARENT))
            setDimAmount(resources.getFraction(R.fraction.scrim, 1, 1))
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
            setDimAmount(resources.getFraction(R.fraction.scrim, 1, 1))
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
        // Black on white in any theme: scanners expect it.
        val px = IntArray(m.width * m.height) { if (m[it % m.width, it / m.width]) Color.BLACK else Color.WHITE }
        return Bitmap.createBitmap(px, m.width, m.height, Bitmap.Config.ARGB_8888)
    }
    // The service stays in the foreground either way; only the system can
    // hide its notification (the channel, or on 13+ the app's permission).
    private fun notificationsOn(): Boolean {
        val nm = getSystemService(NotificationManager::class.java)
        TunnelVpnService.createChannel(this)
        return nm.areNotificationsEnabled() &&
            nm.getNotificationChannel(TunnelVpnService.CHANNEL_ID)?.importance != NotificationManager.IMPORTANCE_NONE
    }

    private fun openNotificationSettings() {
        val appOn = getSystemService(NotificationManager::class.java).areNotificationsEnabled()
        // With the app's notifications off the channel page is inert.
        // Trimmed ROMs may lack either page: fall through like openVendorSettings.
        val channel = Intent(Settings.ACTION_CHANNEL_NOTIFICATION_SETTINGS)
            .putExtra(Settings.EXTRA_APP_PACKAGE, packageName)
            .putExtra(Settings.EXTRA_CHANNEL_ID, TunnelVpnService.CHANNEL_ID)
        val app = Intent(Settings.ACTION_APP_NOTIFICATION_SETTINGS).putExtra(Settings.EXTRA_APP_PACKAGE, packageName)
        val details = Intent(Settings.ACTION_APPLICATION_DETAILS_SETTINGS, Uri.parse("package:$packageName"))
        for (i in if (appOn) listOf(channel, app, details) else listOf(app, details)) {
            runCatching { startActivity(i) }.onSuccess { return }
        }
    }

    // Vendor activities are undocumented and move between ROM versions;
    // fall back to the app's own settings page.
    private fun openVendorSettings(pages: List<Page>) {
        for (page in pages) {
            if (canStart(page)) runCatching { startActivity(pageIntent(page)) }.onSuccess { return }
        }
        openAppDetails()
    }

    // HyperOS answers the request with its own battery page: "no restrictions" there
    // sets its power keeper but not Android's whitelist this tab checks, so the
    // warning stayed. Ask the stock dialog by name first.
    private fun requestBatteryWhitelist() {
        val request = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:$packageName"))
        val stock = ComponentName("com.android.settings", "com.android.settings.fuelgauge.RequestIgnoreBatteryOptimizations")
        runCatching { startActivity(Intent(request).setComponent(stock)) }.onSuccess { return }
        runCatching { startActivity(request) }
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
        // The settings screen answers RESULT_CANCELED either way. Not granted:
        // the page, where the browser does the install instead.
        // Some ROMs kill the process on the grant: the new one has no snapshot yet.
        if (requestCode == INSTALL_PERMISSION_CODE) when {
            !Updater.canInstall(this) -> openDownloadPage()
            versions == null -> updateWhenKnown = true
            else -> startUpdate()
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
