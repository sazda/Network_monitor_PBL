package rs.netmon.sms

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

/** Podešavanja u SharedPreferences; lozinka sanduča je šifrovana ključem iz Android Keystore-a. */
object Settings {
    private const val PREFS = "netmon"
    private const val KEY_ALIAS = "netmon-sms-lozinka"

    data class Values(
        val host: String = "",
        val port: Int = 993,
        val user: String = "",
        val passwordSet: Boolean = false,
        val sender: String = "",
        val numbers: String = "",
        val admin: String = "",
        val maxPerHour: Int = 20,
        val enabled: Boolean = false,
        val checkDeadline: String = "08:30",
        val smtpPort: Int = 465,
    )

    fun values(ctx: Context): Values {
        val p = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE)
        return Values(
            host = p.getString("host", "") ?: "",
            port = p.getInt("port", 993),
            user = p.getString("user", "") ?: "",
            passwordSet = !p.getString("pass", null).isNullOrEmpty(),
            sender = p.getString("sender", "") ?: "",
            numbers = p.getString("numbers", "") ?: "",
            admin = p.getString("admin", "") ?: "",
            maxPerHour = p.getInt("max_per_hour", 20),
            enabled = p.getBoolean("enabled", false),
            checkDeadline = p.getString("check_deadline", "08:30") ?: "",
            smtpPort = p.getInt("smtp_port", 465),
        )
    }

    /** [password] null = bez promene. */
    fun save(ctx: Context, v: Values, password: String?) {
        val e = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit()
            .putString("host", v.host).putInt("port", v.port).putString("user", v.user)
            .putString("sender", v.sender).putString("numbers", v.numbers).putString("admin", v.admin)
            .putInt("max_per_hour", v.maxPerHour).putBoolean("enabled", v.enabled)
            .putString("check_deadline", v.checkDeadline).putInt("smtp_port", v.smtpPort)
        if (password != null) e.putString("pass", encrypt(password))
        e.apply()
    }

    fun setEnabled(ctx: Context, on: Boolean) {
        ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).edit().putBoolean("enabled", on).apply()
    }

    /** Podešavanja za rad (null ako lozinka ne može da se pročita). */
    fun config(ctx: Context): Config? {
        val v = values(ctx)
        val enc = ctx.getSharedPreferences(PREFS, Context.MODE_PRIVATE).getString("pass", null) ?: return null
        val pass = runCatching { decrypt(enc) }.getOrNull() ?: return null
        return Config(
            host = v.host, port = v.port, user = v.user, password = pass, allowedSender = v.sender,
            recipients = Config.parseNumbers(v.numbers), adminNumber = v.admin.trim(), maxPerHour = v.maxPerHour,
            checkDeadline = v.checkDeadline.trim(), smtpPort = v.smtpPort,
        )
    }

    private fun key(): SecretKey {
        val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        (ks.getKey(KEY_ALIAS, null) as? SecretKey)?.let { return it }
        val gen = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore")
        gen.init(
            KeyGenParameterSpec.Builder(KEY_ALIAS, KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .setKeySize(256)
                .build(),
        )
        return gen.generateKey()
    }

    private fun encrypt(plain: String): String {
        val c = Cipher.getInstance("AES/GCM/NoPadding")
        c.init(Cipher.ENCRYPT_MODE, key())
        val out = c.iv + c.doFinal(plain.toByteArray(Charsets.UTF_8))
        return Base64.encodeToString(out, Base64.NO_WRAP)
    }

    private fun decrypt(enc: String): String {
        val raw = Base64.decode(enc, Base64.NO_WRAP)
        val c = Cipher.getInstance("AES/GCM/NoPadding")
        c.init(Cipher.DECRYPT_MODE, key(), GCMParameterSpec(128, raw, 0, 12))
        return String(c.doFinal(raw, 12, raw.size - 12), Charsets.UTF_8)
    }
}
