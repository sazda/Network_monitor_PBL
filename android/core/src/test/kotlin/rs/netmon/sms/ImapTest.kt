package rs.netmon.sms

import java.util.Collections
import java.util.Date
import java.util.Properties
import javax.mail.Folder
import javax.mail.Session
import javax.mail.internet.InternetAddress
import javax.mail.internet.MimeMessage
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertTrue
import kotlin.test.fail

/**
 * Test sa pravim IMAP serverom. Pokreće se samo ako je zadato
 * NETMON_TEST_IMAP=host:port:nalog:lozinka (npr. lokalni Dovecot).
 */
class ImapTest {
    private val env = System.getenv("NETMON_TEST_IMAP").orEmpty()

    @Test
    fun watcherSendsMovesAndSurvivesKeepAlive() {
        if (env.isBlank()) return
        val (host, port, user, pass) = env.split(":")
        val trust = mapOf("mail.imaps.ssl.trust" to "*", "mail.imaps.ssl.checkserveridentity" to "false")
        val cfg = Config(
            host = host, port = port.toInt(), user = user, password = pass, allowedSender = "konzola@firma.rs",
            recipients = listOf("+381641111111", "+381652222222"), adminNumber = "+381643333333",
        )
        val sent = Collections.synchronizedList(mutableListOf<String>())
        val logs = Collections.synchronizedList(mutableListOf<String>())
        var checks = 0
        val w = MailWatcher(cfg, { n, t -> sent += "$n|$t"; null }, ProcessedStore(null), { logs += it; println(it) },
            keepAliveMs = 2_000, extraProps = trust, onCheck = { checks++ })
        println(w.test())

        val props = Properties().apply { putAll(trust); put("mail.store.protocol", "imaps") }
        val st = Session.getInstance(props).getStore("imaps").also { it.connect(host, port.toInt(), user, pass) }
        val inbox = st.getFolder("INBOX")
        fun append(subject: String, from: String = "Network Monitor <konzola@firma.rs>") {
            val m = MimeMessage(Session.getInstance(props))
            m.setFrom(InternetAddress(from)); m.subject = subject; m.sentDate = Date(); m.setText("x")
            inbox.appendMessages(arrayOf(m))
        }
        append("[NETMON-SMS] NetMon 09:12 PAD JG_SW1 (Jug)")
        append("[NETMON-SMS] PAD lazno", from = "napadac@drugi.rs")
        append("obicna posta")

        val t = Thread { w.run() }.also { it.start() }
        waitFor { sent.size >= 2 }
        // Posle nekoliko NOOP-ova (IDLE se prekida i obnavlja) nova poruka i dalje stiže.
        Thread.sleep(5_000)
        append("[NETMON-SMS] PROVERA 06.11.2026 08:00")
        waitFor { sent.size >= 3 }
        w.stop(); t.join(10_000)

        assertEquals(
            listOf("+381641111111|NetMon 09:12 PAD JG_SW1 (Jug)", "+381652222222|NetMon 09:12 PAD JG_SW1 (Jug)",
                "+381643333333|NetMon SMS veza OK (PROVERA 06.11.2026 08:00)"), sent.toList())
        inbox.open(Folder.READ_ONLY)
        val left = inbox.messages.map { it.subject }
        inbox.close(false)
        assertEquals(listOf("obicna posta"), left, "u INBOX-u ostaje samo obična pošta")
        // Server sa prostorom imena „INBOX.” (cPanel): folder je INBOX.NetMon-poslato.
        val ns = st.personalNamespaces.first()
        val done = (if (ns.fullName.isEmpty()) st.getFolder(cfg.doneFolder) else ns.getFolder(cfg.doneFolder)).also { it.open(Folder.READ_ONLY) }
        println("folder: ${done.fullName}")
        assertEquals(3, done.messageCount)
        done.close(false); st.close()
        assertTrue(logs.any { it.contains("nepoznat pošiljalac") })
        assertTrue(logs.none { it.contains("Premeštanje") || it.contains("nije napravljen") }, "premeštanje mora da radi")
        assertEquals(1, checks)
    }

    private fun waitFor(cond: () -> Boolean) {
        repeat(300) { if (cond()) return; Thread.sleep(100) }
        fail("isteklo vreme")
    }
}
