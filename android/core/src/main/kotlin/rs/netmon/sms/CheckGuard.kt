package rs.netmon.sms

import java.io.File
import java.text.SimpleDateFormat
import java.util.Calendar
import java.util.Locale
import java.util.TimeZone

/**
 * Prati da li je dnevna provera konzole stigla. Ako do zadatog vremena nije,
 * telefon sam šalje upozorenje administratoru: tako se primeti i kada konzola,
 * mail server ili veza telefona ne rade (SMS ne zavisi od interneta).
 */
class CheckGuard(
    private val file: File?,
    private val clock: () -> Long = System::currentTimeMillis,
    private val zone: TimeZone = TimeZone.getDefault(),
) {
    private var lastReceived = 0L
    private var lastAlertDay = ""

    init {
        file?.takeIf { it.exists() }?.readLines()?.let { l ->
            lastReceived = l.getOrNull(0)?.toLongOrNull() ?: 0L
            lastAlertDay = l.getOrNull(1).orEmpty()
        }
    }

    @Synchronized
    fun received() {
        lastReceived = clock()
        save()
    }

    @Synchronized
    fun lastReceivedAt(): Long = lastReceived

    /** Tekst upozorenja ako je rok prošao, a provera danas nije stigla; inače null. */
    @Synchronized
    fun due(deadline: String): String? {
        val (h, m) = parseTime(deadline) ?: return null
        val now = clock()
        val cal = Calendar.getInstance(zone).apply { timeInMillis = now }
        val day = day(now)
        cal.set(Calendar.HOUR_OF_DAY, 0); cal.set(Calendar.MINUTE, 0); cal.set(Calendar.SECOND, 0); cal.set(Calendar.MILLISECOND, 0)
        val startOfDay = cal.timeInMillis
        val dl = startOfDay + (h * 60L + m) * 60_000L
        if (now < dl || lastAlertDay == day || lastReceived >= startOfDay) return null
        return "NetMon SMS: dnevna provera nije stigla do $deadline - proveri konzolu, mail server i vezu telefona"
    }

    /** Beleži da je upozorenje za danas poslato. */
    @Synchronized
    fun alerted() {
        lastAlertDay = day(clock())
        save()
    }

    private fun day(t: Long) = SimpleDateFormat("yyyy-MM-dd", Locale.ROOT).apply { timeZone = zone }.format(t)

    private fun save() {
        file?.let { f ->
            val tmp = File(f.path + ".tmp")
            tmp.writeText("$lastReceived\n$lastAlertDay")
            tmp.renameTo(f)
        }
    }

    companion object {
        fun parseTime(s: String): Pair<Int, Int>? {
            val m = Regex("""^(\d{1,2}):(\d{2})$""").matchEntire(s.trim()) ?: return null
            val h = m.groupValues[1].toInt()
            val min = m.groupValues[2].toInt()
            return if (h in 0..23 && min in 0..59) h to min else null
        }
    }
}
