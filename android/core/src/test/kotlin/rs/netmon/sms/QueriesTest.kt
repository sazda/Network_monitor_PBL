package rs.netmon.sms

import java.io.File
import java.net.ServerSocket
import java.nio.file.Files
import java.util.Date
import kotlin.concurrent.thread
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertNotNull
import kotlin.test.assertNull
import kotlin.test.assertTrue

class QueriesTest {
    private val cfg = Config(
        host = "mail", user = "netmon@firma.rs", password = "x", allowedSender = "NetMon <netmon@firma.rs>",
        recipients = listOf("+381641111111", "0652222222"), adminNumber = "+381643333333",
    )

    @Test
    fun commandsAndNumbers() {
        assertEquals("lokacija Glavni Centar", parseCommand("  lokacija   Glavni Centar "))
        assertEquals("STATUS", parseCommand("STATUS"))
        assertEquals("server SQL01", parseCommand(" server   SQL01 "))
        assertEquals("mpls lista", parseCommand("mpls\nlista"))
        assertEquals("lokacja ps", parseCommand("lokacja ps")) // konzola odgovara spiskom komandi
        assertNull(parseCommand("   "))
        assertEquals(60, parseCommand("x".repeat(200))!!.length)
        assertTrue(allowedNumber("0641111111", cfg))
        assertTrue(allowedNumber("+381 65 222 2222", cfg))
        assertTrue(allowedNumber("+381643333333", cfg))
        assertFalse(allowedNumber("+381649999999", cfg))
    }

    @Test
    fun replyGoesOnlyToAsker() {
        val a = decide(Incoming("m", "netmon@firma.rs", "[NETMON-SMS] #4711 STATUS Sever 10:42: sve operativno", Date()), cfg, Date())
        assertEquals(Action.Reply("4711", "STATUS Sever 10:42: sve operativno"), a)
    }

    @Test
    fun bookLimitsAndExpires() {
        var now = 0L
        val f = File(Files.createTempDirectory("netmon").toFile(), "upiti")
        val b = QueryBook(f, { now }, perHour = 2)
        val p1 = assertNotNull(b.add("0641111111", "status"))
        assertNotNull(b.add("+381641111111", "status Dolina"))
        assertNull(b.add("064 111 1111", "status"), "isti broj, treći upit u satu")
        // Upozorenje „previše upita” samo jednom u satu po broju.
        assertTrue(b.shouldWarn("0641111111"))
        assertFalse(b.shouldWarn("+381641111111"))
        assertTrue(b.shouldWarn("0652222222"))
        assertNotNull(b.add("0652222222", "status"))
        // Posle restarta aplikacije upit je i dalje poznat.
        val b2 = QueryBook(f, { now })
        assertEquals("0641111111", b2.take(p1.id)?.number)
        assertNull(b2.take(p1.id), "odgovor samo jednom")
        now += 3 * 60_000L
        assertEquals(2, b2.expired(3 * 60_000L).size)
        assertTrue(b2.expired(0).isEmpty())
    }

    // Upit se šalje e-mailom (ovde na lokalni SMTP server bez TLS-a).
    @Test
    fun mailerSendsQuery() {
        val server = ServerSocket(0)
        var got = ""
        val t = thread {
            server.accept().use { s ->
                val r = s.getInputStream().bufferedReader()
                val w = s.getOutputStream().bufferedWriter()
                fun say(x: String) { w.write(x + "\r\n"); w.flush() }
                say("220 test")
                val sb = StringBuilder()
                var data = false
                while (true) {
                    val line = r.readLine() ?: break
                    if (data) {
                        if (line == ".") { data = false; say("250 ok"); continue }
                        sb.append(line).append('\n'); continue
                    }
                    when {
                        line.startsWith("EHLO") || line.startsWith("HELO") -> say("250 test")
                        line.startsWith("DATA") -> { data = true; say("354 go") }
                        line.startsWith("QUIT") -> { say("221 bye"); break }
                        else -> say("250 ok")
                    }
                }
                got = sb.toString()
            }
        }
        val p = QueryBook.Pending("4711", "0641111111", "status Sever", 0)
        QueryMailer(cfg.copy(host = "127.0.0.1", smtpPort = server.localPort),
            mapOf("mail.transport.protocol" to "smtp", "mail.smtp.host" to "127.0.0.1", "mail.smtp.port" to server.localPort.toString())).send(p)
        t.join(5000)
        server.close()
        assertTrue(got.contains("Subject: [NETMON-UPIT] #4711 status Sever"), got)
        assertFalse(got.contains("0641111111"), "broj telefona ne sme u e-mail")
    }
}
