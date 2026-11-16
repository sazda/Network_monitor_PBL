package rs.netmon.sms

import android.app.AlarmManager
import android.app.Notification
import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import android.os.SystemClock
import java.io.File
import java.util.concurrent.Executors
import java.util.concurrent.ScheduledExecutorService
import java.util.concurrent.TimeUnit

/**
 * Stalni servis: drži IMAP vezu (MailWatcher) i šalje SMS. Radi kao
 * „foreground” servis sa obaveštenjem, da ga Android ne ugasi.
 */
class SmsService : Service() {
    private var watcher: MailWatcher? = null
    private var thread: Thread? = null
    private var wake: PowerManager.WakeLock? = null
    private var guardTimer: ScheduledExecutorService? = null

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onCreate() {
        super.onCreate()
        AppLog.init(this)
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_STOP) {
            stopWatcher()
            stopSelf()
            return START_NOT_STICKY
        }
        goForeground()
        if (intent?.action == ACTION_RESTART) stopWatcher()
        if (thread?.isAlive != true) startWatcher()
        return START_STICKY
    }

    private fun goForeground() {
        val nm = getSystemService(NotificationManager::class.java)
        nm.createNotificationChannel(NotificationChannel(CHANNEL, "NetMon SMS", NotificationManager.IMPORTANCE_LOW))
        val open = PendingIntent.getActivity(this, 0, Intent(this, MainActivity::class.java), PendingIntent.FLAG_IMMUTABLE)
        val n = Notification.Builder(this, CHANNEL)
            .setSmallIcon(android.R.drawable.stat_notify_chat)
            .setContentTitle("NetMon SMS radi")
            .setContentText("Prati sanduče i prosleđuje obaveštenja kao SMS")
            .setOngoing(true)
            .setContentIntent(open)
            .build()
        if (Build.VERSION.SDK_INT >= 34) {
            startForeground(NOTIFICATION_ID, n, ServiceInfo.FOREGROUND_SERVICE_TYPE_SPECIAL_USE)
        } else {
            startForeground(NOTIFICATION_ID, n)
        }
    }

    private fun startWatcher() {
        val cfg = Settings.config(this)
        val problems = cfg?.problems() ?: listOf("lozinka nije sačuvana")
        if (cfg == null || problems.isNotEmpty()) {
            AppLog.add("Ne mogu da pokrenem: ${problems.joinToString(", ")}")
            stopSelf()
            return
        }
        if (wake == null) {
            // Telefon je stalno na punjaču: procesor ne sme da zaspi dok čekamo poštu.
            wake = getSystemService(PowerManager::class.java)
                .newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "netmon:imap").apply { acquire() }
        }
        val sms = AndroidSms(this)
        val guard = checkGuard(this)
        val book = Queries.book(this)
        val w = MailWatcher(cfg, sms, ProcessedStore(File(filesDir, "obradjeno.txt")), AppLog::add,
            onCheck = guard::received, takeQuery = book::take)
        watcher = w
        thread = Thread({ w.run() }, "netmon-imap").apply { start() }
        // Ako dnevna provera konzole ne stigne do zadatog vremena, telefon sam
        // javlja administratoru (radi i bez interneta, SMS ide preko SIM-a).
        guardTimer = Executors.newSingleThreadScheduledExecutor().apply {
            // Upiti bez odgovora konzole (3 min): javlja se onome ko je pitao.
            scheduleWithFixedDelay({
                runCatching {
                    for (q in book.expired(QUERY_TIMEOUT_MS)) {
                        AppLog.add("Konzola nije odgovorila na upit #${q.id} „${q.text}” (${q.number})")
                        sms.send(q.number, "NetMon: konzola nije odgovorila na upit \"${q.text}\" za 3 min - proverite konzolu i mail server")
                    }
                }
            }, 45, 30, TimeUnit.SECONDS)
        }
        if (cfg.checkDeadline.isNotBlank()) {
            guardTimer?.apply {
                scheduleWithFixedDelay({
                    runCatching {
                        val text = guard.due(cfg.checkDeadline) ?: return@runCatching
                        val err = sms.send(cfg.admin, text)
                        if (err == null) {
                            guard.alerted()
                            AppLog.add("Dnevna provera nije stigla do ${cfg.checkDeadline} – upozorenje poslato na ${cfg.admin}")
                        } else {
                            AppLog.add("Dnevna provera nije stigla; upozorenje nije poslato ($err), ponovo za minut")
                        }
                    }
                }, 30, 60, TimeUnit.SECONDS)
            }
        }
        AppLog.add("Pokrenuto (${cfg.recipients.size} brojeva, najviše ${cfg.maxPerHour} SMS/h" +
            (if (cfg.checkDeadline.isNotBlank()) ", provera do ${cfg.checkDeadline})" else ")"))
    }

    private fun stopWatcher() {
        guardTimer?.shutdownNow()
        guardTimer = null
        watcher?.stop()
        thread?.join(5_000)
        watcher = null
        thread = null
    }

    override fun onDestroy() {
        stopWatcher()
        wake?.let { if (it.isHeld) it.release() }
        wake = null
        super.onDestroy()
    }

    companion object {
        const val ACTION_STOP = "rs.netmon.sms.STOP"
        const val QUERY_TIMEOUT_MS = 3 * 60_000L
        const val ACTION_RESTART = "rs.netmon.sms.RESTART"
        private const val CHANNEL = "rad"
        private const val NOTIFICATION_ID = 1

        /** Da li servis radi (za sopstvene servise getRunningServices i dalje radi). */
        @Suppress("DEPRECATION")
        fun isAlive(ctx: Context): Boolean {
            val am = ctx.getSystemService(android.app.ActivityManager::class.java)
            return am.getRunningServices(50).any { it.service.className == SmsService::class.java.name }
        }

        /** Pokreće (ili ponovo pokreće) servis; vraća grešku ili null. */
        fun start(ctx: Context, restart: Boolean = false): String? = try {
            val i = Intent(ctx, SmsService::class.java)
            if (restart) i.action = ACTION_RESTART
            ctx.startForegroundService(i)
            scheduleWatchdog(ctx)
            null
        } catch (e: Exception) {
            "servis nije pokrenut: ${e.message ?: e.javaClass.simpleName}"
        }

        fun stop(ctx: Context) {
            if (isAlive(ctx)) ctx.startService(Intent(ctx, SmsService::class.java).setAction(ACTION_STOP))
        }

        fun checkGuard(ctx: Context) = CheckGuard(File(ctx.filesDir, "provera.txt"))

        /** Na 15 minuta proverava da servis radi (Android ga ponekad ugasi). */
        fun scheduleWatchdog(ctx: Context) {
            val am = ctx.getSystemService(AlarmManager::class.java)
            val pi = PendingIntent.getBroadcast(ctx, 0, Intent(ctx, WatchdogReceiver::class.java), PendingIntent.FLAG_IMMUTABLE)
            am.setInexactRepeating(
                AlarmManager.ELAPSED_REALTIME_WAKEUP, SystemClock.elapsedRealtime() + AlarmManager.INTERVAL_FIFTEEN_MINUTES,
                AlarmManager.INTERVAL_FIFTEEN_MINUTES, pi,
            )
        }
    }
}

/** Posle restarta telefona (ili ažuriranja aplikacije) servis se pokreće sam. */
class BootReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        AppLog.init(ctx)
        if (!Settings.values(ctx).enabled) return
        AppLog.add("Pokretanje posle ${if (intent.action == Intent.ACTION_BOOT_COMPLETED) "restarta telefona" else "ažuriranja"}")
        SmsService.start(ctx)?.let { AppLog.add(it) }
    }
}

/** Provera na 15 minuta: ako je servis ugašen, pokreće ga ponovo. */
class WatchdogReceiver : BroadcastReceiver() {
    override fun onReceive(ctx: Context, intent: Intent) {
        AppLog.init(ctx)
        if (!Settings.values(ctx).enabled || SmsService.isAlive(ctx)) return
        AppLog.add("Servis nije radio – ponovo pokrećem")
        SmsService.start(ctx)?.let { AppLog.add(it) }
    }
}
