package com.bisoncraft.bosswallet

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyPermanentlyInvalidatedException
import android.security.keystore.KeyProperties
import android.util.Base64
import androidx.appcompat.app.AppCompatActivity
import androidx.biometric.BiometricManager
import androidx.biometric.BiometricPrompt
import androidx.core.content.ContextCompat
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

// BiometricAuth stores the app password so a biometric check can release it,
// letting the shell re-unlock the core (app.unlock) without the user retyping
// their password after an idle eviction.
//
// The password is encrypted with an AES/GCM key held in the Android Keystore.
// The key is marked setUserAuthenticationRequired, so the OS only lets a Cipher
// bound to it run inside a BiometricPrompt CryptoObject that the user just
// authenticated - the key material never leaves the TEE/StrongBox and the
// plaintext password is only recoverable behind a live biometric. Only the
// ciphertext and IV live in app prefs; both are useless without the Keystore key.
//
// setInvalidatedByBiometricEnrollment means adding a new fingerprint/face
// permanently invalidates the key: unlock then clears the stored secret and the
// caller falls back to password login, so a newly-enrolled biometric cannot
// unlock an existing wallet.
class BiometricAuth(context: Context) {
    private val prefs = context.applicationContext
        .getSharedPreferences("bossw_biometric", Context.MODE_PRIVATE)

    companion object {
        private const val KEY_NAME = "bossw_pw_key"
        private const val KEYSTORE = "AndroidKeyStore"
        private const val PREF_CT = "ct" // base64 ciphertext
        private const val PREF_IV = "iv" // base64 GCM IV
        private const val GCM_TAG_BITS = 128

        // BIOMETRIC_STRONG only (Class 3). Device-credential (PIN/pattern)
        // fallback is intentionally omitted for now: mixing DEVICE_CREDENTIAL
        // with a CryptoObject is only supported on API 30+, and minSdk is 21.
        // The prompt's Cancel button drops the user back to the SPA password
        // screen. TODO(biometrics): add device-credential fallback on API 30+.
        private const val AUTHENTICATORS = BiometricManager.Authenticators.BIOMETRIC_STRONG
    }

    // isEnrolled reports whether a password has been stored for biometric unlock.
    fun isEnrolled(): Boolean =
        prefs.contains(PREF_CT) && prefs.contains(PREF_IV) && keyExists()

    // canAuthenticate reports whether the device has usable, enrolled biometric
    // hardware, so the SPA can decide whether to offer the opt-in toggle.
    fun canAuthenticate(context: Context): Boolean =
        BiometricManager.from(context).canAuthenticate(AUTHENTICATORS) ==
            BiometricManager.BIOMETRIC_SUCCESS

    // clear removes the stored secret and its Keystore key (opt-out, or after a
    // key invalidation).
    fun clear() {
        prefs.edit().remove(PREF_CT).remove(PREF_IV).apply()
        try {
            keystore().deleteEntry(KEY_NAME)
        } catch (_: Exception) {
        }
    }

    // enroll captures pw behind a biometric prompt and stores the ciphertext.
    // onResult(true) on success, onResult(false) on cancel/error. Must be called
    // on the main thread (BiometricPrompt requirement). The caller owns pw and
    // should zero it afterward.
    fun enroll(activity: AppCompatActivity, pw: ByteArray, onResult: (Boolean) -> Unit) {
        val cipher = try {
            initEncryptCipher()
        } catch (_: Exception) {
            onResult(false); return
        }
        authenticate(activity, cipher, "Enable biometric unlock",
            onError = { onResult(false) },
            onSuccess = { authed ->
                try {
                    val ct = authed.doFinal(pw)
                    prefs.edit()
                        .putString(PREF_CT, b64(ct))
                        .putString(PREF_IV, b64(authed.iv))
                        .apply()
                    onResult(true)
                } catch (_: Exception) {
                    onResult(false)
                }
            })
    }

    // unlock releases the stored password behind a biometric prompt.
    // onResult(pw) on success - the caller MUST zero pw after use - or
    // onResult(null) on cancel/error/invalidation. Must be called on the main
    // thread.
    fun unlock(activity: AppCompatActivity, onResult: (ByteArray?) -> Unit) {
        val ct = prefs.getString(PREF_CT, null)?.let { unb64(it) }
        val iv = prefs.getString(PREF_IV, null)?.let { unb64(it) }
        if (ct == null || iv == null) {
            onResult(null); return
        }
        val cipher = try {
            initDecryptCipher(iv)
        } catch (_: KeyPermanentlyInvalidatedException) {
            // A biometric enrollment change killed the key. Drop the now-useless
            // secret and fall back to password login.
            clear(); onResult(null); return
        } catch (_: Exception) {
            onResult(null); return
        }
        authenticate(activity, cipher, "Unlock Boss Wallet",
            onError = { onResult(null) },
            onSuccess = { authed ->
                try {
                    onResult(authed.doFinal(ct))
                } catch (_: Exception) {
                    // Decrypt failure means the stored secret is unusable; clear it.
                    clear(); onResult(null)
                }
            })
    }

    // authenticateSimple runs a biometric check with no CryptoObject. It is for
    // the UI gate, which only needs to confirm the user is present before
    // revealing the already-resident session - it decrypts nothing and touches
    // no keystore key. onResult(true) on success, onResult(false) on cancel/error.
    // Must be called on the main thread.
    fun authenticateSimple(activity: AppCompatActivity, onResult: (Boolean) -> Unit) {
        val prompt = BiometricPrompt(
            activity,
            ContextCompat.getMainExecutor(activity),
            object : BiometricPrompt.AuthenticationCallback() {
                override fun onAuthenticationError(code: Int, msg: CharSequence) = onResult(false)
                override fun onAuthenticationFailed() {}
                override fun onAuthenticationSucceeded(result: BiometricPrompt.AuthenticationResult) =
                    onResult(true)
            })
        val info = BiometricPrompt.PromptInfo.Builder()
            .setTitle("Unlock Boss Wallet")
            .setNegativeButtonText("Cancel")
            .setAllowedAuthenticators(AUTHENTICATORS)
            .build()
        prompt.authenticate(info)
    }

    private fun authenticate(
        activity: AppCompatActivity,
        cipher: Cipher,
        title: String,
        onError: () -> Unit,
        onSuccess: (Cipher) -> Unit,
    ) {
        val prompt = BiometricPrompt(
            activity,
            ContextCompat.getMainExecutor(activity),
            object : BiometricPrompt.AuthenticationCallback() {
                override fun onAuthenticationError(code: Int, msg: CharSequence) = onError()

                // A single non-match; the system prompt stays up for a retry.
                override fun onAuthenticationFailed() {}

                override fun onAuthenticationSucceeded(result: BiometricPrompt.AuthenticationResult) {
                    val c = result.cryptoObject?.cipher
                    if (c == null) onError() else onSuccess(c)
                }
            })
        val info = BiometricPrompt.PromptInfo.Builder()
            .setTitle(title)
            .setNegativeButtonText("Cancel")
            .setAllowedAuthenticators(AUTHENTICATORS)
            .build()
        prompt.authenticate(info, BiometricPrompt.CryptoObject(cipher))
    }

    private fun initEncryptCipher(): Cipher =
        Cipher.getInstance(TRANSFORMATION).apply { init(Cipher.ENCRYPT_MODE, getOrCreateKey()) }

    private fun initDecryptCipher(iv: ByteArray): Cipher =
        Cipher.getInstance(TRANSFORMATION).apply {
            init(Cipher.DECRYPT_MODE, getKey(), GCMParameterSpec(GCM_TAG_BITS, iv))
        }

    private fun keystore(): KeyStore = KeyStore.getInstance(KEYSTORE).apply { load(null) }

    private fun keyExists(): Boolean = try {
        keystore().containsAlias(KEY_NAME)
    } catch (_: Exception) {
        false
    }

    private fun getKey(): SecretKey = keystore().getKey(KEY_NAME, null) as SecretKey

    private fun getOrCreateKey(): SecretKey {
        if (keyExists()) return getKey()
        val gen = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, KEYSTORE)
        gen.init(
            KeyGenParameterSpec.Builder(
                KEY_NAME,
                KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT,
            )
                .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .setUserAuthenticationRequired(true)
                .setInvalidatedByBiometricEnrollment(true)
                .build(),
        )
        return gen.generateKey()
    }

    private fun b64(b: ByteArray) = Base64.encodeToString(b, Base64.NO_WRAP)
    private fun unb64(s: String) = Base64.decode(s, Base64.NO_WRAP)
}

private const val TRANSFORMATION = "AES/GCM/NoPadding"
