package org.duckdns.splitvpn

import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.pm.ServiceInfo
import android.net.ConnectivityManager
import android.net.LinkProperties
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import android.os.Handler
import android.os.IBinder
import android.os.Looper
import android.os.Message
import android.os.Messenger
import android.os.ParcelFileDescriptor
import android.os.RemoteException
import android.os.SystemClock
import android.util.Base64
import androidx.core.content.ContextCompat
import com.google.android.gms.tasks.Tasks
import com.google.android.play.core.integrity.IntegrityManagerFactory
import com.google.android.play.core.integrity.IntegrityTokenRequest
import com.google.firebase.analytics.FirebaseAnalytics
import com.google.firebase.remoteconfig.CustomSignals
import com.google.firebase.remoteconfig.FirebaseRemoteConfig
import com.google.firebase.remoteconfig.FirebaseRemoteConfigSettings
import java.net.HttpURLConnection
import java.net.URL
import java.security.SecureRandom
import java.time.Instant
import java.time.ZoneId
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean

/** Everything that needs a real TUN, the Go AAR or the network; swapped in unit tests. */
internal interface Backend {
    fun establish(service: VpnService, enabled: List<Service>, extra: List<Route>): ParcelFileDescriptor?
    /** On success takes ownership of [fd]; on failure the caller closes it. */
    fun start(fd: ParcelFileDescriptor, addr: String, sni: String, path: String, cred: ByteArray, domains: String, routes: String, cacheFile: String, host: tunnel.Host, logger: tunnel.Logger)
    fun stop()
    /**
     * New relay sessions go to [ep], open ones stay; Go then reports RelayDown for it afresh.
     * [suspect]: moving on from a dead one, apps are refused until a probe gets through.
     */
    fun setEndpoint(ep: TunnelVpnService.Endpoint, suspect: Boolean)
    fun networkChanged()
    fun networkLost()
    /** Drops Go's DNS cache only; the relay's health is networkChanged's business. */
    fun dnsChanged()
    fun probe(url: String)
    /** Play Integrity token for [nonce]; null when unavailable (no project number, no Google services). */
    fun integrityToken(ctx: Context, nonce: ByteArray): ByteArray?
    fun register(addr: String, sni: String, path: String, kind: Int, proof: ByteArray): ByteArray
    fun credExpires(cred: ByteArray): Long
    /** Debug test relay: verify its TLS against this PEM CA; empty restores the system roots. */
    fun trustCA(pem: String)
    /** Relays to try, in order; decrypting the baked-in list takes Go. */
    fun endpoints(): List<TunnelVpnService.Endpoint>
    fun decryptEndpoints(blob: String): String
    /** Fetches and activates Remote Config, blocking; throws when it cannot. */
    fun fetchConfig(): RcValues
    /** Relay bytes used today and the limit (0: none); over it Go refuses the relay and tells the host. */
    fun setQuota(used: Long, limit: Long)
    fun usage(): Long
    /** NXDOMAIN for ad domains; set before start. */
    fun setAdBlock(on: Boolean)
    /** Unix seconds of the last session the relay let in (101); 0: none in this process. */
    fun lastRelayOk(): Long
    /** Firebase Analytics. Names and values in latin, never a site, an address or an error's text. */
    fun event(ctx: Context, name: String, params: Map<String, String> = emptyMap())
    fun userProperty(ctx: Context, name: String, value: String)
    fun setAnalytics(ctx: Context, on: Boolean)
}

internal object GoBackend : Backend {
    override fun establish(service: VpnService, enabled: List<Service>, extra: List<Route>): ParcelFileDescriptor? {
        val b = service.Builder()
            .setSession("SplitVPN")
            .addAddress("10.255.0.1", 32)
            // Fake resolver served by the Go stack (DoH through the relay).
            .addDnsServer("10.255.0.2")
            .addRoute("10.255.0.2", 32)
            .setMtu(1500)
        for (r in enabled.flatMap { it.allRoutes } + extra) b.addRoute(r.address, r.prefix)
        return b.establish()
    }
    override fun start(fd: ParcelFileDescriptor, addr: String, sni: String, path: String, cred: ByteArray, domains: String, routes: String, cacheFile: String, host: tunnel.Host, logger: tunnel.Logger) {
        tunnel.Tunnel.setVerbose(BuildConfig.DEBUG)
        tunnel.Tunnel.start(fd.fd.toLong(), addr, sni, path, cred, domains, routes, cacheFile, host, logger)
        fd.detachFd() // Go owns it now; Tunnel.stop() closes it
    }
    override fun trustCA(pem: String) = tunnel.Tunnel.trustCA(pem)
    override fun endpoints() = Endpoints.defaults
    override fun decryptEndpoints(blob: String): String = tunnel.Tunnel.decryptEndpoints(blob)
    // Goes past the relay: Go sends the RC and Installations hosts direct
    // (alwaysDirect in sni.go), though googleapis.com is a YouTube domain.
    override fun fetchConfig(): RcValues {
        val rc = FirebaseRemoteConfig.getInstance()
        Tasks.await(
            rc.setConfigSettingsAsync(
                FirebaseRemoteConfigSettings.Builder()
                    // RC throttles at about 5 fetches an hour; below the
                    // interval fetch answers from its own cache.
                    // A minute under the retry: equal ones would race, and
                    // every other retry would get RC's cache back.
                    .setMinimumFetchIntervalInSeconds(if (BuildConfig.DEBUG) 0 else 14 * 60)
                    .build(),
            ),
            10, TimeUnit.SECONDS,
        )
        // Always set, never only for debug: custom signals persist, and a
        // release installed over a debug build would keep "debug".
        Tasks.await(rc.setCustomSignals(CustomSignals.Builder().put("build", if (BuildConfig.DEBUG) "debug" else "release").build()), 10, TimeUnit.SECONDS)
        Tasks.await(rc.fetchAndActivate(), 60, TimeUnit.SECONDS)
        // Not asLong(): a typo in the console would throw and cost the endpoints too.
        val quota = rc.getValue("daily_quota_mb").takeIf { it.source == FirebaseRemoteConfig.VALUE_SOURCE_REMOTE }?.asString()?.trim()?.toLongOrNull()
        return RcValues(rc.getString("endpoints"), rc.getLong("min_version"), rc.getLong("latest_version"), rc.getString("update_url"), quota)
    }
    override fun setQuota(used: Long, limit: Long) = tunnel.Tunnel.setQuota(used, limit)
    override fun usage() = tunnel.Tunnel.usage()
    override fun setAdBlock(on: Boolean) = tunnel.Tunnel.setAdBlock(on)
    override fun lastRelayOk() = tunnel.Tunnel.lastRelayOK()
    override fun event(ctx: Context, name: String, params: Map<String, String>) = analytics(ctx) {
        logEvent(name, Bundle().apply { params.forEach { (k, v) -> putString(k, v) } })
    }
    override fun userProperty(ctx: Context, name: String, value: String) = analytics(ctx) { setUserProperty(name, value) }
    override fun setAnalytics(ctx: Context, on: Boolean) = analytics(ctx) {
        setAnalyticsCollectionEnabled(on)
        // Crash.init set it while collection was off, and the SDK dropped it.
        if (on) Crash.userId(ctx)?.let(::setUserId)
    }
    // Analytics must never be the thing that takes the tunnel down.
    private inline fun analytics(ctx: Context, body: FirebaseAnalytics.() -> Unit) {
        try {
            FirebaseAnalytics.getInstance(ctx).body()
        } catch (e: Exception) {
            AppLog.e("analytics", e)
        }
    }
    override fun integrityToken(ctx: Context, nonce: ByteArray): ByteArray? {
        val project = BuildConfig.INTEGRITY_PROJECT
        if (project == 0L) return null
        val request = IntegrityTokenRequest.builder()
            .setNonce(Base64.encodeToString(nonce, Base64.URL_SAFE or Base64.NO_WRAP or Base64.NO_PADDING))
            .setCloudProjectNumber(project)
            .build()
        return try {
            Tasks.await(IntegrityManagerFactory.create(ctx).requestIntegrityToken(request), 30, TimeUnit.SECONDS)
                .token().toByteArray()
        } catch (e: Exception) {
            AppLog.e("integrity token", e, expected = true) // no GMS, no network, timeout
            null
        }
    }
    override fun register(addr: String, sni: String, path: String, kind: Int, proof: ByteArray): ByteArray =
        tunnel.Tunnel.register(addr, sni, path, kind.toLong(), proof)
    override fun credExpires(cred: ByteArray) = tunnel.Tunnel.credExpires(cred)
    override fun stop() = tunnel.Tunnel.stop()
    override fun setEndpoint(ep: TunnelVpnService.Endpoint, suspect: Boolean) = tunnel.Tunnel.setEndpoint(ep.addr, ep.host, ep.path, suspect)
    override fun networkChanged() = tunnel.Tunnel.networkChanged()
    override fun networkLost() = tunnel.Tunnel.networkLost()
    override fun dnsChanged() = tunnel.Tunnel.dnsChanged()
    override fun probe(url: String) {
        val c = URL(url).openConnection() as HttpURLConnection
        c.requestMethod = "HEAD"
        c.connectTimeout = 5000
        c.readTimeout = 5000
        c.instanceFollowRedirects = false
        c.responseCode
        c.disconnect()
    }
}

class TunnelVpnService : VpnService() {

    internal var backend: Backend = GoBackend

    companion object {
        const val ACTION_START = "org.duckdns.splitvpn.START"
        const val ACTION_STOP = "org.duckdns.splitvpn.STOP"
        const val ACTION_CRASH = "org.duckdns.splitvpn.CRASH" // debug smoke test, see MainActivity
        const val ACTION_BIND = "org.duckdns.splitvpn.BIND"
        const val EXTRA_SERVICES = "services"
        const val EXTRA_INVITE = "invite"
        const val EXTRA_ADBLOCK = "adblock"
        // Debug builds only, for the instrumented tests against a relay on
        // the host: "host|ip|port|path", the PEM CA its TLS chains to, and
        // whether to forget the test credential first (renewal cases).
        const val EXTRA_ENDPOINT = "endpoint"
        // Instead of EXTRA_ENDPOINT: a list as Remote Config would bring it,
        // the blob's decrypted JSON. Taken like a fetched one (switching and
        // all), kept apart from the real cache; Firebase is not asked.
        const val EXTRA_RC_ENDPOINTS = "rc_endpoints"
        const val EXTRA_CA = "ca"
        const val EXTRA_FORGET_CRED = "forget_cred"
        // Debug builds: the daily limit in MB, as if Remote Config had sent
        // it, and today's count back to zero.
        const val EXTRA_QUOTA_MB = "quota_mb"
        const val EXTRA_QUOTA_RESET = "quota_reset"
        // Debug builds: Analytics collection on (DebugView). The SDK's flag
        // sticks across runs and overrides the manifest, so every START
        // without this extra turns it off again.
        const val EXTRA_ANALYTICS = "analytics"
        const val ERR_NEED_CODE = "Нужен код доступа"
        const val ERR_NO_SERVER = "Не удалось обновить доступ: нет связи с сервером. Повторите позже"
        const val ERR_OFFLINE = "Нет сети. Доступ обновится сам, когда сеть появится"
        const val ERR_UPDATE_REQUIRED = "Эта версия больше не поддерживается — обновите приложение"
        const val ERR_QUOTA = "Дневной лимит трафика исчерпан"
        internal const val ERR_ESTABLISH = "VPN establish failed"

        // Failures of normal life: no network, VDS down, access denied, no VPN
        // permission. Anything else on the way up is a bug and gets a report.
        internal fun expected(e: Throwable) = e.message.orEmpty().let { m ->
            listOf("relay unreachable", "relay reply", "register reply", "rejected", "stopped", ERR_ESTABLISH, ERR_NEED_CODE, ERR_NO_SERVER, ERR_UPDATE_REQUIRED).any(m::contains)
        }
        // vpn_failed's reason. Our own messages only: any other text may carry an address.
        internal fun reasonOf(msg: String) = when (msg) {
            ERR_NEED_CODE -> "need_code"
            ERR_NO_SERVER -> "no_server"
            ERR_OFFLINE -> "offline"
            ERR_UPDATE_REQUIRED -> "update_required"
            ERR_QUOTA -> "quota"
            ERR_ESTABLISH -> "establish"
            else -> "other"
        }
        // Renew this long before expiry so a week offline still ends connected.
        private const val RENEW_BEFORE_S = 24 * 3600L
        // Process-local prefs (:vpn only). Services.PREFS belongs to the UI
        // process; SharedPreferences are not safe across processes.
        const val PREFS = "vpn"
        const val KEY_WANTED = "wanted"
        const val KEY_SERVICES = "services"
        const val KEY_ADBLOCK = "adblock"
        const val KEY_FAILED_AT = "failed_at"
        // A fresh process (bind, sticky restart) does not retry a failed
        // connect sooner than this; explicit START and boot always try.
        const val RESUME_BACKOFF_MS = 60_000L
        // After a registration refusal: retrying every resume only collects
        // more refusals, yet a refusal can be the issuer's own outage or a
        // late GMS after boot, so wanted stays and the retry is just rarer.
        const val KEY_NEED_CODE = "need_code"
        const val NEED_CODE_BACKOFF_MS = 3600_000L
        // A listed site resolved outside the routes: the TUN is rebuilt with
        // the cached subnets, at most this often (rebuilds cut sessions).
        const val REBUILD_MIN_GAP_MS = 10 * 60_000L
        // And on a timer, in case the stale flag was swallowed by the gap.
        const val REBUILD_PERIOD_MS = 12 * 3600_000L
        const val CONFIG_PERIOD_MS = 6 * 3600_000L
        // While the server is unavailable: a new list may be published any
        // moment. Release stays just above RC's minimumFetchInterval, a retry
        // sooner would only get RC's own cache back.
        val CONFIG_RETRY_MS = if (BuildConfig.DEBUG) 90_000L else 15 * 60_000L
        // Every endpoint failed its verdict: the next round waits at least
        // this long instead of handshaking the whole list every few seconds.
        // As long as the fetch retry: a new list is the other way out.
        val ROUND_PAUSE_MS = CONFIG_RETRY_MS
        // Both double while the servers stay down, up to this many times the
        // base (15 min → 3 h in release): a phone in a drawer must not wake
        // four times an hour for a whole day. Anything new starts them short again.
        private const val BACKOFF_MAX = 12L
        internal fun backoff(base: Long, level: Int) = minOf(base shl minOf(level, 4), base * BACKOFF_MAX)
        // Start blocked by the cached min_version: how long it waits for a
        // fresh one before refusing on the cache.
        internal var VERSION_FETCH_WAIT_MS = 10_000L
        // A switch between networks is a moment without any: not worth a warning.
        private const val NO_NETWORK_AFTER_MS = 5_000L
        private const val NEW_NETWORK_GRACE_MS = 10_000L
        // Go retries the current relay on a new network at once: up to 10 s
        // of dial and 10 s of upgrade. Only then is "still down" news about it.
        internal const val NEW_NETWORK_SWITCH_MS = NEW_NETWORK_GRACE_MS + 20_000L
        private const val REPROBE_DELAY_MS = 5_000L
        private const val WHY_NEW_LIST = "new list"
        internal const val CHANNEL_ID = "vpn_channel"
        private const val NOTIFICATION_ID = 1

        // The UI opens this channel's system page: it must exist before the
        // service ever posted on it.
        internal fun createChannel(ctx: Context) {
            ctx.getSystemService(NotificationManager::class.java)
                .createNotificationChannel(NotificationChannel(CHANNEL_ID, "Состояние VPN", NotificationManager.IMPORTANCE_LOW))
        }
        private const val QUOTA_NOTIFICATION_ID = 2
        private const val QUOTA_CHANNEL_ID = "quota_channel"
    }

    // host is for SNI/cert only; the client never resolves it (would loop
    // through our own resolver). The relay lives behind the cover site at a
    // secret path. The list is the last one from Remote Config, else the
    // baked-in one. An unreachable relay does not fail start: the running
    // tunnel moves on by itself, see onNoServer.
    data class Endpoint(val host: String, val ip: String, val port: Int, val path: String) {
        val addr get() = "$ip:$port"
    }

    @Volatile private var testEndpoint: Endpoint? = null
    @Volatile private var testRc = false
    // Never the real defaults under a test list: the emulator must not dial the VDS.
    private val endpoints get() = testEndpoint?.let { listOf(it) } ?: Endpoints.cached(this, testRc) ?: if (testRc) emptyList() else backend.endpoints()
    // What the running tunnel talks to; null while none runs.
    @Volatile private var current: Endpoint? = null

    // The running one (a rebuild keeps it), else the last that worked, then the rest in order.
    private fun ordered(): List<Endpoint> {
        val list = endpoints
        val first = (current ?: Endpoints.working(this, testRc))?.takeIf { it in list } ?: return list
        return listOf(first) + (list - first)
    }
    private val testStand get() = testEndpoint != null || testRc

    /**
     * Runs [op] against each endpoint until one succeeds. A relay that answered (rejected) is final,
     * unless [tryAll]: a registration 404 also means "not an issuer", so the rest still get asked
     * and the refusal wins over network errors only once none issued.
     */
    private fun <T> anyEndpoint(what: String, tryAll: Boolean = false, op: (Endpoint) -> T): T {
        var last: Exception = IllegalStateException("no endpoints: ${Endpoints.failure ?: "empty list"}")
        var refusal: Exception? = null
        for (ep in ordered()) {
            try {
                return op(ep)
            } catch (e: Exception) {
                if (e.message?.contains("rejected") == true) {
                    if (!tryAll) throw e
                    refusal = e
                }
                // The stack goes with the last failure, from whoever catches it.
                AppLog.e("$what via ${ep.host} failed: $e", expected = true)
                last = e
            }
        }
        throw refusal ?: last
    }

    private val clients = mutableSetOf<Messenger>()
    private val messenger = Messenger(Handler(Looper.getMainLooper()) { msg ->
        when (msg.what) {
            VpnClient.MSG_REGISTER -> msg.replyTo?.let { clients += it; push(it); resetBackoff() }
            VpnClient.MSG_UNREGISTER -> msg.replyTo?.let { clients -= it }
        }
        true
    })

    private val stateListener: (VpnState, String?) -> Unit = { _, _ -> pushAll() }
    private val logListener: (List<String>) -> Unit = { pushAll() }

    private var netCallback: ConnectivityManager.NetworkCallback? = null
    private var defaultCallback: ConnectivityManager.NetworkCallback? = null
    @Volatile private var tunnelThread: Thread? = null
    @Volatile private var probeThread: Thread? = null
    // Guards the stopping flag against the CONNECTED transition, so a STOP
    // that lands while Tunnel.start is running never sees CONNECTED after it.
    // An Object for its monitor: the start's wait for the fetch wakes on the fetch or a STOP.
    private val lock = Object()
    private var stopping = false

    private val vpnPrefs get() = getSharedPreferences(PREFS, MODE_PRIVATE)
    private val goLogger get() = AppLog.goLogger

    override fun onCreate() {
        super.onCreate()
        // Before anything logs them; the UI process masks by this list too.
        Endpoints.attach(this)
        Endpoints.remember(Endpoints.defaults + Endpoints.cached(this).orEmpty())
        AppLog.init(this)
        Crash.init(this, "vpn")
        TunnelState.subscribe(stateListener)
        TunnelState.subscribeLog(logListener)
        // The user hid the notification by blocking its channel (the menu
        // sends them there); unblocking does not bring the posted one back.
        ContextCompat.registerReceiver(
            this, channelReceiver,
            IntentFilter(NotificationManager.ACTION_NOTIFICATION_CHANNEL_BLOCK_STATE_CHANGED),
            ContextCompat.RECEIVER_NOT_EXPORTED,
        )
        // Process came back (UI bind, boot, sticky restart) while the user
        // still wants the tunnel — bring it up without a tap.
        val failedAgo = System.currentTimeMillis() - vpnPrefs.getLong(KEY_FAILED_AT, 0)
        if (vpnPrefs.getBoolean(KEY_WANTED, false) && TunnelState.state == VpnState.DISCONNECTED &&
            failedAgo > if (vpnPrefs.getBoolean(KEY_NEED_CODE, false)) NEED_CODE_BACKOFF_MS else RESUME_BACKOFF_MS
        ) {
            AppLog.i("wanted=true on create, resuming")
            startForegroundService(Intent(this, TunnelVpnService::class.java).setAction(ACTION_START))
        }
    }

    override fun onBind(intent: Intent?): IBinder? {
        // The system binds with SERVICE_INTERFACE to control the VPN; the UI
        // binds with ACTION_BIND for state updates.
        if (intent?.action == SERVICE_INTERFACE) return super.onBind(intent)
        return messenger.binder
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        AppLog.i("onStartCommand action=${intent?.action}")
        when (intent?.action) {
            ACTION_CRASH -> if (BuildConfig.DEBUG) Crash.smokeTest(intent.getStringExtra("kind") ?: "vpn")
            ACTION_STOP -> {
                vpnPrefs.edit().putBoolean(KEY_WANTED, false).apply()
                cancelRetry()
                stopTunnel(stopSelfWhenDone = true)
            }
            // null: START_STICKY restart after the process was killed.
            // SERVICE_INTERFACE: always-on VPN from system settings.
            ACTION_START, SERVICE_INTERFACE, null -> {
                val services = intent?.getStringArrayListExtra(EXTRA_SERVICES)?.toSet()
                    ?: vpnPrefs.getStringSet(KEY_SERVICES, null)
                    ?: Services.ALL.filter { it.defaultEnabled }.map { it.id }.toSet()
                val adBlock = if (intent?.hasExtra(EXTRA_ADBLOCK) == true) intent.getBooleanExtra(EXTRA_ADBLOCK, false)
                    else vpnPrefs.getBoolean(KEY_ADBLOCK, false)
                vpnPrefs.edit().putBoolean(KEY_WANTED, true).apply()
                cancelRetry()
                if (BuildConfig.DEBUG) testHooks(intent)
                startForegroundCompat()
                startTunnel(services, adBlock, intent?.getStringExtra(EXTRA_INVITE))
            }
        }
        return START_STICKY
    }

    // A START without the extras restores the real endpoints and CA: the
    // :vpn process, and Go's root pool with it, outlive a test run.
    private fun testHooks(intent: Intent?) = runCatching {
        collecting = intent?.getBooleanExtra(EXTRA_ANALYTICS, false) == true
        backend.setAnalytics(this, collecting)
        val ep = intent?.getStringExtra(EXTRA_ENDPOINT)?.split("|")?.takeIf { it.size == 4 }
        val rcList = intent?.getStringExtra(EXTRA_RC_ENDPOINTS)
        testEndpoint = ep?.let { (host, ip, port, path) -> Endpoint(host, ip, port.toInt(), path) }
        testRc = ep == null && rcList != null
        backend.trustCA(if (testStand) intent?.getStringExtra(EXTRA_CA).orEmpty() else "")
        if (ep != null) AppLog.i("test endpoint ${ep[0]} ${ep[1]}:${ep[2]}")
        if (testStand && intent?.getBooleanExtra(EXTRA_FORGET_CRED, false) == true) Credentials.clear(this, test = true)
        if (intent?.hasExtra(EXTRA_QUOTA_MB) == true) Quota.setLimitMb(this, intent.getLongExtra(EXTRA_QUOTA_MB, Quota.DEFAULT_MB))
        if (intent?.getBooleanExtra(EXTRA_QUOTA_RESET, false) == true) Quota.save(this, 0, Quota.today())
        if (testRc) {
            // Each test starts from its list's first, whatever worked in the last one.
            Endpoints.setWorking(this, null, test = true)
            // A running tunnel takes it like a fetch; a stopped one starts on it.
            applyConfig(RcValues(rcList.orEmpty(), 0, 0, ""), decrypt = { it })
        }
    }.onFailure { AppLog.e("test hooks", it) }

    private fun pushAll() {
        for (c in clients.toList()) push(c)
    }

    private fun push(to: Messenger) {
        try {
            to.send(VpnClient.snapshot(TunnelState.state, TunnelState.lastError, TunnelState.logLines(), TunnelState.connectedAt, TunnelState.waiting, RemoteConfig.versions(this), Usage(usedNow(), Quota.limit(this), Quota.today())))
        } catch (_: RemoteException) {
            clients -= to
        }
    }

    private fun startForegroundCompat() {
        val notification = createNotification()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.UPSIDE_DOWN_CAKE) {
            startForeground(NOTIFICATION_ID, notification, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        } else {
            startForeground(NOTIFICATION_ID, notification)
        }
    }

    private class NeedCode : Exception(ERR_NEED_CODE)
    // offline: no network with internet at all, so the retry is the network's
    // to trigger, not the user's.
    private class NoServer(val offline: Boolean) : Exception(ERR_NO_SERVER)

    /**
     * Credential to connect with: the stored one while it has more than a
     * day left, otherwise a fresh one from the issuer (Play Integrity first,
     * invite code second). A still-valid one survives a failed renewal.
     * Runs on the GoTunnel thread.
     */
    private fun ensureCredential(invite: String?): ByteArray {
        val now = System.currentTimeMillis() / 1000
        val cur = Credentials.load(this, testStand)
        val exp = cur?.let { backend.credExpires(it) } ?: 0L
        if (cur != null && exp > now + RENEW_BEFORE_S) return cur
        if (cur != null && exp <= now) Credentials.clear(this, testStand)

        val proofs = mutableListOf<Pair<Int, ByteArray>>()
        val nonce = ByteArray(32).also { SecureRandom().nextBytes(it) }
        backend.integrityToken(this, nonce)?.let { proofs += tunnel.Tunnel.KindIntegrity.toInt() to (nonce + it) }
        invite?.trim()?.takeIf { it.isNotEmpty() }?.let { proofs += tunnel.Tunnel.KindInvite.toInt() to it.toByteArray() }
        var refused = false
        for ((kind, proof) in proofs) {
            try {
                val cred = anyEndpoint("register", tryAll = true) { backend.register(it.addr, it.host, it.path, kind, proof) }
                Credentials.save(this, cred, testStand)
                AppLog.i("credential: registered kind=$kind, expires=${backend.credExpires(cred)}")
                TunnelState.log("Доступ получен")
                return cred
            } catch (e: Exception) {
                if (e.message?.contains("rejected") == true) refused = true
                AppLog.e("register kind=$kind failed", e, expected = expected(e))
            }
        }
        if (cur != null && exp > now) return cur
        // Offline, no proof reaches the issuer (Integrity itself needs the
        // network): asking for a code would send the user after the wrong fix.
        // Only an outright refusal is a code's business: an http error or an
        // odd status is the relay or nginx being broken, not the device.
        if (!refused) {
            val online = hasInternet()
            if (proofs.isNotEmpty() || !online) throw NoServer(offline = !online)
        }
        throw NeedCode()
    }

    private fun hasInternet() = getSystemService(ConnectivityManager::class.java).let { cm ->
        cm.getNetworkCapabilities(cm.activeNetwork)?.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED) == true
    }

    // Direct sockets must bypass the TUN, or a dial into a routed subnet
    // would come back into our own stack. RoutesStale: a listed site now
    // lives outside the routes, rebuild with the cache (rate-limited).
    private val host = object : tunnel.Host {
        override fun protect(fd: Long) = this@TunnelVpnService.protect(fd.toInt())
        override fun routesStale() { mainHandler.post { rebuildTunnel("routes stale") } }
        override fun dnsServers() = defaultDns?.second?.joinToString("\n") ?: ""
        override fun relayDown(down: Boolean) {
            relayDown = down
            mainHandler.post {
                if (down) freshDown = true else relayUp()
                updateWaiting()
            }
        }
        override fun quotaExceeded() { mainHandler.post { onQuotaExceeded() } }
        override fun quotaProgress() { mainHandler.post { onQuotaProgress() } }
    }
    @Volatile private var relayDown = false
    // Underlying networks with internet, to tell "no network" from "the
    // network blocks the server"; guarded by itself. Not VALIDATED: a
    // network that blocks the VDS often fails Android's check too.
    private val networks = mutableSetOf<Network>()
    // elapsedRealtime of the last network to appear / of the set going
    // empty. netLostAt is also stamped by watchNetwork on the tunnel thread:
    // the callback reports the current networks asynchronously, and the
    // grace covers that instead of a seed.
    private var netAppearedAt = 0L
    @Volatile private var netLostAt = 0L

    // Main thread only. Android knows about "no network" at once; the
    // relay's own verdict takes a breaker's worth of failures. Right after a
    // network appears the prober is still trying it: "server unavailable"
    // then would be a wrong diagnosis for a moment, so the old one stays.
    private fun updateWaiting() {
        mainHandler.removeCallbacks(recheckWaiting)
        val noNetwork = synchronized(networks) { networks.isEmpty() }
        val now = android.os.SystemClock.elapsedRealtime()
        val lostFor = now - netLostAt
        val appearedFor = now - netAppearedAt
        val w = when {
            TunnelState.state != VpnState.CONNECTED -> null
            noNetwork && lostFor >= NO_NETWORK_AFTER_MS -> Waiting.NO_NETWORK
            !relayDown -> null
            // A network gone for a moment, or none reported yet right after
            // the start: the old verdict stays until the grace is over.
            noNetwork -> TunnelState.waiting
            appearedFor < NEW_NETWORK_GRACE_MS -> TunnelState.waiting ?: Waiting.NO_SERVER
            else -> Waiting.NO_SERVER
        }
        when {
            noNetwork && lostFor < NO_NETWORK_AFTER_MS -> mainHandler.postDelayed(recheckWaiting, NO_NETWORK_AFTER_MS - lostFor)
            !noNetwork && appearedFor < NEW_NETWORK_GRACE_MS -> mainHandler.postDelayed(recheckWaiting, NEW_NETWORK_GRACE_MS - appearedFor)
        }
        // Go tells "down" once per endpoint: after a switch the stale
        // relayDown still says NO_SERVER, and only a fresh verdict judges the new one.
        val fresh = freshDown
        freshDown = false
        if (fresh) awaitingVerdict = false
        val was = TunnelState.waiting
        val changed = TunnelState.setWaiting(w)
        if (changed) AppLog.i("waiting: $w (no network=$noNetwork, relay down=$relayDown)")
        if (w == Waiting.NO_SERVER && (changed || fresh) && !awaitingVerdict) onNoServer()
        if (!changed) return
        // Not on a bare RelayDown: that one also comes with no network at all.
        mainHandler.removeCallbacks(noServerFetch)
        // Its run also checks for relay_down.
        if (w == Waiting.NO_SERVER) noServerFetch.run()
        getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID, createNotification())
        // Pings measured during the outage are errors; measure again, once
        // the network that brought the relay back has settled (a Wi-Fi join
        // often follows right after).
        if (was != null && w == null) mainHandler.postDelayed({ running?.let { (enabled, _) -> reprobe(enabled) } }, REPROBE_DELAY_MS)
    }
    private val recheckWaiting = Runnable { updateWaiting() }

    // Main thread only. roundFails: endpoints judged down since the relay
    // last answered; the whole list down pauses the round.
    private var freshDown = false
    private var awaitingVerdict = false
    private var roundFails = 0
    private var paused = false
    private var roundLevel = 0
    private var fetchLevel = 0
    // uptimeMillis the pending nextRound / noServerFetch is due at.
    private var nextRoundAt = 0L
    private var noServerFetchAt = 0L
    private val nextRound = Runnable {
        paused = false
        roundFails = 0
        if (TunnelState.waiting == Waiting.NO_SERVER && !awaitingVerdict) onNoServer()
    }

    private fun resetRound() {
        mainHandler.removeCallbacks(nextRound)
        paused = false
        roundFails = 0
    }

    // The servers may well be reachable from here: the pause is over. The
    // current one gets Go's probe on this network first; a next network
    // restarts the wait, so Wi-Fi↔LTE flapping costs one switch, not one each.
    // A round from a network change at most once per base pause: networks
    // that keep changing (metro, the edge of a Wi-Fi) would each buy one.
    private fun newNetwork() {
        resetFetchBackoff()
        if (SystemClock.uptimeMillis() - networkRoundAt < ROUND_PAUSE_MS) return
        resetRound()
        resetRoundBackoff()
        mainHandler.removeCallbacks(switchOnNewNetwork)
        mainHandler.postDelayed(switchOnNewNetwork, NEW_NETWORK_SWITCH_MS)
    }
    private var networkRoundAt = -ROUND_PAUSE_MS
    private val switchOnNewNetwork = Runnable {
        checkRelayDown()
        if (TunnelState.waiting == Waiting.NO_SERVER && !awaitingVerdict) {
            networkRoundAt = SystemClock.uptimeMillis()
            onNoServer()
        }
    }

    private fun resetBackoff() {
        resetRoundBackoff()
        resetFetchBackoff()
    }

    // Timers already due later than a base wait from now come forward to it.
    private fun resetRoundBackoff() {
        roundLevel = 0
        val now = SystemClock.uptimeMillis()
        if (paused && nextRoundAt > now + ROUND_PAUSE_MS) {
            mainHandler.removeCallbacks(nextRound)
            nextRoundAt = now + ROUND_PAUSE_MS
            mainHandler.postAtTime(nextRound, nextRoundAt)
        }
    }

    private fun resetFetchBackoff() {
        fetchLevel = 0
        val now = SystemClock.uptimeMillis()
        if (TunnelState.waiting == Waiting.NO_SERVER && noServerFetchAt > now + CONFIG_RETRY_MS) {
            mainHandler.removeCallbacks(noServerFetch)
            noServerFetchAt = now + CONFIG_RETRY_MS
            mainHandler.postAtTime(noServerFetch, noServerFetchAt)
        }
    }

    private fun relayUp() {
        mainHandler.removeCallbacks(switchOnNewNetwork)
        resetRound()
        resetBackoff()
        awaitingVerdict = false
        checkActive()
        val ep = current ?: return
        if (testEndpoint == null && ep != Endpoints.working(this, testRc)) Endpoints.setWorking(this, ep, testRc)
    }

    // The current endpoint is down with a network that has internet. The
    // instrumented tests' relay is the only one they have, and a single
    // endpoint is left to Go's own prober.
    private fun onNoServer() {
        val list = endpoints
        if (testEndpoint != null || paused || list.size < 2) return
        if (++roundFails >= list.size) {
            paused = true
            val wait = backoff(ROUND_PAUSE_MS, roundLevel++)
            AppLog.i("endpoint: all ${list.size} down, next round in ${wait / 1000} s")
            nextRoundAt = SystemClock.uptimeMillis() + wait
            mainHandler.postAtTime(nextRound, nextRoundAt)
            return
        }
        switchEndpoint(list[(list.indexOf(current) + 1) % list.size], "no server")
    }

    // Main thread only. New relay sessions go to [ep], open ones stay.
    private fun switchEndpoint(ep: Endpoint, why: String) {
        if (current == null || synchronized(lock) { stopping || rebuilding || TunnelState.state != VpnState.CONNECTED }) return
        try {
            backend.setEndpoint(ep, suspect = why != WHY_NEW_LIST)
        } catch (e: Exception) {
            AppLog.e("set endpoint", e)
            return
        }
        current = ep
        awaitingVerdict = true
        val list = endpoints
        val n = list.indexOf(ep) + 1
        AppLog.i("endpoint: switched to #$n of ${list.size} ($why)")
        // No ": " in these: MainActivity reads "<name>: <ping>" lines as probes.
        TunnelState.log(
            when {
                why == WHY_NEW_LIST -> "Получен новый список серверов"
                n == 1 -> "Сервер недоступен, возвращаюсь на основной"
                else -> "Сервер недоступен, переключаюсь на резервный ($n из ${list.size})"
            },
        )
    }

    // A list that differs from the cache is the operator's word: back to
    // its first. The same list again (every fetch on NO_SERVER) must not
    // pull the tunnel back onto a dead first one.
    private var missedChange = false

    private fun onNewList(changed: Boolean) {
        if (changed) Endpoints.setWorking(this, null, testRc)
        if (changed && (current == null || synchronized(lock) { stopping || rebuilding || TunnelState.state != VpnState.CONNECTED })) {
            // Connecting or rebuilding: the tunnel may still come up on the
            // old list's endpoint, and the check once it is up must know.
            missedChange = true
            return
        }
        val cur = current ?: return
        val list = endpoints
        if (!changed && cur in list) return
        resetRound()
        resetBackoff()
        val first = list.firstOrNull() ?: return
        if (first != cur) {
            switchEndpoint(first, WHY_NEW_LIST)
        } else if (TunnelState.waiting == Waiting.NO_SERVER && !awaitingVerdict) {
            // Already on it and known down: re-dialing it would only reset
            // Go's breaker, and no fresh verdict would ever come to move on.
            onNoServer()
        }
    }

    // Only a start blocked by min_version waits for it (on lock): until
    // it lands the cache is what we have. One at a time, a trigger during a
    // fetch is dropped.
    private val fetching = AtomicBoolean(false)
    private val periodicFetch = object : Runnable {
        override fun run() {
            fetchConfig("periodic")
            mainHandler.postDelayed(this, CONFIG_PERIOD_MS)
        }
    }
    private val noServerFetch = object : Runnable {
        override fun run() {
            checkRelayDown()
            fetchConfig("no server")
            noServerFetchAt = SystemClock.uptimeMillis() + backoff(CONFIG_RETRY_MS, fetchLevel++)
            mainHandler.postAtTime(this, noServerFetchAt)
        }
    }

    private fun stopFetching() {
        mainHandler.removeCallbacks(periodicFetch)
        mainHandler.removeCallbacks(noServerFetch)
    }

    private fun fetchConfig(why: String) {
        // The instrumented tests' relay: the real Firebase is none of their business.
        if (testStand) return
        if (!fetching.compareAndSet(false, true)) return
        Thread({
            try {
                applyConfig(backend.fetchConfig(), backend::decryptEndpoints)
            } catch (e: Throwable) {
                AppLog.e("remote config ($why) failed", e, expected = true) // offline, throttled
            } finally {
                fetching.set(false)
                synchronized(lock) { lock.notifyAll() }
            }
        }, "RemoteConfig").start()
    }

    private fun applyConfig(v: RcValues, decrypt: (String) -> String) {
        val before = Endpoints.cached(this, testRc)
        val versions = RemoteConfig.versions(this)
        val limit = Quota.limit(this)
        RemoteConfig.apply(this, v, decrypt, testRc)
        val after = Endpoints.cached(this, testRc)
        if (after != null) mainHandler.post { onNewList(changed = after != before) }
        if (RemoteConfig.versions(this) != versions) mainHandler.post { onNewVersions() }
        if (Quota.limit(this) != limit) mainHandler.post { onNewLimit() }
    }

    // Main thread only. A lower limit already used up stops the tunnel via
    // Go's QuotaExceeded; a raised one lets the next tap through.
    private fun onNewLimit() {
        AppLog.i("quota: limit ${Quota.limit(this)} bytes")
        if (countDay >= 0) backend.setQuota(backend.usage(), Quota.limit(this))
        synchronized(lock) {
            if (TunnelState.state == VpnState.ERROR && TunnelState.lastError == ERR_QUOTA && !Quota.exceeded(usedNow(), Quota.limit(this))) {
                TunnelState.set(VpnState.DISCONNECTED)
                getSystemService(NotificationManager::class.java).cancel(QUOTA_NOTIFICATION_ID)
            }
        }
        pushAll()
    }

    // The day Go's count began, -1 before any in this process. Go's counter
    // outlives Stop, so after it the count is still Go's to tell.
    @Volatile private var countDay = -1L

    private fun usedNow() = if (countDay == Quota.today()) backend.usage() else Quota.used(this)

    // Main thread only. Midnight has passed since the count began: today's starts at zero.
    private fun rollDay(): Boolean {
        val today = Quota.today()
        if (countDay < 0 || countDay == today) return false
        backend.setQuota(0, Quota.limit(this))
        countDay = today
        Quota.save(this, 0, today)
        AppLog.i("quota: new day")
        return true
    }

    // Main thread only. Go's step of traffic: to prefs, and to the UI with it.
    // A killed process loses at most a step of the day.
    private fun onQuotaProgress() {
        if (countDay < 0 || synchronized(lock) { stopping || TunnelState.state != VpnState.CONNECTING && TunnelState.state != VpnState.CONNECTED }) return
        if (rollDay()) return
        Quota.save(this, backend.usage(), countDay)
        pushAll()
    }

    // Main thread only. Go refuses the relay from now on; the VPN goes off
    // for the rest of the day, so the apps are not left with a dead route.
    private fun onQuotaExceeded() {
        if (synchronized(lock) { stopping || TunnelState.state != VpnState.CONNECTING && TunnelState.state != VpnState.CONNECTED }) return
        // Go's count may still be yesterday's: only traffic rolls the day.
        if (rollDay()) return
        // Raised meanwhile: Go judged by the old limit.
        if (!Quota.exceeded(backend.usage(), Quota.limit(this))) return
        daily("quota_exhausted")
        AppLog.i("quota: ${backend.usage()} of ${Quota.limit(this)} bytes used, stopping")
        vpnPrefs.edit().putBoolean(KEY_WANTED, false).apply()
        cancelRetry()
        notifyQuota()
        stopTunnel(stopSelfWhenDone = true, error = ERR_QUOTA)
    }

    // The tunnel goes down in the background, often with the app closed:
    // the notification is how the user learns why.
    private fun notifyQuota() {
        val nm = getSystemService(NotificationManager::class.java)
        // Not the VPN's quiet channel: this one has to be noticed.
        nm.createNotificationChannel(NotificationChannel(QUOTA_CHANNEL_ID, "Лимит трафика", NotificationManager.IMPORTANCE_DEFAULT))
        val open = android.app.PendingIntent.getActivity(
            this, 1,
            Intent(this, MainActivity::class.java).addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP),
            android.app.PendingIntent.FLAG_IMMUTABLE,
        )
        nm.notify(
            QUOTA_NOTIFICATION_ID,
            Notification.Builder(this, QUOTA_CHANNEL_ID)
                .setContentTitle(ERR_QUOTA)
                .setContentText("VPN выключен. Включите его снова после 00:00")
                .setSmallIcon(android.R.drawable.ic_menu_compass)
                .setContentIntent(open)
                .setAutoCancel(true)
                .build(),
        )
    }

    private fun onNewVersions() {
        // min_version lowered: the red banner goes by itself, the next tap connects.
        synchronized(lock) {
            if (TunnelState.state == VpnState.ERROR && TunnelState.lastError == ERR_UPDATE_REQUIRED && !RemoteConfig.versions(this).required) {
                AppLog.i("min_version ${RemoteConfig.versions(this).min} lets this version through")
                TunnelState.set(VpnState.DISCONNECTED)
            }
        }
        pushAll()
        // Only while in the foreground: a notify() after that leaves an orphan.
        if (TunnelState.state == VpnState.CONNECTING || TunnelState.state == VpnState.CONNECTED || retryOnNetwork != null) {
            getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID, createNotification())
        }
    }

    // The system's default network and its resolvers. protect()ed sockets
    // leave by it, and another network's resolver may refuse them (a
    // carrier's does a Wi-Fi address). Never the VPN's own: that one is our
    // fake DNS and would loop.
    @Volatile private var defaultDns: Pair<Network, List<String>>? = null
    // The default network and whether Android found internet on it.
    @Volatile private var defaultValidated: Pair<Network, Boolean>? = null
    // elapsedRealtime the default network last changed. A network up for
    // long (LTE beside a Wi-Fi) is new to the relay once it becomes the
    // default. Main thread only.
    private var defaultChangedAt = 0L
    private val mainHandler = Handler(Looper.getMainLooper())
    private val disconnectFallback = Runnable {
        synchronized(lock) {
            stopping = false
            if (TunnelState.state != VpnState.DISCONNECTED) TunnelState.set(VpnState.DISCONNECTED)
        }
    }
    private val deferredRebuild = Runnable { rebuildTunnel("deferred") }
    private val periodicRebuild = object : Runnable {
        override fun run() {
            rebuildTunnel("periodic")
            mainHandler.postDelayed(this, REBUILD_PERIOD_MS)
        }
    }
    // What the tunnel runs with, for rebuilds.
    private var running: Pair<List<Service>, ByteArray>? = null
    private var routesInTun: List<Route> = emptyList()
    @Volatile private var lastRebuildAt = 0L
    private var rebuilding = false

    private fun establishTun(enabled: List<Service>): ParcelFileDescriptor {
        val known = enabled.flatMap { it.allRoutes }
        val extra = RouteCache.routesFor(this, enabled, known)
        if (extra.isNotEmpty()) AppLog.i("routes: +${extra.size} from cache: ${extra.joinToString { it.address }}")
        routesInTun = known + extra
        return backend.establish(this, enabled, extra) ?: throw IllegalStateException(ERR_ESTABLISH)
    }

    private fun startGo(fd: ParcelFileDescriptor, cred: ByteArray, enabled: List<Service>) = anyEndpoint("start") {
        val domains = enabled.flatMap { s -> s.domains }.joinToString("\n")
        val routes = routesInTun.joinToString("\n") { r -> "${r.address}/${r.prefix}" }
        // Before start: Go's first verdict may reach relayUp while start still runs.
        current = it
        backend.start(fd, it.addr, it.host, it.path, cred, domains, routes, RouteCache.file(this).path, host, goLogger)
        running = enabled to cred
        AppLog.i("Go tunnel started via ${it.host}")
    }

    private fun startTunnel(serviceIds: Set<String>, adBlock: Boolean, invite: String? = null) {
        synchronized(lock) {
            // While stopping, the stop thread still owns tunnelThread and Go;
            // wanted stays true, so the next bind/boot brings it back.
            if (stopping || TunnelState.state == VpnState.CONNECTING || TunnelState.state == VpnState.CONNECTED) {
                AppLog.i("startTunnel: busy (state=${TunnelState.state}, stopping=$stopping), skip")
                return
            }
            TunnelState.clearLog()
            TunnelState.set(VpnState.CONNECTING)
        }
        AppLog.i("startTunnel services=$serviceIds adblock=$adBlock")
        // Before any fail: the SDK stamps it on every event after it. Its
        // share among the active also tells when the ad list starts to
        // block the hosts Analytics sends to.
        backend.userProperty(this, "adblock", if (adBlock) "on" else "off")

        val enabled = Services.ALL.filter { it.id in serviceIds }
        if (enabled.isEmpty()) {
            fail("Не выбран ни один сервис")
            return
        }
        val used = Quota.used(this)
        val limit = Quota.limit(this)
        if (Quota.exceeded(used, limit)) {
            AppLog.i("startTunnel: quota $used of $limit bytes used today")
            // Boot and always-on would only come back here.
            vpnPrefs.edit().putBoolean(KEY_WANTED, false).apply()
            fail(ERR_QUOTA)
            // A limit raised in RC lifts the refusal (onNewLimit); nothing else would fetch it.
            fetchConfig("quota")
            return
        }
        getSystemService(NotificationManager::class.java).cancel(QUOTA_NOTIFICATION_ID)
        // Persisted only when these routes really go in, so a null-intent
        // restart reproduces what was running, not what was asked last.
        vpnPrefs.edit().putStringSet(KEY_SERVICES, serviceIds).putBoolean(KEY_ADBLOCK, adBlock).remove(KEY_NEED_CODE).apply()
        current = null
        mainHandler.removeCallbacks(switchOnNewNetwork)
        networkRoundAt = -ROUND_PAUSE_MS
        resetRound()
        resetBackoff()
        missedChange = false
        freshDown = false
        awaitingVerdict = false
        stopFetching()
        fetchConfig("start")
        mainHandler.postDelayed(periodicFetch, CONFIG_PERIOD_MS)
        // Not against the instrumented tests' relay: RC's versions are not about it.
        val tooOld = !testStand && RemoteConfig.versions(this).required

        tunnelThread = Thread({
            var fd: ParcelFileDescriptor? = null
            try {
                // The cache may predate a lowered min_version: the start's own
                // fetch decides, and only when it fails or hangs, the cache.
                if (tooOld) {
                    // A STOP wakes it too: its thread joins this one.
                    val stopped = synchronized(lock) {
                        val until = System.nanoTime() + VERSION_FETCH_WAIT_MS * 1_000_000
                        var left = VERSION_FETCH_WAIT_MS
                        while (!stopping && fetching.get() && left > 0) {
                            lock.wait(left)
                            left = (until - System.nanoTime()) / 1_000_000
                        }
                        stopping
                    }
                    if (stopped) return@Thread
                    val versions = RemoteConfig.versions(this)
                    if (versions.required) {
                        AppLog.i("startTunnel: version ${BuildConfig.VERSION_CODE} below min_version ${versions.min}")
                        // No retries on bind or boot: only an update gets past this.
                        vpnPrefs.edit().putBoolean(KEY_WANTED, false).apply()
                        if (!synchronized(lock) { stopping }) fail(ERR_UPDATE_REQUIRED)
                        return@Thread
                    }
                }
                var cred = ensureCredential(invite)
                fd = establishTun(enabled)
                AppLog.i("TUN established, fd=${fd.fd}, starting Go tunnel")
                // countDay first: a fetch landing meanwhile sets Go itself,
                // or its limit is read here, or it lost to ours and goes again.
                val day = Quota.today()
                countDay = day
                val limitNow = Quota.limit(this)
                backend.setQuota(used, limitNow)
                if (Quota.limit(this) != limitNow) mainHandler.post { onNewLimit() }
                Quota.save(this, used, day)
                backend.setAdBlock(adBlock)
                try {
                    startGo(fd, cred, enabled)
                } catch (e: Exception) {
                    if (e.message?.contains("credential rejected") != true) throw e
                    // Revoked or expired on the server side: register again.
                    // The TUN must be down meanwhile, Integrity needs DNS.
                    AppLog.i("credential rejected by relay, registering again")
                    fd.close()
                    fd = null
                    Credentials.clear(this, testStand)
                    cred = ensureCredential(invite)
                    fd = establishTun(enabled)
                    startGo(fd, cred, enabled)
                }
                fd = null
                vpnPrefs.edit().remove(KEY_FAILED_AT).apply()
                val connected = synchronized(lock) {
                    // STOP arrived meanwhile: its thread joins us and stops Go.
                    if (stopping) return@synchronized false
                    TunnelState.set(VpnState.CONNECTED)
                    watchNetwork()
                    watchScreen()
                    mainHandler.post { updateWaiting() }
                    mainHandler.post { checkActive() }
                    // A list that landed while connecting could not switch.
                    mainHandler.post { onNewList(changed = missedChange.also { missedChange = false }) }
                    mainHandler.removeCallbacks(periodicRebuild)
                    mainHandler.postDelayed(periodicRebuild, REBUILD_PERIOD_MS)
                    probeThread = Thread({ probeServices(enabled) }, "Probe")
                    true
                }
                if (!connected) return@Thread
                TunnelState.log("Туннель поднят")
                probeThread?.start()
            } catch (e: Throwable) {
                AppLog.e("startTunnel failed", e, expected = expected(e))
                fd?.close()
                if (e is NeedCode) vpnPrefs.edit().putBoolean(KEY_NEED_CODE, true).apply()
                // STOP already running: it will report DISCONNECTED itself.
                if (synchronized(lock) { stopping }) return@Thread
                val msg = e.message ?: e.javaClass.simpleName
                when {
                    e is NoServer && e.offline -> retryWhenOnline(serviceIds, invite)
                    e is NoServer -> fail(msg, reason = "no_server")
                    e is NeedCode -> fail(msg, reason = "need_code")
                    else -> fail(msg)
                }
            }
        }, "GoTunnel").also { it.start() }
    }

    // The credential expired while offline (a week without the relay) and
    // renewing needs a network: an error the user cannot fix by tapping.
    // The service stays in the foreground, so the retry is not a start from
    // the background, and registers again once a network with internet is
    // up. A network that is up but blocks the issuer stays a plain error.
    private fun retryWhenOnline(services: Set<String>, invite: String?) = mainHandler.post {
        // STOP may have landed between the GoTunnel thread's check and this post.
        if (synchronized(lock) { stopping } || !vpnPrefs.getBoolean(KEY_WANTED, false)) return@post
        cancelRetry()
        vpnPrefs.edit().putLong(KEY_FAILED_AT, System.currentTimeMillis()).apply()
        TunnelState.set(VpnState.ERROR, ERR_OFFLINE)
        daily("vpn_failed", "offline")
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                mainHandler.post {
                    if (retryOnNetwork !== this) return@post
                    cancelRetry()
                    // A network back is the retry, at once. The callback also
                    // fires for a network that already matches while
                    // hasInternet() judges only the default one: that round
                    // would repeat itself, so the second and later ones keep
                    // the backoff (Integrity + a handshake per proof each).
                    val now = SystemClock.elapsedRealtime()
                    val wait = (lastRetryAt + RESUME_BACKOFF_MS - now).coerceAtLeast(0)
                    lastRetryAt = now + wait
                    retryRunnable = Runnable {
                        retryRunnable = null
                        if (!vpnPrefs.getBoolean(KEY_WANTED, false) || TunnelState.lastError != ERR_OFFLINE) return@Runnable
                        AppLog.i("network available: $network, renewing the credential")
                        startForegroundCompat()
                        startTunnel(services, vpnPrefs.getBoolean(KEY_ADBLOCK, false), invite)
                    }.also { mainHandler.postDelayed(it, wait) }
                }
            }
        }
        getSystemService(ConnectivityManager::class.java).registerNetworkCallback(
            NetworkRequest.Builder()
                .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
                .addCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
                .build(),
            cb
        )
        retryOnNetwork = cb
        getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID, createNotification())
    }
    // Main thread only.
    private var retryOnNetwork: ConnectivityManager.NetworkCallback? = null
    private var retryRunnable: Runnable? = null
    private var lastRetryAt = -RESUME_BACKOFF_MS

    private fun cancelRetry() {
        retryOnNetwork?.let { getSystemService(ConnectivityManager::class.java).unregisterNetworkCallback(it) }
        retryOnNetwork = null
        retryRunnable?.let { mainHandler.removeCallbacks(it) }
        retryRunnable = null
    }

    // Underlying network changed (wifi <-> LTE). A lost network takes every
    // relay connection with it: cut them so apps reconnect right away instead
    // of hanging on TCP retransmits. A new one only invalidates idle DoH.
    private fun watchNetwork() {
        val cm = getSystemService(ConnectivityManager::class.java)
        val cb = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                AppLog.i("network available: $network")
                synchronized(networks) { networks += network }
                backend.networkChanged()
                mainHandler.post {
                    netAppearedAt = android.os.SystemClock.elapsedRealtime()
                    newNetwork()
                    updateWaiting()
                    checkActive()
                }
            }

            override fun onLost(network: Network) {
                val none = synchronized(networks) { networks -= network; networks.isEmpty() }
                AppLog.i("network lost: $network, cutting relay connections")
                backend.networkLost()
                mainHandler.post {
                    if (none) netLostAt = android.os.SystemClock.elapsedRealtime()
                    updateWaiting()
                }
            }
        }
        netLostAt = SystemClock.elapsedRealtime()
        // Default request carries NOT_VPN, so our own tun0 never triggers this.
        cm.registerNetworkCallback(
            NetworkRequest.Builder().addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET).build(),
            cb
        )
        // The callback is the only source of `networks`: allNetworks lists
        // ones it never reports (restricted carrier networks), and those
        // would never be lost, so airplane mode would go unnoticed.
        netCallback = cb

        val def = object : ConnectivityManager.NetworkCallback() {
            override fun onLinkPropertiesChanged(network: Network, lp: LinkProperties) {
                val dns = network to lp.dnsServers.mapNotNull { it.hostAddress }
                // Fires on every link change (signal, addresses), mostly with the same DNS.
                if (dns == defaultDns) return
                defaultDns = dns
                AppLog.i("default network $network dns: ${dns.second}")
                // The old resolvers' answers may be local to their network.
                backend.dnsChanged()
                mainHandler.post { checkActive() }
            }

            // A request's callback hears a new default network without a lost
            // for the old one: the old one's verdict must not carry over.
            override fun onAvailable(network: Network) {
                if (defaultValidated?.first != network) defaultValidated = null
                defaultChangedAt = SystemClock.elapsedRealtime()
            }

            // On every change: the validation may also go.
            override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) {
                val validated = network to caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_VALIDATED)
                if (validated == defaultValidated) return
                defaultValidated = validated
                if (validated.second) mainHandler.post { checkRelayDown() }
            }

            override fun onLost(network: Network) {
                if (defaultDns?.first == network) defaultDns = null
                if (defaultValidated?.first == network) defaultValidated = null
            }
        }
        // The same capabilities as the system's default request, so the best
        // match is the default network. Only listening from S; below, a
        // request that follows the default one keeps nothing else up.
        val request = NetworkRequest.Builder().addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET).build()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.S) cm.registerBestMatchingNetworkCallback(request, def, mainHandler)
        else cm.requestNetwork(request, def, mainHandler)
        defaultCallback = def
    }

    // fail() on a tunnel thread may race stopTunnel() on main: whoever takes
    // the callbacks (under lock, as watchNetwork sets them) lets them go, the
    // other finds none. A second unregister throws.
    private fun unwatchNetwork() {
        val (net, def) = synchronized(lock) { (netCallback to defaultCallback).also { netCallback = null; defaultCallback = null } }
        val cm = getSystemService(ConnectivityManager::class.java)
        net?.let(cm::unregisterNetworkCallback)
        def?.let(cm::unregisterNetworkCallback)
        defaultDns = null
        defaultValidated = null
        synchronized(networks) { networks.clear() }
    }

    private val channelReceiver = object : BroadcastReceiver() {
        override fun onReceive(ctx: Context, intent: Intent) {
            if (intent.getStringExtra(NotificationManager.EXTRA_NOTIFICATION_CHANNEL_ID) != CHANNEL_ID) return
            if (intent.getBooleanExtra(NotificationManager.EXTRA_BLOCKED_STATE, true)) return
            if (TunnelState.state == VpnState.CONNECTING || TunnelState.state == VpnState.CONNECTED || retryOnNetwork != null) {
                getSystemService(NotificationManager::class.java).notify(NOTIFICATION_ID, createNotification())
            }
        }
    }

    // Screen on and off: the day's first relay session usually comes in
    // between (unlocked, opened Telegram). The screen woke the phone anyway.
    private var screenReceiver: BroadcastReceiver? = null

    private fun watchScreen() {
        val r = object : BroadcastReceiver() {
            override fun onReceive(ctx: Context, intent: Intent) { mainHandler.post { checkActive() } }
        }
        val filter = IntentFilter(Intent.ACTION_SCREEN_ON).apply { addAction(Intent.ACTION_SCREEN_OFF) }
        ContextCompat.registerReceiver(this, r, filter, ContextCompat.RECEIVER_NOT_EXPORTED)
        screenReceiver = r
    }

    // Taken as in unwatchNetwork. fail() also comes before CONNECTED, with none.
    private fun unwatchScreen() {
        synchronized(lock) { screenReceiver.also { screenReceiver = null } }?.let { unregisterReceiver(it) }
    }

    // Always in release; in debug as EXTRA_ANALYTICS says. Off, a day must
    // not be marked: the event would never go, and a later run with the
    // extra would find the day taken.
    private var collecting = !BuildConfig.DEBUG

    private fun eventKey(name: String, reason: String?) = listOfNotNull("analytics", name, reason).joinToString(".")

    // Main thread only. Each event at most once a local day (per reason):
    // the reports count devices a day, and starts repeat themselves (resume
    // on every bind, the offline retry, always-on after the quota).
    private fun daily(name: String, reason: String? = null) {
        if (!collecting) return
        val key = eventKey(name, reason)
        val today = Quota.today()
        if (vpnPrefs.getLong(key, -1) == today) return
        vpnPrefs.edit().putLong(key, today).apply()
        backend.event(this, name, reason?.let { mapOf("reason" to it) }.orEmpty())
    }

    // Main thread only. vpn_active: the relay let a session in today, not
    // merely a tunnel up (white lists, a dead relay). Not by relayDown: that
    // one stays "up" through a night until a breaker's worth of failures.
    private fun checkActive() {
        if (!collecting || TunnelState.state != VpnState.CONNECTED || vpnPrefs.getLong(eventKey("vpn_active", null), -1) == Quota.today()) return
        val ok = backend.lastRelayOk()
        if (ok > 0 && Instant.ofEpochSecond(ok).atZone(ZoneId.systemDefault()).toLocalDate().toEpochDay() == Quota.today()) daily("vpn_active")
    }

    // Main thread only. relay_down: internet works and the VDS does not
    // answer. NO_SERVER alone also comes on a Wi-Fi without internet or
    // behind a captive portal; Android's validation tells those apart. And
    // the verdict counts only past the time a new network takes to judge
    // the relay (updateWaiting keeps the old one meanwhile).
    private fun checkRelayDown() {
        val now = SystemClock.elapsedRealtime()
        if (TunnelState.waiting == Waiting.NO_SERVER && defaultValidated?.second == true &&
            now - netAppearedAt >= NEW_NETWORK_SWITCH_MS && now - defaultChangedAt >= NEW_NETWORK_SWITCH_MS
        ) daily("vpn_failed", "relay_down")
    }

    private fun fail(msg: String, reason: String = reasonOf(msg)) {
        vpnPrefs.edit().putLong(KEY_FAILED_AT, System.currentTimeMillis()).apply()
        unwatchNetwork()
        unwatchScreen()
        mainHandler.removeCallbacks(periodicRebuild)
        mainHandler.removeCallbacks(deferredRebuild)
        stopFetching()
        mainHandler.removeCallbacks(switchOnNewNetwork)
        // Off the main thread too: the round's flags are the main thread's.
        mainHandler.post { resetRound() }
        current = null
        relayDown = false
        TunnelState.set(VpnState.ERROR, msg)
        mainHandler.post { daily("vpn_failed", reason) }
        stopForeground(STOP_FOREGROUND_REMOVE)
        cancelLateNotification()
        stopSelf()
    }

    // updateWaiting may have been past its CONNECTED check on the main
    // thread when the service went down; a late notify() would leave an
    // orphan. Queued after it, this removes that one.
    private fun cancelLateNotification() {
        mainHandler.post { getSystemService(NotificationManager::class.java).cancel(NOTIFICATION_ID) }
    }

    private fun reprobe(enabled: List<Service>) {
        synchronized(lock) {
            if (probeThread?.isAlive == true || TunnelState.state != VpnState.CONNECTED) return
            probeThread = Thread({ probeServices(enabled) }, "Probe").also { it.start() }
        }
    }

    // Runs on its own thread after CONNECTED. App's own traffic goes through
    // the TUN like everyone else's, so this measures the real path. stopTunnel
    // detaches the thread (probeThread = null); results after that are dropped.
    private fun probeServices(enabled: List<Service>) {
        val me = Thread.currentThread()
        val pool = Executors.newFixedThreadPool(enabled.size.coerceAtMost(6))
        try {
            val latch = java.util.concurrent.CountDownLatch(enabled.size)
            for (svc in enabled) {
                pool.submit {
                    try {
                        val t0 = System.nanoTime()
                        val result = try {
                            backend.probe(svc.probeUrl)
                            "${(System.nanoTime() - t0) / 1_000_000} мс"
                        } catch (e: Exception) {
                            "ошибка: ${e.javaClass.simpleName}"
                        }
                        if (probeThread === me) TunnelState.log("${svc.title}: $result")
                    } finally {
                        latch.countDown()
                    }
                }
            }
            try { latch.await(15, TimeUnit.SECONDS) } catch (_: InterruptedException) {}
        } finally {
            pool.shutdownNow()
        }
    }

    /**
     * Rebuild the TUN with the routes the cache has learned. Runs on the main
     * thread's request, does the work on its own thread; the state stays
     * CONNECTED, sessions through the relay are cut and apps reconnect.
     */
    private fun rebuildTunnel(why: String) {
        val (enabled, cred) = synchronized(lock) {
            if (stopping || rebuilding || TunnelState.state != VpnState.CONNECTED) return
            val wait = REBUILD_MIN_GAP_MS - (System.currentTimeMillis() - lastRebuildAt)
            if (wait > 0) {
                // Not lost: retried once the gap has passed.
                AppLog.i("rebuild ($why): too soon, retry in ${wait / 1000} s")
                mainHandler.removeCallbacks(deferredRebuild)
                mainHandler.postDelayed(deferredRebuild, wait)
                return
            }
            val known = enabled().flatMap { it.allRoutes }
            if (RouteCache.routesFor(this, enabled(), known).none { it !in routesInTun }) {
                AppLog.i("rebuild ($why): nothing new in cache, skip")
                return
            }
            rebuilding = true
            lastRebuildAt = System.currentTimeMillis()
            running ?: return
        }
        AppLog.i("rebuild ($why): restarting TUN with cached routes")
        val t = Thread({
            var fd: ParcelFileDescriptor? = null
            try {
                // New TUN first: Android swaps the interface in place, so there
                // is no moment without a VPN. Only then the old stack goes.
                fd = establishTun(enabled)
                backend.stop()
                startGo(fd, cred, enabled)
                fd = null
                TunnelState.log("Маршруты обновлены")
            } catch (e: Throwable) {
                AppLog.e("rebuild failed", e)
                fd?.close()
                if (!synchronized(lock) { stopping }) fail(e.message ?: e.javaClass.simpleName, reason = "rebuild")
            } finally {
                synchronized(lock) { rebuilding = false }
                // A list that landed meanwhile could not switch.
                mainHandler.post { onNewList(changed = missedChange.also { missedChange = false }) }
            }
        }, "GoRebuild")
        tunnelThread = t
        t.start()
    }

    private fun enabled(): List<Service> = running?.first ?: emptyList()

    /** [error]: the state to end in instead of DISCONNECTED. */
    private fun stopTunnel(stopSelfWhenDone: Boolean = false, error: String? = null) {
        synchronized(lock) {
            if (stopping) {
                AppLog.i("stopTunnel: already stopping, skip")
                return
            }
            // Nothing runs: the DISCONNECTING dance would hold `stopping` for
            // the fallback's 3 s and swallow a START right behind this STOP.
            if (TunnelState.state == VpnState.DISCONNECTED) {
                AppLog.i("stopTunnel: nothing to stop")
                if (stopSelfWhenDone) {
                    stopForeground(STOP_FOREGROUND_REMOVE)
                    stopSelf()
                }
                return
            }
            stopping = true
            lock.notifyAll()
            TunnelState.clearLog()
            TunnelState.set(VpnState.DISCONNECTING)
            unwatchNetwork()
            unwatchScreen()
            mainHandler.removeCallbacks(periodicRebuild)
            mainHandler.removeCallbacks(deferredRebuild)
            stopFetching()
            mainHandler.removeCallbacks(switchOnNewNetwork)
            resetRound()
            current = null
            probeThread?.interrupt()
            probeThread = null
        }
        AppLog.i("stopTunnel")
        val starting = tunnelThread
        Thread({
            // Tunnel.start's relay probe dials with a 10 s timeout; Go must
            // not be stopped underneath it or the stack outlives us.
            starting?.join(15_000)
            tunnelThread = null
            try {
                backend.stop()
            } catch (e: Throwable) {
                AppLog.e("Tunnel.stop crashed", e)
            }
            AppLog.i("stopTunnel done")
            relayDown = false
            if (countDay >= 0) Quota.save(this, backend.usage(), countDay)
            if (stopSelfWhenDone) {
                stopForeground(STOP_FOREGROUND_REMOVE)
                cancelLateNotification()
                if (error != null) {
                    synchronized(lock) {
                        stopping = false
                        TunnelState.set(VpnState.ERROR, error)
                    }
                } else {
                    mainHandler.postDelayed(disconnectFallback, 3000)
                }
                stopSelf()
            } else {
                synchronized(lock) {
                    stopping = false
                    TunnelState.set(VpnState.DISCONNECTED)
                }
            }
        }, "TunnelStop").start()
    }

    override fun onDestroy() {
        unregisterReceiver(channelReceiver)
        mainHandler.removeCallbacks(disconnectFallback)
        stopFetching()
        cancelRetry()
        synchronized(lock) {
            stopping = false
            if (TunnelState.state != VpnState.DISCONNECTED) {
                TunnelState.set(VpnState.DISCONNECTED)
            }
        }
        TunnelState.unsubscribe(stateListener)
        TunnelState.unsubscribeLog(logListener)
        super.onDestroy()
    }

    override fun onRevoke() {
        // Another VPN took over or the user pulled permission: don't fight it.
        vpnPrefs.edit().putBoolean(KEY_WANTED, false).apply()
        stopTunnel(stopSelfWhenDone = true)
    }

    private fun createNotification(): Notification {
        createChannel(this)
        val stopIntent = android.app.PendingIntent.getService(
            this, 0,
            Intent(this, TunnelVpnService::class.java).setAction(ACTION_STOP),
            android.app.PendingIntent.FLAG_IMMUTABLE,
        )
        val b = Notification.Builder(this, CHANNEL_ID)
            .setContentTitle("Split VPN")
            .setContentText(
                when {
                    retryOnNetwork != null -> "Нет сети · подключится автоматически"
                    TunnelState.waiting == Waiting.NO_NETWORK -> "Нет сети · подключится автоматически"
                    TunnelState.waiting == Waiting.NO_SERVER -> "Сервер недоступен · подключится автоматически"
                    else -> "VPN включён"
                }
            )
            .setSmallIcon(android.R.drawable.ic_menu_compass)
            .addAction(Notification.Action.Builder(null, "Выключить", stopIntent).build())
        if (RemoteConfig.versions(this).available) {
            // The download and the installer's dialog need an activity: the UI process does it.
            val update = android.app.PendingIntent.getActivity(
                this, 0,
                Intent(this, MainActivity::class.java)
                    .putExtra(MainActivity.EXTRA_UPDATE, true)
                    .addFlags(Intent.FLAG_ACTIVITY_NEW_TASK or Intent.FLAG_ACTIVITY_SINGLE_TOP),
                android.app.PendingIntent.FLAG_IMMUTABLE or android.app.PendingIntent.FLAG_UPDATE_CURRENT,
            )
            b.setSubText("Доступно обновление")
                .addAction(Notification.Action.Builder(null, "Обновить", update).build())
        }
        return b.build()
    }
}
