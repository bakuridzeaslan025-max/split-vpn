import java.net.InetAddress
import java.util.Base64
import java.util.Properties

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("com.google.gms.google-services")
    id("com.google.firebase.crashlytics")
}

// Google Cloud project number for Play Integrity; 0 disables it (dev builds
// register with an invite code only). local.properties: integrity.project=123456
val localProps = Properties().apply {
    rootProject.file("local.properties").takeIf { it.exists() }?.inputStream()?.use { load(it) }
}
val integrityProject = localProps.getProperty("integrity.project")?.trim()?.takeIf { it.isNotEmpty() } ?: "0"
// Default relay endpoints, baked in encrypted so the addresses are not a grep
// of the APK away. local.properties:
//   rc.key=<64 hex>   AES-256 key, the same one `make tunnel` gives Go
//   vds.endpoints=host|ip|port|path;host|ip|port|path   first one is primary
// Either missing: the list is empty and the app cannot connect, so only a
// debug build goes on (with a warning); a release one fails.
val endpointsBlob = encryptEndpoints(
    localProps.getProperty("rc.key")?.trim().orEmpty(),
    localProps.getProperty("vds.endpoints")?.trim().orEmpty(),
)
if (localProps.getProperty("vpn.path") != null) {
    logger.warn("local.properties: vpn.path is no longer read, the path goes into vds.endpoints")
}
// Checked when a release build runs, not at configuration: a debug build
// configures the release variant too.
val noEndpoints = endpointsBlob.isEmpty()
tasks.matching { it.name == "preReleaseBuild" }.configureEach {
    doFirst { if (noEndpoints) throw GradleException("release without endpoints: set rc.key and vds.endpoints in local.properties") }
}
// Upload key for Play (gitignored): keystore.file, keystore.pass, key.alias, key.pass
val keystoreFile = localProps.getProperty("keystore.file")?.takeIf { it.isNotEmpty() }

/**
 * Base64 of [1 key version][12 nonce][AES-GCM ciphertext+tag], AAD
 * "endpoints|v1"; opened by DecryptEndpoints in android/tunnel/endpoints.go.
 * The nonce is an HMAC of the plaintext rather than random: the same list
 * gives the same BuildConfig, so every build does not recompile the app,
 * and a different list still never reuses a nonce. The HMAC gets its own
 * key derived from rc.key, not the AES key itself.
 * Checks the same as the Go side: a list Go refuses is no list at all.
 */
fun encryptEndpoints(keyHex: String, spec: String): String {
    if (keyHex.isEmpty() || spec.isEmpty()) {
        logger.warn("local.properties: rc.key or vds.endpoints not set, the app gets no endpoints")
        return ""
    }
    require(Regex("[0-9a-fA-F]{64}").matches(keyHex)) { "rc.key: expected 64 hex chars" }
    val key = keyHex.chunked(2).map { it.toInt(16).toByte() }.toByteArray()
    val json = spec.split(';').filter { it.isNotBlank() }.joinToString(",", "[", "]") { e ->
        val p = e.trim().split('|')
        require(
            p.size == 4 && p.none { it.isEmpty() || '"' in it || '\\' in it } && isIpLiteral(p[1]) &&
                p[2].toIntOrNull() in 1..65535 && p[3].startsWith("/") && p[3].length >= 8,
        ) { "vds.endpoints: expected host|ip|port|path;… (ip a literal, port 1..65535, path /… of 8+ chars)" }
        """{"host":"${p[0]}","ip":"${p[1]}","port":${p[2]},"path":"${p[3]}"}"""
    }.toByteArray()
    val aad = "endpoints|v1".toByteArray()
    fun hmac(k: ByteArray, data: ByteArray) = javax.crypto.Mac.getInstance("HmacSHA256")
        .apply { init(javax.crypto.spec.SecretKeySpec(k, "HmacSHA256")) }.doFinal(data)
    val nonce = hmac(hmac(key, "nonce".toByteArray()), aad + json).copyOf(12)
    val gcm = javax.crypto.Cipher.getInstance("AES/GCM/NoPadding")
    gcm.init(javax.crypto.Cipher.ENCRYPT_MODE, javax.crypto.spec.SecretKeySpec(key, "AES"), javax.crypto.spec.GCMParameterSpec(128, nonce))
    gcm.updateAAD(aad)
    return Base64.getEncoder().encodeToString(byteArrayOf(1) + nonce + gcm.doFinal(json))
}

// Never a DNS lookup: in brackets InetAddress takes only an IPv6 literal.
fun isIpLiteral(ip: String) =
    Regex("""(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})""").matchEntire(ip)?.groupValues?.drop(1)?.all { it.toInt() <= 255 }
        ?: (':' in ip && runCatching { InetAddress.getByName("[$ip]") }.isSuccess)

android {
    namespace = "org.duckdns.splitvpn"
    compileSdk = 36

    defaultConfig {
        applicationId = "org.newvpn" // Play Console package; Kotlin namespace stays
        minSdk = 26
        targetSdk = 36
        versionCode = 120 // an earlier internal release in Play took 68
        versionName = "0.9.1"
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
        buildConfigField("long", "INTEGRITY_PROJECT", "${integrityProject}L")
        buildConfigField("String", "ENDPOINTS", "\"$endpointsBlob\"")
    }

    buildFeatures.buildConfig = true

    signingConfigs {
        create("release") {
            if (keystoreFile != null) {
                storeFile = rootProject.file(keystoreFile)
                storePassword = localProps.getProperty("keystore.pass")
                keyAlias = localProps.getProperty("key.alias")
                keyPassword = localProps.getProperty("key.pass")
            }
        }
    }

    buildTypes {
        debug {
            versionNameSuffix = "-debug"
            manifestPlaceholders["analyticsEnabled"] = "false"
            if (keystoreFile != null) signingConfig = signingConfigs.getByName("release")
        }
        release {
            // T8: shrink dex and resources. gomobile ships keep rules for go.** and
            // tunnel.** inside the AAR (JNI looks them up by name).
            isMinifyEnabled = true
            isShrinkResources = true
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
            manifestPlaceholders["analyticsEnabled"] = "true"
            if (keystoreFile != null) signingConfig = signingConfigs.getByName("release")
        }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    testOptions.unitTests.isIncludeAndroidResources = true
}

dependencies {
    implementation(files("libs/tunnel.aar"))
    implementation("androidx.core:core:1.13.1")
    implementation("com.google.zxing:core:3.5.3")
    implementation("com.google.android.play:integrity:1.4.0")
    implementation(platform("com.google.firebase:firebase-bom:33.8.0")) // config 22.1: custom signals
    implementation("com.google.firebase:firebase-crashlytics")
    implementation("com.google.firebase:firebase-config")
    implementation("com.google.firebase:firebase-analytics")
    testImplementation("junit:junit:4.13.2")
    testImplementation("org.robolectric:robolectric:4.14.1")
    androidTestImplementation("androidx.test:runner:1.6.2")
    androidTestImplementation("androidx.test.ext:junit:1.2.1")
    androidTestImplementation("androidx.test.uiautomator:uiautomator:2.3.0")
}

// ./gradlew updateRoutes — regenerates CdnRoutes.kt: the /24 subnets behind
// the domains of Services.kt entries marked cdn = true. Lookups run ON THE
// VDS over ssh (DoH to Cloudflare from there): CDNs answer by the asker's
// region, and the phone resolves through the relay, so it sees the VDS's
// answers. Resolving from the Mac would bake Moscow addresses in.
// Needs ssh access to the VDS: vds.ssh=user@host in local.properties.
tasks.register("updateRoutes") {
    group = "release"
    description = "Regenerate CdnRoutes.kt from the cdn services' domains, resolved on the VDS"
    doLast {
        val src = file("src/main/kotlin/org/duckdns/splitvpn/Services.kt").readText()
        val domainRe = Regex("\"([a-z0-9-]+(?:\\.[a-z0-9-]+)+)\"")
        // One chunk per Service("id" …; keep those marked cdn = true.
        val blocks = LinkedHashMap<String, List<String>>()
        for (chunk in src.split("Service(\"").drop(1)) {
            val id = chunk.substringBefore('"')
            if (!chunk.contains("cdn = true")) continue
            blocks[id] = domainRe.findAll(chunk).map { it.groupValues[1] }
                .filter { !it.startsWith("http") && it.any(Char::isLetter) }.distinct().toList()
        }
        require(blocks.isNotEmpty()) { "no cdn services in Services.kt" }
        val host = localProps.getProperty("vds.ssh")?.trim()?.takeIf { it.isNotEmpty() }
            ?: throw GradleException("updateRoutes: set vds.ssh=user@host (ssh access to the VDS) in local.properties")
        // Apex + the usual CDN hostnames: an apex like twimg.com has no
        // address of its own, the images live on pbs./abs./video.
        val prefixes = listOf("", "www.", "api.", "cdn.", "static.", "media.", "pbs.", "abs.", "video.", "gateway.", "chat.", "assets.")
        val script = buildString {
            append("for h in")
            blocks.values.flatten().forEach { d -> prefixes.forEach { append(" $it$d") } }
            // Attribute answers to the host we asked for: behind a CNAME the
            // A records carry another name (pbs.twimg.com → *.cloudflare.net).
            append("; do curl -s -H 'accept: application/dns-json' \"https://cloudflare-dns.com/dns-query?name=\$h&type=A\" | ")
            append("sed 's/[{}]/\\n/g' | grep -o '\"type\":1,[^}]*\"data\":\"[0-9.]*\"' | ")
            append("sed \"s/.*\\\"data\\\":\\\"\\([0-9.]*\\)\\\".*/\$h \\1/\"; done")
        }
        val proc = ProcessBuilder("ssh", "-o", "ConnectTimeout=15", host, script).redirectErrorStream(true).start()
        val out = proc.inputStream.bufferedReader().readText()
        require(proc.waitFor() == 0) { "ssh failed:\n$out" }
        val bySite = out.lines().mapNotNull { l ->
            val p = l.trim().split(' '); if (p.size == 2) p[0].trimEnd('.').removePrefix("www.") to p[1] else null
        }
        val sb = StringBuilder()
        sb.append("package org.duckdns.splitvpn\n\n")
        sb.append("// GENERATED by `./gradlew updateRoutes` — do not edit by hand.\n")
        sb.append("// /24 subnets the cdn services' domains resolved to, as seen from the VDS:\n")
        sb.append("// the phone resolves through the relay, so it sees the same answers.\n")
        sb.append("// Regenerate before a release; the on-device route cache picks up\n")
        sb.append("// moves in between.\n")
        sb.append("object CdnRoutes {\n    val BY_SERVICE: Map<String, List<Route>> = mapOf(\n")
        for ((id, domains) in blocks) {
            val subnets = bySite.filter { (site, _) -> domains.any { site == it || site.endsWith(".$it") } }
                .map { (_, ip) -> ip.split('.').take(3).joinToString(".") + ".0" }.distinct()
                .sortedWith(compareBy({ it.split('.')[0].toInt() }, { it.split('.')[1].toInt() }, { it.split('.')[2].toInt() }))
            require(subnets.isNotEmpty()) { "no addresses for $id: $domains" }
            sb.append("        \"$id\" to listOf(\n")
            subnets.forEach { sb.append("            Route(\"$it\", 24),\n") }
            sb.append("        ),\n")
        }
        sb.append("    )\n}\n")
        file("src/main/kotlin/org/duckdns/splitvpn/CdnRoutes.kt").writeText(sb.toString())
        println("CdnRoutes.kt: " + blocks.keys.joinToString { id -> "$id=${bySite.count { (s, _) -> blocks[id]!!.any { s == it || s.endsWith(".$it") } }} answers" })
    }
}
