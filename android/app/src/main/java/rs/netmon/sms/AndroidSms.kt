package rs.netmon.sms

import android.app.Activity
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.os.Build
import android.telephony.SmsManager
import java.util.concurrent.CountDownLatch
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger

/**
 * Slanje SMS-a preko SIM kartice telefona. Čeka potvrdu mreže da je poruka
 * predata (ne i da je isporučena primaocu).
 */
class AndroidSms(private val ctx: Context) : SmsSender {

    @Suppress("DEPRECATION")
    private fun manager(): SmsManager =
        if (Build.VERSION.SDK_INT >= 31) ctx.getSystemService(SmsManager::class.java) else SmsManager.getDefault()

    override fun send(number: String, text: String): String? {
        val sm = runCatching { manager() }.getOrElse { return "SMS nije dostupan: ${it.message}" }
        val parts = sm.divideMessage(text)
        val action = ACTION + "." + seq.incrementAndGet()
        val latch = CountDownLatch(parts.size)
        val errors = mutableListOf<String>()
        val receiver = object : BroadcastReceiver() {
            override fun onReceive(c: Context, i: Intent) {
                if (resultCode != Activity.RESULT_OK) synchronized(errors) { errors += describe(resultCode) }
                latch.countDown()
            }
        }
        if (Build.VERSION.SDK_INT >= 33) {
            ctx.registerReceiver(receiver, IntentFilter(action), Context.RECEIVER_NOT_EXPORTED)
        } else {
            ctx.registerReceiver(receiver, IntentFilter(action))
        }
        try {
            val sent = ArrayList<PendingIntent>()
            for (k in parts.indices) {
                val i = Intent(action).setPackage(ctx.packageName)
                sent += PendingIntent.getBroadcast(ctx, seq.incrementAndGet(), i, PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_ONE_SHOT)
            }
            try {
                if (parts.size == 1) sm.sendTextMessage(number, null, text, sent[0], null)
                else sm.sendMultipartTextMessage(number, null, parts, sent, null)
            } catch (e: SecurityException) {
                return "nema dozvole za slanje SMS-a"
            } catch (e: Exception) {
                return e.message ?: e.javaClass.simpleName
            }
            if (!latch.await(90, TimeUnit.SECONDS)) return "mreža nije potvrdila slanje (90 s)"
            synchronized(errors) { return errors.firstOrNull() }
        } finally {
            runCatching { ctx.unregisterReceiver(receiver) }
        }
    }

    companion object {
        private const val ACTION = "rs.netmon.sms.SENT"
        private val seq = AtomicInteger((System.currentTimeMillis() % 100000).toInt())

        fun describe(code: Int): String = when (code) {
            SmsManager.RESULT_ERROR_GENERIC_FAILURE -> "opšta greška pri slanju (kredit, SIM?)"
            SmsManager.RESULT_ERROR_NO_SERVICE -> "nema mobilne mreže"
            SmsManager.RESULT_ERROR_NULL_PDU -> "greška poruke"
            SmsManager.RESULT_ERROR_RADIO_OFF -> "radio je isključen (avionski režim?)"
            else -> "greška pri slanju (kod $code)"
        }
    }
}
