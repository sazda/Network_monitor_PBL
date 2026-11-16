package rs.netmon.sms

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.provider.Telephony
import java.io.File
import java.util.concurrent.ExecutorService
import java.util.concurrent.Executors

/** Zajednički podaci za SMS upite (jedan primerak u procesu). */
object Queries {
    @Volatile
    private var book: QueryBook? = null
    val worker: ExecutorService = Executors.newSingleThreadExecutor()

    fun book(ctx: Context): QueryBook = book ?: synchronized(this) {
        book ?: QueryBook(File(ctx.applicationContext.filesDir, "upiti.txt")).also { book = it }
    }
}

/**
 * Prima SMS sa brojeva iz aplikacije („lokacija PS”, „server lista”,
 * „status”...) i šalje ga konzoli e-mailom kao upit. Odgovor stiže kao
 * obična poruka u sanduče (MailWatcher) i ide samo broju koji je pitao.
 * SMS-ovi sa drugih brojeva se ne diraju.
 */
class SmsReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        if (intent.action != Telephony.Sms.Intents.SMS_RECEIVED_ACTION) return
        val app = ctx.applicationContext
        AppLog.init(app)
        if (!Settings.values(app).enabled) return
        val parts = Telephony.Sms.Intents.getMessagesFromIntent(intent) ?: return
        val bySender = linkedMapOf<String, StringBuilder>()
        for (m in parts) {
            val from = m.originatingAddress ?: continue
            bySender.getOrPut(from) { StringBuilder() }.append(m.messageBody ?: "")
        }
        for ((from, body) in bySender) {
            val cmd = parseCommand(body.toString()) ?: continue
            val cfg = Settings.config(app) ?: return
            if (!allowedNumber(from, cfg)) {
                AppLog.add("SMS sa broja $from nije prosleđen: broj nije u listi")
                continue
            }
            val sms = AndroidSms(app)
            val p = Queries.book(app).add(from, cmd)
            if (p == null) {
                AppLog.add("Upit sa broja $from odbijen: previše upita u poslednjih sat vremena")
                if (Queries.book(app).shouldWarn(from)) {
                    Queries.worker.execute { sms.send(from, "NetMon: previse upita, pokusajte kasnije") }
                }
                continue
            }
            Queries.worker.execute {
                try {
                    QueryMailer(cfg).send(p)
                    AppLog.add("Upit #${p.id} „$cmd” sa broja $from poslat konzoli")
                } catch (e: Exception) {
                    Queries.book(app).take(p.id)
                    AppLog.add("Upit #${p.id} nije poslat: ${MailWatcher.describe(e)}")
                    sms.send(from, "NetMon: upit nije poslat konzoli (nema veze sa mail serverom)")
                }
            }
        }
    }
}
