package rs.netmon.sms

import java.io.File
import java.util.Date
import java.util.Properties
import javax.mail.Message
import javax.mail.Session
import javax.mail.Transport
import javax.mail.internet.InternetAddress
import javax.mail.internet.MimeMessage
import kotlin.random.Random

/** Oznaka u naslovu e-maila kojim telefon šalje upit konzoli. */
const val QUERY_TAG = "[NETMON-UPIT]"

/**
 * Tekst upita iz SMS-a (npr. „lokacija PS”, „server lista”, „status”); null
 * za prazan SMS. Konzola odlučuje šta je komanda: na nepoznatu odgovara
 * spiskom komandi, pa se prosleđuje svaki SMS sa broja iz aplikacije.
 */
fun parseCommand(body: String): String? {
    val t = body.trim().replace(Regex("\\s+"), " ")
    return t.take(60).ifEmpty { null }
}

/** Poslednjih 9 cifara broja (+381641234567 i 0641234567 su isti broj). */
fun numberKey(n: String): String = n.filter { it.isDigit() }.takeLast(9)

fun allowedNumber(from: String, cfg: Config): Boolean {
    val k = numberKey(from)
    return k.length >= 6 && (cfg.recipients + cfg.adminNumber).any { numberKey(it) == k }
}

/**
 * Upiti koji čekaju odgovor konzole: broj upita → ko je pitao. Odgovor
 * („[NETMON-SMS] #broj …”) ide samo tom broju; broj telefona nikad ne ide
 * u e-mail. Čuva se u fajlu (preživi restart aplikacije).
 */
class QueryBook(
    private val file: File?,
    private val clock: () -> Long = System::currentTimeMillis,
    private val perHour: Int = 10,
) {
    data class Pending(val id: String, val number: String, val text: String, val at: Long)

    private val pending = LinkedHashMap<String, Pending>()
    private val asked = mutableListOf<Pair<String, Long>>() // (broj, vreme) za ograničenje
    private val warned = HashMap<String, Long>() // broj → do kada se ne upozorava ponovo

    init {
        file?.takeIf { it.exists() }?.readLines()?.forEach { line ->
            val p = line.split('\t')
            if (p.size == 4) p[3].toLongOrNull()?.let { pending[p[0]] = Pending(p[0], p[1], p[2], it) }
        }
    }

    /** Novi upit; null ako je broj prekoračio ograničenje po satu. */
    @Synchronized
    fun add(number: String, text: String): Pending? {
        val now = clock()
        asked.removeAll { now - it.second >= RateLimiter.HOUR }
        val key = numberKey(number)
        if (asked.count { it.first == key } >= perHour) return null
        asked += key to now
        var id: String
        do id = Random.nextInt(1000, 1_000_000).toString() while (id in pending)
        val p = Pending(id, number, text.replace('\t', ' ').replace('\n', ' '), now)
        pending[id] = p
        save()
        return p
    }

    /**
     * true samo za prvo prekoračenje ograničenja u satu za taj broj, da
     * upozorenje „previše upita” ne bi išlo na svaki SMS (npr. automatski
     * odgovor sa telefona zaposlenog bi inače pravio beskonačnu razmenu).
     */
    @Synchronized
    fun shouldWarn(number: String): Boolean {
        val now = clock()
        val key = numberKey(number)
        if (now < (warned[key] ?: 0L)) return false
        warned[key] = now + RateLimiter.HOUR
        return true
    }

    /** Uzima upit po broju (odgovor je stigao); null ako ne postoji. */
    @Synchronized
    fun take(id: String): Pending? = pending.remove(id)?.also { save() }

    /** Upiti bez odgovora duže od [timeoutMs] (uklanjaju se). */
    @Synchronized
    fun expired(timeoutMs: Long): List<Pending> {
        val now = clock()
        val old = pending.values.filter { now - it.at >= timeoutMs }
        old.forEach { pending.remove(it.id) }
        if (old.isNotEmpty()) save()
        return old
    }

    private fun save() {
        file?.let { f ->
            val tmp = File(f.path + ".tmp")
            tmp.writeText(pending.values.joinToString("\n") { "${it.id}\t${it.number}\t${it.text}\t${it.at}" })
            tmp.renameTo(f)
        }
    }
}

/** Šalje upit konzoli e-mailom (SMTP preko TLS-a, isti nalog kao sanduče). */
class QueryMailer(
    private val cfg: Config,
    private val extraProps: Map<String, String> = emptyMap(),
) {
    fun send(p: QueryBook.Pending) {
        val props = Properties()
        props["mail.transport.protocol"] = "smtps"
        props["mail.smtps.host"] = cfg.host
        props["mail.smtps.port"] = cfg.smtpPort.toString()
        props["mail.smtps.auth"] = "true"
        props["mail.smtps.ssl.checkserveridentity"] = "true"
        props["mail.smtps.connectiontimeout"] = "20000"
        props["mail.smtps.timeout"] = "30000"
        props.putAll(extraProps)
        val session = Session.getInstance(props)
        val msg = MimeMessage(session)
        msg.setFrom(InternetAddress(cfg.user, "NetMon SMS"))
        msg.setRecipient(Message.RecipientType.TO, InternetAddress(cfg.user))
        msg.subject = "$QUERY_TAG #${p.id} ${p.text}"
        msg.sentDate = Date()
        msg.setText("Upit sa NetMon SMS telefona.\n", "utf-8")
        val proto = props["mail.transport.protocol"] as String
        session.getTransport(proto).use { t ->
            t.connect(cfg.host, cfg.smtpPort, cfg.user, cfg.password)
            t.sendMessage(msg, msg.allRecipients)
        }
    }
}
