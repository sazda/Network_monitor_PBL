package rs.netmon.sms

import android.content.Context
import java.io.File
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale

/** Dnevnik događaja (poslednjih 200), čuva se u fajlu i prikazuje na ekranu. */
object AppLog {
    private const val KEEP = 200
    private val lines = ArrayDeque<String>()
    private var file: File? = null
    private val listeners = mutableListOf<() -> Unit>()
    private val fmt = SimpleDateFormat("dd.MM. HH:mm:ss", Locale.ROOT)

    @Synchronized
    fun init(ctx: Context) {
        if (file != null) return
        val f = File(ctx.filesDir, "dnevnik.txt")
        file = f
        if (f.exists()) f.readLines().takeLast(KEEP).forEach { lines.addLast(it) }
    }

    fun add(msg: String) {
        val snapshot: List<() -> Unit>
        synchronized(this) {
            lines.addLast("${fmt.format(Date())}  $msg")
            while (lines.size > KEEP) lines.removeFirst()
            runCatching { file?.writeText(lines.joinToString("\n")) }
            snapshot = listeners.toList()
        }
        snapshot.forEach { it() }
    }

    @Synchronized
    fun text(): String = lines.reversed().joinToString("\n")

    @Synchronized
    fun listen(l: () -> Unit) {
        listeners += l
    }

    @Synchronized
    fun unlisten(l: () -> Unit) {
        listeners -= l
    }
}
