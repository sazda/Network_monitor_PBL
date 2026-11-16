package rs.netmon.sms

import com.sun.mail.imap.IMAPFolder
import java.io.File
import java.text.SimpleDateFormat
import java.util.Date
import java.util.Locale
import java.util.Properties
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicBoolean
import javax.mail.Flags
import javax.mail.Folder
import javax.mail.Message
import javax.mail.Session
import javax.mail.Store
import javax.mail.internet.MimeMessage
import javax.mail.search.AndTerm
import javax.mail.search.FlagTerm
import javax.mail.search.SubjectTerm

/** Slanje SMS-a (na Android-u SmsManager). */
fun interface SmsSender {
    /** Vraća null ako je poslato, inače opis greške. */
    fun send(number: String, text: String): String?
}

fun interface Logger {
    fun log(msg: String)
}

/** Obrađene poruke (Message-ID), da ista poruka nikad ne ode dvaput. */
class ProcessedStore(private val file: File?, private val keep: Int = 500) {
    private val ids = LinkedHashSet<String>()

    init {
        file?.takeIf { it.exists() }?.readLines()?.filter { it.isNotBlank() }?.let { ids.addAll(it) }
    }

    @Synchronized
    fun contains(id: String) = id in ids

    @Synchronized
    fun add(id: String) {
        ids.add(id)
        while (ids.size > keep) ids.remove(ids.first())
        file?.let { f ->
            val tmp = File(f.path + ".tmp")
            tmp.writeText(ids.joinToString("\n"))
            tmp.renameTo(f)
        }
    }
}

/**
 * Prati sanduče preko IMAP-a (IDLE: server javlja novu poruku odmah) i za
 * svaku poruku konzole šalje SMS. Veza se proverava na [keepAliveMs]; kad
 * pukne, ponovo se uspostavlja.
 */
class MailWatcher(
    private val cfg: Config,
    private val sms: SmsSender,
    private val store: ProcessedStore,
    private val log: Logger,
    private val clock: () -> Long = System::currentTimeMillis,
    private val keepAliveMs: Long = 4 * 60_000L,
    /** Dodatna javax.mail podešavanja (samo za testove). */
    private val extraProps: Map<String, String> = emptyMap(),
    /** Poziva se kada je obrađena dnevna provera konzole. */
    private val onCheck: () -> Unit = {},
    /** Odgovor na upit: broj upita → ko je pitao (null = nepoznat ili istekao). */
    private val takeQuery: (String) -> QueryBook.Pending? = { null },
) {
    private val limiter = RateLimiter(cfg.maxPerHour)
    private val stopped = AtomicBoolean(false)

    @Volatile
    private var current: Store? = null

    private fun session(): Session {
        val p = Properties()
        p["mail.store.protocol"] = "imaps"
        p["mail.imaps.host"] = cfg.host
        p["mail.imaps.port"] = cfg.port.toString()
        p["mail.imaps.ssl.checkserveridentity"] = "true"
        p["mail.imaps.connectiontimeout"] = "20000"
        // Duže od keepAliveMs: veza koja ne odgovori ni na NOOP se prekida.
        p["mail.imaps.timeout"] = (keepAliveMs + 60_000L).toString()
        p["mail.imaps.writetimeout"] = "30000"
        p.putAll(extraProps)
        return Session.getInstance(p)
    }

    private fun connect(): Store {
        val st = session().getStore("imaps")
        st.connect(cfg.host, cfg.port, cfg.user, cfg.password)
        return st
    }

    /** Provera veze za dugme „Proveri vezu”: prijava i broj poruka konzole. */
    fun test(): String {
        val st = connect()
        try {
            val inbox = st.getFolder("INBOX")
            inbox.open(Folder.READ_ONLY)
            val waiting = inbox.search(AndTerm(FlagTerm(Flags(Flags.Flag.SEEN), false), SubjectTerm(cfg.tag))).size
            val total = inbox.messageCount
            inbox.close(false)
            val idle = (st as? com.sun.mail.imap.IMAPStore)?.hasCapability("IDLE") == true
            return "Prijava uspela. U sanduču $total poruka, neobrađenih za SMS: $waiting. " +
                if (idle) "Server podržava IDLE (poruke stižu odmah)." else "Server ne podržava IDLE (provera na ${keepAliveMs / 60_000} min)."
        } finally {
            runCatching { st.close() }
        }
    }

    /** Prekida rad (iz druge niti). */
    fun stop() {
        stopped.set(true)
        runCatching { current?.close() }
    }

    /** Radi dok se ne pozove [stop]. */
    fun run() {
        var backoff = 5_000L
        while (!stopped.get()) {
            try {
                val st = connect()
                current = st
                log.log("Povezan na ${cfg.host} kao ${cfg.user}")
                backoff = 5_000L
                watch(st)
            } catch (e: Exception) {
                if (stopped.get()) break
                log.log("Greška veze: ${describe(e)}; ponovo za ${backoff / 1000} s")
                sleep(backoff)
                backoff = (backoff * 2).coerceAtMost(120_000L)
            } finally {
                runCatching { current?.close() }
                current = null
            }
        }
        log.log("Zaustavljeno")
    }

    private fun watch(st: Store) {
        val inbox = st.getFolder("INBOX") as IMAPFolder
        inbox.open(Folder.READ_WRITE)
        val done = doneFolder(st)
        val idle = (st as? com.sun.mail.imap.IMAPStore)?.hasCapability("IDLE") == true
        val timer = Executors.newSingleThreadScheduledExecutor()
        try {
            // NOOP prekida IDLE, pa se na keepAliveMs ponovo proverava sanduče
            // i veza (mobilni internet zna da „zaboravi” vezu bez greške).
            if (idle) {
                timer.scheduleWithFixedDelay({
                    runCatching { inbox.doCommand { p -> p.simpleCommand("NOOP", null) } }
                }, keepAliveMs, keepAliveMs, TimeUnit.MILLISECONDS)
            }
            while (!stopped.get()) {
                processNew(inbox, done)
                if (idle) inbox.idle(true) else {
                    sleep(keepAliveMs)
                    inbox.messageCount
                }
                if (!inbox.isOpen) throw IllegalStateException("sanduče je zatvoreno")
            }
        } finally {
            timer.shutdownNow()
            runCatching { if (inbox.isOpen) inbox.close(false) }
        }
    }

    // Folder se pravi u ličnom prostoru naloga: na serverima kao što je
    // Dovecot na cPanel-u svi folderi moraju biti ispod INBOX-a („INBOX.ime”).
    private fun doneFolder(st: Store): Folder? = runCatching {
        val ns = runCatching { st.personalNamespaces.firstOrNull() }.getOrNull()
        val f = if (ns != null && ns.fullName.isNotEmpty()) ns.getFolder(cfg.doneFolder) else st.getFolder(cfg.doneFolder)
        if (!f.exists()) f.create(Folder.HOLDS_MESSAGES)
        f
    }.onFailure { log.log("Folder ${cfg.doneFolder} nije napravljen (${describe(it)}); poruke se samo označavaju kao pročitane") }
        .getOrNull()

    /** Obrađuje sve neobrađene poruke sa oznakom; vraća broj obrađenih. */
    fun processNew(inbox: Folder, done: Folder?): Int {
        val found = inbox.search(AndTerm(FlagTerm(Flags(Flags.Flag.SEEN), false), SubjectTerm(cfg.tag)))
        val msgs = found.sortedBy { it.receivedDate ?: it.sentDate ?: Date(0) }
        var n = 0
        for (m in msgs) {
            if (handle(m)) {
                markDone(inbox, m, done)
                n++
            }
        }
        return n
    }

    /** true = poruka je obrađena (može da se skloni). */
    private fun handle(m: Message): Boolean {
        val id = (m as? MimeMessage)?.messageID ?: "${m.sentDate?.time}:${m.subject}"
        if (store.contains(id)) return true
        val inc = Incoming(id, m.from?.firstOrNull()?.toString() ?: "", m.subject ?: "", m.sentDate)
        when (val a = decide(inc, cfg, Date(clock()))) {
            is Action.Skip -> {
                log.log("Preskočeno: ${a.reason} – „${inc.subject.take(80)}”")
                store.add(id)
                return true
            }
            is Action.Reply -> {
                val q = takeQuery(a.id)
                if (q == null) {
                    log.log("Odgovor na upit #${a.id} preskočen: upit nije poznat ili je istekao")
                    store.add(id)
                    return true
                }
                val err = sms.send(q.number, a.text)
                if (err != null) {
                    log.log("Odgovor na upit #${a.id} nije poslat na ${q.number}: $err")
                } else {
                    log.log("Odgovor na upit „${q.text}” poslat na ${q.number}: ${a.text}")
                }
                store.add(id)
                return true
            }
            is Action.Send -> {
                val now = clock()
                if (a.counted && !limiter.allow(now)) {
                    log.log("Ograničenje ${cfg.maxPerHour} SMS/h: nije poslato „${a.text}”")
                    if (limiter.shouldWarn(now)) {
                        val until = SimpleDateFormat("HH:mm", Locale.ROOT).format(Date(limiter.freeAt()))
                        sms.send(cfg.admin, "NetMon SMS: vise od ${cfg.maxPerHour} poruka na sat, slanje pauzirano do $until")
                    }
                    store.add(id)
                    return true
                }
                val errors = a.numbers.mapNotNull { num -> sms.send(num, a.text)?.let { "$num: $it" } }
                if (errors.size == a.numbers.size) {
                    // Nijedan SMS nije otišao: poruka ostaje za sledeći pokušaj.
                    log.log("SMS nije poslat (${errors.joinToString("; ")}); ponovni pokušaj kasnije")
                    return false
                }
                store.add(id)
                if (a.check) onCheck()
                log.log("SMS poslat na ${a.numbers.size - errors.size}/${a.numbers.size} brojeva: ${a.text}" +
                    if (errors.isEmpty()) "" else " (greške: ${errors.joinToString("; ")})")
                return true
            }
        }
    }

    private fun markDone(inbox: Folder, m: Message, done: Folder?) {
        runCatching { m.setFlag(Flags.Flag.SEEN, true) }
        if (done == null) return
        runCatching {
            if (inbox is IMAPFolder) inbox.moveMessages(arrayOf(m), done) else {
                inbox.copyMessages(arrayOf(m), done)
                m.setFlag(Flags.Flag.DELETED, true)
                inbox.expunge()
            }
        }.onFailure { log.log("Premeštanje u ${cfg.doneFolder}: ${describe(it)}") }
    }

    private fun sleep(ms: Long) {
        val end = clock() + ms
        while (!stopped.get() && clock() < end) Thread.sleep(minOf(1000L, ms))
    }

    companion object {
        fun describe(e: Throwable): String {
            val root = generateSequence(e) { it.cause }.last()
            return when (root) {
                is javax.mail.AuthenticationFailedException -> "prijava nije uspela (nalog ili lozinka)"
                is java.net.UnknownHostException -> "server ${root.message} nije pronađen (DNS / internet)"
                is java.net.SocketTimeoutException -> "server ne odgovara (isteklo vreme)"
                is java.net.ConnectException -> "veza odbijena (${root.message})"
                is javax.net.ssl.SSLException -> "TLS greška: ${root.message}"
                else -> (e.message ?: e.javaClass.simpleName).take(200)
            }
        }
    }
}
