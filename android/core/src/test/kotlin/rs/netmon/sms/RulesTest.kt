package rs.netmon.sms

import java.io.File
import java.nio.file.Files
import java.util.Date
import kotlin.test.Test
import kotlin.test.assertEquals
import kotlin.test.assertFalse
import kotlin.test.assertTrue

class RulesTest {
    private val cfg = Config(
        host = "mail", user = "netmon", password = "x", allowedSender = "Network Monitor <konzola@firma.rs>",
        recipients = listOf("+381641111111", "0652222222"), adminNumber = "+381643333333",
    )
    private val now = Date(1_800_000_000_000L)

    private fun msg(subject: String, from: String = "konzola@firma.rs", ageMin: Long = 1) =
        Incoming("id", from, subject, Date(now.time - ageMin * 60_000))

    @Test
    fun sendsToRecipients() {
        val a = decide(msg("[NETMON-SMS] NetMon 09:12 PAD JG_SW1 (Jug)"), cfg, now)
        assertEquals(Action.Send(cfg.recipients, "NetMon 09:12 PAD JG_SW1 (Jug)", true), a)
    }

    @Test
    fun replyNewLines() {
        val a = decide(msg("[NETMON-SMS] #7 NetMon komande:\\nlokacija lista - spisak lokacija\\n server lista - spisak servera"), cfg, now)
        assertEquals(Action.Reply("7", "NetMon komande:\nlokacija lista - spisak lokacija\nserver lista - spisak servera"), a)
        // Obaveštenja ostaju u jednom redu (oznaka se ne tumači).
        val n = decide(msg("[NETMON-SMS] PAD a\\nb"), cfg, now) as Action.Send
        assertEquals("PAD a\\nb", n.text)
    }

    @Test
    fun longReplyIsNotTruncated() {
        val list = (1..80).joinToString(", ") { "srv-%02d".format(it) }
        val a = decide(msg("[NETMON-SMS] #42 SERVERI 10:42 (80): $list"), cfg, now)
        assertEquals(Action.Reply("42", "SERVERI 10:42 (80): $list"), a)
        // Obaveštenje ostaje najviše dva SMS-a.
        val n = decide(msg("[NETMON-SMS] $list"), cfg, now) as Action.Send
        assertEquals(SMS_MAX_TEXT, n.text.length)
        assertTrue(n.text.endsWith("..."))
    }

    @Test
    fun dailyCheckGoesToAdminOnly() {
        val a = decide(msg("[NETMON-SMS] PROVERA 06.11.2026 08:00", from = "\"Network Monitor\" <KONZOLA@firma.rs>"), cfg, now)
        assertEquals(Action.Send(listOf("+381643333333"), "NetMon SMS veza OK (PROVERA 06.11.2026 08:00)", false, check = true), a)
    }

    @Test
    fun skipsForeignOldAndUntagged() {
        assertTrue(decide(msg("[NETMON-SMS] PAD", from = "napadac@drugi.rs"), cfg, now) is Action.Skip)
        assertTrue(decide(msg("[NETMON-SMS] PAD", ageMin = 61), cfg, now) is Action.Skip)
        assertTrue(decide(msg("Re: [NETMON-SMS] PAD"), cfg, now) is Action.Skip)
        assertTrue(decide(msg("[NETMON-SMS]   "), cfg, now) is Action.Skip)
    }

    @Test
    fun longTextIsCut() {
        val a = decide(msg("[NETMON-SMS] " + "x".repeat(500)), cfg, now) as Action.Send
        assertEquals(SMS_MAX_TEXT, a.text.length)
    }

    @Test
    fun configProblemsAndNumbers() {
        assertEquals(listOf("+381641111111", "0652222222"), Config.parseNumbers("+381641111111,\n065-222-2222"))
        assertTrue(cfg.problems().isEmpty())
        assertFalse(cfg.copy(recipients = listOf("064abc")).problems().isEmpty())
        assertEquals("+381641111111", cfg.copy(adminNumber = "").admin)
    }

    @Test
    fun rateLimiter() {
        val l = RateLimiter(2)
        assertTrue(l.allow(0))
        assertTrue(l.allow(1000))
        assertFalse(l.allow(2000))
        assertTrue(l.shouldWarn(2000))
        assertFalse(l.shouldWarn(3000))
        assertEquals(RateLimiter.HOUR, l.freeAt())
        assertTrue(l.allow(RateLimiter.HOUR + 1))
    }

    @Test
    fun processedStorePersists() {
        val f = File(Files.createTempDirectory("netmon").toFile(), "ids")
        val s = ProcessedStore(f, keep = 2)
        s.add("a"); s.add("b"); s.add("c")
        val s2 = ProcessedStore(f, keep = 2)
        assertFalse(s2.contains("a"))
        assertTrue(s2.contains("b") && s2.contains("c"))
    }
}
