package org.duckdns.splitvpn

import android.content.Context
import android.net.VpnService
import android.os.ParcelFileDescriptor
import java.io.File
import java.util.concurrent.CopyOnWriteArrayList
import java.util.concurrent.CountDownLatch

internal class FakeBackend : Backend {
    val events = CopyOnWriteArrayList<String>()
    val startGate = CountDownLatch(1)
    val probeGate = CountDownLatch(1)
    var blockStart = false
    var blockProbe = false
    var startError: Throwable? = null
    /** What Go's own probe tells the host during Start. */
    var relayDownAtStart: Boolean? = null
    var pfd: ParcelFileDescriptor? = null

    var token: ByteArray? = null
    var registerResult: ByteArray? = null
    var registerError: Throwable? = null
    val registered = CopyOnWriteArrayList<Pair<Int, ByteArray>>()
    var startedWith: ByteArray? = null
    var startedDomains: String? = null
    var startedSni: String? = null

    val establishedExtra = CopyOnWriteArrayList<List<Route>>()
    var startedHost: tunnel.Host? = null
    var startedRoutes: String? = null

    override fun establish(service: VpnService, enabled: List<Service>, extra: List<Route>): ParcelFileDescriptor {
        establishedExtra += extra
        return pfd ?: ParcelFileDescriptor.open(File.createTempFile("tun", null), ParcelFileDescriptor.MODE_READ_ONLY)
    }

    override fun start(fd: ParcelFileDescriptor, addr: String, sni: String, path: String, cred: ByteArray, domains: String, routes: String, cacheFile: String, desyncFile: String, host: tunnel.Host, logger: tunnel.Logger) {
        startedDomains = domains
        startedAdBlock = adBlock
        startedDirect = direct
        startedStrategies = strategies
        startedDesyncFile = desyncFile
        startedSni = sni
        startedRoutes = routes
        startedHost = host
        relayDownAtStart?.let(host::relayDown)
        if (blockStart) startGate.await()
        startError?.let { throw it }
        if (rejectCreds.remove(cred.toList())) throw Exception("credential rejected")
        startedWith = cred
        events += "start"
    }
    /** Credentials the fake relay answers 404 to, once each. */
    val rejectCreds = mutableSetOf<List<Byte>>()

    override fun stop() { events += "stop" }
    val endpointsSet = CopyOnWriteArrayList<TunnelVpnService.Endpoint>()
    val suspects = CopyOnWriteArrayList<Boolean>()
    override fun setEndpoint(ep: TunnelVpnService.Endpoint, suspect: Boolean) { endpointsSet += ep; suspects += suspect }
    override fun networkChanged() { events += "networkChanged" }
    override fun networkLost() { events += "networkLost" }
    override fun dnsChanged() { events += "dnsChanged" }
    override fun probe(url: String) { if (blockProbe) probeGate.await() }

    override fun integrityToken(ctx: Context, nonce: ByteArray) = token
    override fun trustCA(pem: String) {}
    var endpoints = listOf(TunnelVpnService.Endpoint("cover.example.org", "203.0.113.10", 443, "/test"))
    override fun endpoints() = endpoints

    /** "blob:<json>" opens to <json>, anything else does not decrypt. */
    override fun decryptEndpoints(blob: String) = blob.removePrefix("blob:").takeIf { it != blob } ?: throw Exception("endpoints: decryption failed")
    var config = RcValues("", 0, 0, "")
    var configError: Throwable? = null
    val configGate = CountDownLatch(1)
    var blockConfig = false
    val fetches = java.util.concurrent.atomic.AtomicInteger()
    override fun fetchConfig(): RcValues {
        fetches.incrementAndGet()
        if (blockConfig) configGate.await()
        configError?.let { throw it }
        return config
    }
    /** Go's counter: what setQuota set plus what the test adds. */
    @Volatile var used = 0L
    @Volatile var limit = 0L
    val quotasSet = CopyOnWriteArrayList<Pair<Long, Long>>()
    override fun setQuota(used: Long, limit: Long) { this.used = used; this.limit = limit; quotasSet += used to limit }
    override fun usage() = used
    @Volatile var adBlock: Boolean? = null
    /** What setAdBlock had set when start ran. */
    var startedAdBlock: Boolean? = null
    override fun setAdBlock(on: Boolean) { adBlock = on }
    @Volatile var direct: Boolean? = null
    /** What setDirect, setStrategies had set when start ran. */
    var startedDirect: Boolean? = null
    var startedStrategies: String? = null
    var startedDesyncFile: String? = null
    @Volatile var strategies: String? = null
    /** What Go answers setStrategies: the entries it dropped. */
    var dropped = ""
    override fun setDirect(on: Boolean) { direct = on }
    override fun setStrategies(json: String): String { strategies = json; return dropped }
    val netKeys = CopyOnWriteArrayList<String>()
    override fun setNetKey(key: String) { netKeys += key }
    /** Go's LastRelayOK, unix seconds. */
    @Volatile var lastRelayOk = 0L
    override fun lastRelayOk() = lastRelayOk
    /** What went to Analytics: event names with their params. */
    val analytics = CopyOnWriteArrayList<Pair<String, Map<String, String>>>()
    val userProperties = java.util.concurrent.ConcurrentHashMap<String, String>()
    @Volatile var analyticsOn: Boolean? = null
    override fun event(ctx: Context, name: String, params: Map<String, String>) { analytics += name to params }
    override fun userProperty(ctx: Context, name: String, value: String) { userProperties[name] = value }
    override fun setAnalytics(ctx: Context, on: Boolean) { analyticsOn = on }
    override fun register(addr: String, sni: String, path: String, kind: Int, proof: ByteArray): ByteArray {
        registered += kind to proof
        registerError?.let { throw it }
        return registerResult ?: throw IllegalStateException("no registerResult")
    }
    override fun credExpires(cred: ByteArray): Long =
        if (cred.size == 77) ((cred[9].toLong() and 0xff) shl 24) or ((cred[10].toLong() and 0xff) shl 16) or
            ((cred[11].toLong() and 0xff) shl 8) or (cred[12].toLong() and 0xff) else 0

    fun release() { startGate.countDown(); probeGate.countDown(); configGate.countDown() }
}

/** A 77-byte credential expiring at [expires] (unix seconds). */
internal fun fakeCred(expires: Long, kind: Int = 1): ByteArray = ByteArray(77).also {
    it[0] = kind.toByte()
    it[9] = (expires shr 24).toByte(); it[10] = (expires shr 16).toByte()
    it[11] = (expires shr 8).toByte(); it[12] = expires.toByte()
}

/**
 * The service's own threads, done: once released from a FakeBackend gate
 * they still set TunnelState and prefs, which must not land in the next test.
 */
internal fun awaitServiceThreads() {
    val names = setOf("GoTunnel", "GoRebuild", "TunnelStop", "RemoteConfig", "Probe")
    val deadline = System.currentTimeMillis() + 20_000
    while (Thread.getAllStackTraces().keys.any { it.isAlive && it.name in names } && System.currentTimeMillis() < deadline) {
        Thread.sleep(20)
        org.robolectric.shadows.ShadowLooper.idleMainLooper(100, java.util.concurrent.TimeUnit.MILLISECONDS)
    }
}
