package org.duckdns.splitvpn

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import java.io.File

@RunWith(RobolectricTestRunner::class)
class AppLogTest {

    private val app get() = RuntimeEnvironment.getApplication()
    private val dir get() = AppLog.dir(app)

    @Before
    fun setUp() {
        dir.deleteRecursively()
        AppLog.init(app)
    }

    @Test
    fun writesLevelsAndGoLinesToOneFile() {
        AppLog.i("started")
        AppLog.go("dns a.test: 50 bytes")
        AppLog.e("boom", IllegalStateException("bad"))
        val lines = File(dir, AppLog.FILE).readLines()
        assertTrue(lines[0], Regex("""\d\d-\d\d \d\d:\d\d:\d\d\.\d{3} I started""").matches(lines[0]))
        assertTrue(lines[1].endsWith(" G dns a.test: 50 bytes"))
        assertTrue(lines[2].endsWith(" E boom"))
        assertTrue(lines.any { it.contains("IllegalStateException: bad") })
    }

    @Test
    fun rotatesOnceOverLimit() {
        val chunk = "x".repeat(64 * 1024)
        repeat(16) { AppLog.i(chunk) } // 16 * (64 KB + header) is just over MAX_BYTES
        AppLog.i("after")
        val rotated = File(dir, AppLog.ROTATED)
        val current = File(dir, AppLog.FILE)
        assertTrue(rotated.exists())
        assertTrue(rotated.length() > AppLog.MAX_BYTES)
        assertEquals(1, current.readLines().size)
        assertTrue(current.readText().endsWith(" I after\n"))
        assertEquals(listOf(current, rotated), AppLog.files(app))
    }

    /**
     * The panic marker crosses the Go/Kotlin border as a gomobile-exported
     * constant. If gomobile ever renames it, or the Go side changes its
     * wording, panics silently degrade into ordinary log lines and no report
     * is ever filed — so pin both ends against a line Go actually prints.
     */
    @Test
    fun recognisesTheMarkerGoActuallyPrints() {
        val real = "go panic in handleTCP: assignment to entry in nil map" +
            " ← tunnel.handleTCP(tunnel.go:410)"
        assertTrue(real.startsWith(tunnel.Tunnel.PanicPrefix))
        assertFalse("dns a.test: 50 bytes".startsWith(tunnel.Tunnel.PanicPrefix))
    }

    @Test
    fun filesEmptyBeforeAnyWrite() {
        assertTrue(AppLog.files(app).isEmpty())
        assertFalse(File(dir, AppLog.FILE).exists())
    }
}
