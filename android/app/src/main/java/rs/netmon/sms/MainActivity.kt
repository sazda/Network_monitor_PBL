package rs.netmon.sms

import android.Manifest
import android.app.Activity
import android.content.Intent
import android.content.pm.PackageManager
import android.graphics.Typeface
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.PowerManager
import android.provider.Settings as AndroidSettings
import android.text.InputType
import android.view.View
import android.view.WindowInsets
import android.widget.Button
import android.widget.EditText
import android.widget.LinearLayout
import android.widget.ScrollView
import android.widget.TextView

/** Jedan ekran: podešavanja, dugmad za proveru i dnevnik. Bez XML-a i AndroidX biblioteka. */
class MainActivity : Activity() {
    private lateinit var host: EditText
    private lateinit var port: EditText
    private lateinit var user: EditText
    private lateinit var pass: EditText
    private lateinit var sender: EditText
    private lateinit var numbers: EditText
    private lateinit var admin: EditText
    private lateinit var perHour: EditText
    private lateinit var deadline: EditText
    private lateinit var smtpPort: EditText
    private lateinit var status: TextView
    private lateinit var logView: TextView
    private val onLog: () -> Unit = { runOnUiThread { logView.text = AppLog.text() } }

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        AppLog.init(this)
        val dp = resources.displayMetrics.density
        val pad = (16 * dp).toInt()

        val col = LinearLayout(this).apply { orientation = LinearLayout.VERTICAL; setPadding(pad, pad, pad, pad) }
        fun label(t: String, big: Boolean = false) = TextView(this).apply {
            text = t
            textSize = if (big) 20f else 14f
            if (big) setTypeface(typeface, Typeface.BOLD)
            setPadding(0, (8 * dp).toInt(), 0, 0)
        }.also { col.addView(it) }
        fun field(l: String, hint: String, type: Int = InputType.TYPE_CLASS_TEXT): EditText {
            label(l)
            return EditText(this).apply { this.hint = hint; inputType = type; isSingleLine = type and InputType.TYPE_TEXT_FLAG_MULTI_LINE == 0 }
                .also { col.addView(it) }
        }
        fun button(t: String, onClick: () -> Unit) = Button(this).apply { text = t; setOnClickListener { onClick() } }.also { col.addView(it) }

        label("NetMon SMS", big = true)
        status = label("")
        label("Sanduče (IMAP)", big = true)
        host = field("Server", "webmail.firma.rs", InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_URI)
        port = field("Port", "993", InputType.TYPE_CLASS_NUMBER)
        user = field("Nalog", "netmon@firma.rs", InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_EMAIL_ADDRESS)
        pass = field("Lozinka", "", InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD)
        sender = field("Adresa konzole (pošiljalac)", "adresa iz polja „Pošiljalac” u konzoli",
            InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_EMAIL_ADDRESS)
        label("SMS", big = true)
        numbers = field("Brojevi za obaveštenja (jedan po redu)", "+38164…",
            InputType.TYPE_CLASS_PHONE or InputType.TYPE_TEXT_FLAG_MULTI_LINE).apply {
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_FLAG_MULTI_LINE
            minLines = 3
        }
        admin = field("Broj administratora (dnevna provera, upozorenja)", "+38164…", InputType.TYPE_CLASS_PHONE)
        perHour = field("Najviše SMS obaveštenja na sat", "20", InputType.TYPE_CLASS_NUMBER)
        smtpPort = field("SMTP port za upite „status” (465 = TLS)", "465", InputType.TYPE_CLASS_NUMBER)
        deadline = field("Dnevna provera konzole mora stići do (SS:MM, prazno = bez)", "08:30", InputType.TYPE_CLASS_DATETIME or InputType.TYPE_DATETIME_VARIATION_TIME)

        button("Sačuvaj i pokreni") { saveAndStart() }
        button("Zaustavi") { Settings.setEnabled(this, false); SmsService.stop(this); refresh() }
        button("Proveri vezu sa sandučetom") { testConnection() }
        button("Pošalji probni SMS administratoru") { testSms() }
        button("Isključi štednju baterije za aplikaciju") { askBattery() }
        label("Dnevnik", big = true)
        logView = TextView(this).apply { typeface = Typeface.MONOSPACE; textSize = 11f; setTextIsSelectable(true) }
        col.addView(logView)

        val scroll = ScrollView(this).apply { addView(col) }
        // Android 15+ crta preko statusne trake: sadržaj se pomera ispod nje.
        scroll.setOnApplyWindowInsetsListener { v, insets ->
            if (Build.VERSION.SDK_INT >= 30) {
                val b = insets.getInsets(WindowInsets.Type.systemBars() or WindowInsets.Type.ime())
                v.setPadding(b.left, b.top, b.right, b.bottom)
            } else {
                @Suppress("DEPRECATION")
                v.setPadding(insets.systemWindowInsetLeft, insets.systemWindowInsetTop, insets.systemWindowInsetRight, insets.systemWindowInsetBottom)
            }
            insets
        }
        setContentView(scroll)

        load()
        askPermissions()
    }

    override fun onResume() {
        super.onResume()
        AppLog.listen(onLog)
        logView.text = AppLog.text()
        refresh()
    }

    override fun onPause() {
        AppLog.unlisten(onLog)
        super.onPause()
    }

    private fun load() {
        val v = Settings.values(this)
        host.setText(v.host)
        port.setText(v.port.toString())
        user.setText(v.user)
        pass.hint = if (v.passwordSet) "sačuvana (prazno = bez promene)" else ""
        sender.setText(v.sender)
        numbers.setText(Config.parseNumbers(v.numbers).joinToString("\n"))
        admin.setText(v.admin)
        perHour.setText(v.maxPerHour.toString())
        deadline.setText(v.checkDeadline)
        smtpPort.setText(v.smtpPort.toString())
    }

    private fun current(enabled: Boolean) = Settings.Values(
        host = host.text.toString().trim(),
        port = port.text.toString().trim().toIntOrNull() ?: 993,
        user = user.text.toString().trim(),
        sender = sender.text.toString().trim(),
        numbers = Config.parseNumbers(numbers.text.toString()).joinToString("\n"),
        admin = admin.text.toString().replace(" ", "").trim(),
        maxPerHour = perHour.text.toString().trim().toIntOrNull() ?: 20,
        enabled = enabled,
        checkDeadline = deadline.text.toString().trim(),
        smtpPort = smtpPort.text.toString().trim().toIntOrNull() ?: 465,
    )

    private fun save(enabled: Boolean): Config? {
        val pw = pass.text.toString().takeIf { it.isNotEmpty() }
        Settings.save(this, current(enabled), pw)
        pass.setText("")
        load()
        val cfg = Settings.config(this)
        val problems = cfg?.problems() ?: listOf("lozinka nije uneta")
        if (problems.isNotEmpty()) {
            AppLog.add("Podešavanja nisu potpuna: ${problems.joinToString(", ")}")
            return null
        }
        return cfg
    }

    private fun saveAndStart() {
        if (save(enabled = true) == null) {
            Settings.setEnabled(this, false)
            refresh()
            return
        }
        if (!hasSmsPermission()) {
            AppLog.add("Nema dozvole za slanje SMS-a – dozvolite je")
            askPermissions()
        }
        AppLog.add("Podešavanja sačuvana")
        SmsService.start(this, restart = true)?.let { AppLog.add(it) }
        logView.postDelayed({ refresh() }, 1500)
    }

    private fun testConnection() {
        val cfg = save(enabled = Settings.values(this).enabled) ?: return
        AppLog.add("Proveravam vezu sa ${cfg.host}:${cfg.port}…")
        Thread {
            val msg = runCatching {
                MailWatcher(cfg, { _, _ -> "proba" }, ProcessedStore(null), {}).test()
            }.getOrElse { "Veza nije uspela: ${MailWatcher.describe(it)}" }
            AppLog.add(msg)
        }.start()
    }

    private fun testSms() {
        val cfg = save(enabled = Settings.values(this).enabled) ?: return
        if (!hasSmsPermission()) {
            askPermissions()
            return
        }
        AppLog.add("Šaljem probni SMS na ${cfg.admin}…")
        Thread {
            val err = AndroidSms(this).send(cfg.admin, "NetMon SMS: probna poruka sa telefona")
            AppLog.add(if (err == null) "Probni SMS poslat na ${cfg.admin}" else "Probni SMS nije poslat: $err")
        }.start()
    }

    private fun hasSmsPermission() = checkSelfPermission(Manifest.permission.SEND_SMS) == PackageManager.PERMISSION_GRANTED

    private fun askPermissions() {
        val need = mutableListOf<String>()
        if (!hasSmsPermission()) need += Manifest.permission.SEND_SMS
        if (checkSelfPermission(Manifest.permission.RECEIVE_SMS) != PackageManager.PERMISSION_GRANTED) need += Manifest.permission.RECEIVE_SMS
        if (Build.VERSION.SDK_INT >= 33 && checkSelfPermission(Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED) {
            need += Manifest.permission.POST_NOTIFICATIONS
        }
        if (need.isNotEmpty()) requestPermissions(need.toTypedArray(), 1)
    }

    override fun onRequestPermissionsResult(requestCode: Int, permissions: Array<out String>, grantResults: IntArray) {
        super.onRequestPermissionsResult(requestCode, permissions, grantResults)
        if (!hasSmsPermission()) {
            AppLog.add("Dozvola za SMS nije data. Ako je ne nudi: Podešavanja → Aplikacije → NetMon SMS → ⋮ → Dozvoli ograničena podešavanja, pa ponovo.")
        }
        refresh()
    }

    private fun batteryFree() = getSystemService(PowerManager::class.java).isIgnoringBatteryOptimizations(packageName)

    @Suppress("BatteryLife")
    private fun askBattery() {
        if (batteryFree()) {
            AppLog.add("Štednja baterije je već isključena za aplikaciju")
            return
        }
        runCatching {
            startActivity(Intent(AndroidSettings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS, Uri.parse("package:$packageName")))
        }.onFailure { startActivity(Intent(AndroidSettings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS)) }
    }

    private fun refresh() {
        val alive = SmsService.isAlive(this)
        val lines = mutableListOf(
            if (alive) "● Radi" else "○ Ne radi",
            if (hasSmsPermission()) "SMS dozvola: da" else "SMS dozvola: NE",
            if (checkSelfPermission(Manifest.permission.RECEIVE_SMS) == PackageManager.PERMISSION_GRANTED) "Prijem SMS upita (status): da"
            else "Prijem SMS upita (status): NE – dozvolite primanje SMS-a",
            if (batteryFree()) "Štednja baterije: isključena (dobro)" else "Štednja baterije: UKLJUČENA – isključite je",
            SmsService.checkGuard(this).lastReceivedAt().let { t ->
                if (t == 0L) "Dnevna provera: još nije stigla" else "Dnevna provera stigla: " +
                    java.text.SimpleDateFormat("dd.MM. HH:mm", java.util.Locale.ROOT).format(java.util.Date(t))
            },
        )
        status.text = lines.joinToString("\n")
        status.visibility = View.VISIBLE
    }
}
