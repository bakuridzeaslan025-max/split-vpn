package org.duckdns.splitvpn

data class Route(val address: String, val prefix: Int)

data class Service(
    val id: String,
    val title: String,
    val probeUrl: String,
    val defaultEnabled: Boolean,
    /** The service's own networks, hand-written. Empty for [cdn] services. */
    val routes: List<Route>,
    /** Site suffixes that go through the relay; other names inside the routes are dialed directly. */
    val domains: List<String>,
    /** Hosted on a shared CDN: subnets come from [CdnRoutes] (`./gradlew updateRoutes`), not from here. */
    val cdn: Boolean = false,
) {
    val allRoutes: List<Route> get() = routes + (CdnRoutes.BY_SERVICE[id] ?: emptyList())
}

object Services {
    val ALL = listOf(
        Service("telegram", "Telegram", "https://web.telegram.org/", true, listOf(
            Route("149.154.160.0", 20),
            Route("91.108.4.0", 22),
            Route("91.108.8.0", 22),
            Route("91.108.12.0", 22),
            Route("91.108.16.0", 22),
            Route("91.108.20.0", 22),
            Route("91.108.56.0", 22),
            Route("91.105.192.0", 23),
            Route("185.76.151.0", 24),
            Route("95.161.64.0", 20),
        ), listOf("telegram.org", "t.me", "telegram.me", "telesco.pe", "tdesktop.com", "telegram.dog")),
        Service("youtube", "YouTube", "https://www.youtube.com/", false, listOf(
            Route("216.58.192.0", 19),
            Route("142.250.0.0", 15),
            Route("172.217.0.0", 16),
            Route("74.125.0.0", 16),
            Route("173.194.0.0", 16),
            Route("209.85.128.0", 17),
        ), listOf(
            "youtube.com", "youtu.be", "googlevideo.com", "ytimg.com", "ggpht.com", "youtube-nocookie.com",
            "googleapis.com", "google.com", "gstatic.com", "googleusercontent.com", "gvt1.com",
        )),
        Service("meta", "Instagram", "https://www.instagram.com/", false, listOf(
            Route("157.240.0.0", 16),
            Route("31.13.24.0", 21),
            Route("31.13.64.0", 18),
            Route("179.60.192.0", 22),
            Route("57.144.0.0", 14),
            Route("163.70.128.0", 17),
            Route("185.60.216.0", 22),
            Route("129.134.0.0", 16),
        ), listOf(
            "instagram.com", "cdninstagram.com", "facebook.com", "facebook.net", "fbcdn.net", "fb.com", "fbsbx.com",
            "whatsapp.com", "whatsapp.net", "messenger.com", "meta.com",
        )),
        Service("slack", "Slack", "https://app.slack.com/", false, emptyList(), listOf(
            "slack.com", "slackb.com", "slack-imgs.com", "slack-edge.com", "slack-core.com",
        ), cdn = true),
        Service("twitter", "X", "https://x.com/", false, listOf(
            Route("104.244.42.0", 24),
            Route("199.59.148.0", 22),
        ), listOf("x.com", "twitter.com", "twimg.com", "t.co"), cdn = true),
        Service("cloudflare", "OpenAI", "https://chatgpt.com/", false, emptyList(), listOf(
            "openai.com", "chatgpt.com", "oaistatic.com", "oaiusercontent.com",
            "discord.com", "discord.gg", "discordapp.com", "discordapp.net", "discord.media",
            "anthropic.com", "claude.ai",
        ), cdn = true),
    )

    const val PREFS = "splitvpn"

    fun prefKey(id: String) = "svc_$id"
}
