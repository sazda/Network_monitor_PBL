package rs.netmon.sms

import java.io.File
import java.nio.file.Files
import java.util.Calendar
import java.util.TimeZone
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertNotNull
import kotlin.test.assertNull

class CheckGuardTest {
    private val tz = TimeZone.getTimeZone("Europe/Belgrade")
    private fun at(day: Int, h: Int, m: Int) = Calendar.getInstance(tz).apply {
        clear(); set(2026, Calendar.NOVEMBER, day, h, m, 0)
    }.timeInMillis

    @Test
    fun alertsOnceWhenCheckIsMissing() {
        var now = at(9, 8, 29)
        val f = File(Files.createTempDirectory("netmon").toFile(), "provera")
        val g = CheckGuard(f, { now }, tz)
        assertNull(g.due("08:30"), "pre roka")
        now = at(9, 8, 31)
        assertNotNull(g.due("08:30"))
        g.alerted()
        assertNull(g.due("08:30"), "jednom dnevno")
        assertNull(CheckGuard(f, { now }, tz).due("08:30"), "pamti se i posle restarta")
        // Sledećeg dana provera stigne u 08:00: nema upozorenja.
        now = at(10, 8, 0)
        g.received()
        now = at(10, 9, 0)
        assertNull(g.due("08:30"))
        // Prekosutra ne stigne.
        now = at(11, 8, 45)
        assertNotNull(g.due("08:30"))
        assertNull(g.due(""), "isključeno")
        assertEquals(8 to 30, CheckGuard.parseTime("8:30"))
        assertNull(CheckGuard.parseTime("25:00"))
    }
}
