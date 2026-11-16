package rs.netmon.sms

import java.util.Date

/** Podešavanja aplikacije (lozinka se na telefonu čuva šifrovano). */
data class Config(
    val host: String,
    val port: Int = 993,
    val user: String,
    val password: String,
    /** Adresa sa koje konzola šalje (polje „Pošiljalac” u konzoli). */
    val allowedSender: String,
    /** Brojevi koji primaju obaveštenja. Nikad se ne čitaju iz e-maila. */
    val recipients: List<String>,
    /** Broj koji prima potvrdu dnevne provere i upozorenja aplikacije. */
    val adminNumber: String,
    val tag: String = TAG,
    val maxPerHour: Int = 20,
    val maxAgeMinutes: Int = 60,
    val doneFolder: String = "NetMon-poslato",
    /** Do kada (SS:MM) mora stići dnevna provera konzole; "" = ne proverava se. */
    val checkDeadline: String = "08:30",
    /** SMTP port za slanje upita konzoli (465 = TLS od početka). */
    val smtpPort: Int = 465,
) {
    val admin: String get() = adminNumber.ifBlank { recipients.firstOrNull() ?: "" }

    fun problems(): List<String> {
        val p = mutableListOf<String>()
        if (host.isBlank()) p += "server nije unet"
        if (port !in 1..65535) p += "neispravan port"
        if (user.isBlank() || password.isBlank()) p += "nalog ili lozinka nisu uneti"
        if (!allowedSender.contains("@")) p += "adresa pošiljaoca (konzole) nije uneta"
        if (recipients.isEmpty()) p += "nema nijednog broja za obaveštenja"
        (recipients + listOfNotNull(adminNumber.ifBlank { null })).filterNot(::validNumber).forEach { p += "neispravan broj: $it" }
        if (maxPerHour < 1) p += "ograničenje mora biti bar 1 SMS na sat"
        if (checkDeadline.isNotBlank() && CheckGuard.parseTime(checkDeadline) == null) p += "vreme dnevne provere mora biti SS:MM (npr. 08:30)"
        return p
    }

    companion object {
        const val TAG = "[NETMON-SMS]"

        fun validNumber(n: String) = Regex("""^\+?[0-9]{6,15}$""").matches(n)

        /** Brojevi razdvojeni zarezom, razmakom ili novim redom. */
        fun parseNumbers(text: String): List<String> =
            text.split(',', ';', ' ', '\n', '\t').map { it.replace("-", "").replace("/", "").trim() }.filter { it.isNotEmpty() }.distinct()
    }
}

/** Poruka iz sanduča (samo ono što je potrebno za odluku). */
data class Incoming(val id: String, val from: String, val subject: String, val sent: Date?)

sealed class Action {
    /**
     * Pošalji [text] na [numbers]; [counted] = ulazi u ograničenje po satu;
     * [check] = dnevna provera konzole (beleži se da je stigla).
     */
    data class Send(val numbers: List<String>, val text: String, val counted: Boolean, val check: Boolean = false) : Action()

    /** Odgovor na upit #[id]: ide samo broju koji je pitao. */
    data class Reply(val id: String, val text: String) : Action()

    /** Ne šalje se; poruka se ipak označava kao obrađena. */
    data class Skip(val reason: String) : Action()
}

const val SMS_MAX_TEXT = 320 // najviše dva SMS-a po obaveštenju (odgovori na upite nemaju ograničenje)

/** Odlučuje šta se radi sa porukom. */
fun decide(msg: Incoming, cfg: Config, now: Date): Action {
    val subject = msg.subject.trim()
    if (!subject.startsWith(cfg.tag, ignoreCase = true)) return Action.Skip("naslov nema oznaku ${cfg.tag}")
    if (!sameAddress(msg.from, cfg.allowedSender)) return Action.Skip("nepoznat pošiljalac ${msg.from}")
    val sent = msg.sent
    if (sent != null && now.time - sent.time > cfg.maxAgeMinutes * 60_000L) {
        return Action.Skip("poruka je starija od ${cfg.maxAgeMinutes} min")
    }
    val text = clean(subject.substring(cfg.tag.length))
    if (text.isEmpty()) return Action.Skip("prazna poruka")
    Regex("""^#(\d{1,9})\s+(.+)$""").matchEntire(text)?.let { m ->
        // Dužinu odgovora određuje konzola (npr. „server lista” može biti duži).
        return Action.Reply(m.groupValues[1], replyLines(m.groupValues[2]))
    }
    if (text.startsWith("PROVERA", ignoreCase = true)) {
        return Action.Send(listOf(cfg.admin), "NetMon SMS veza OK (${limit(text)})", counted = false, check = true)
    }
    return Action.Send(cfg.recipients, limit(text), counted = true)
}

/** Adresa iz zaglavlja „From” (npr. "Network Monitor <a@b.rs>") u obliku a@b.rs. */
fun addressOf(from: String): String {
    val m = Regex("<([^>]+)>").find(from)
    return (m?.groupValues?.get(1) ?: from).trim().lowercase()
}

fun sameAddress(from: String, allowed: String) = addressOf(from) == addressOf(allowed)

private fun clean(s: String): String =
    s.map { if (it.code < 0x20 || it.code == 0x7f) ' ' else it }.joinToString("").replace(Regex("\\s+"), " ").trim()

/**
 * Odgovor na upit: oznaka `\n` od konzole postaje novi red (naslov e-maila
 * ne može da ima nove redove). Razmaci oko preloma se uklanjaju.
 */
fun replyLines(t: String): String =
    t.split("\\n").joinToString("\n") { it.trim() }.trim()

/** Obaveštenja: najviše dva SMS-a. */
private fun limit(t: String): String = if (t.length > SMS_MAX_TEXT) t.substring(0, SMS_MAX_TEXT - 3) + "..." else t

/** Ograničenje broja SMS obaveštenja na sat (klizni prozor). */
class RateLimiter(private val perHour: Int) {
    private val times = ArrayDeque<Long>()
    private var warnedUntil = 0L

    /** true = sme da se pošalje (i beleži slanje). */
    fun allow(now: Long): Boolean {
        while (times.isNotEmpty() && now - times.first() >= HOUR) times.removeFirst()
        if (times.size >= perHour) return false
        times.addLast(now)
        return true
    }

    /** Kada se ponovo sme slati (posle prekoračenja). */
    fun freeAt(): Long = (times.firstOrNull() ?: 0L) + HOUR

    /** true samo za prvo prekoračenje u prozoru (da upozorenje ode jednom). */
    fun shouldWarn(now: Long): Boolean {
        if (now < warnedUntil) return false
        warnedUntil = freeAt()
        return true
    }

    companion object {
        const val HOUR = 3_600_000L
    }
}
