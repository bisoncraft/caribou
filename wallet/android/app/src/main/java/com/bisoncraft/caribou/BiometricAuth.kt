package com.bisoncraft.caribou

import android.content.Context
import android.os.Build
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyPermanentlyInvalidatedException
import android.security.keystore.KeyProperties
import android.util.Base64
import androidx.annotation.VisibleForTesting
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
// plaintext password is only recoverable behind a live biometric (or, on API
// 30+, the device credential). Only the ciphertext and IV live in app prefs;
// both are useless without the Keystore key.
//
// setInvalidatedByBiometricEnrollment only bites where the key is valid for
// biometric auth alone - the platform's own wording is that it applies when the
// key is "valid for biometric authentication only". Authorizing the device
// credential alongside the biometric silently voids it: measured on a Pixel 9a
// (API 37, GrapheneOS), where enrolling a new fingerprint left a
// credential-authorized key usable while a biometric-only key built alongside it
// was invalidated. Removing a print is not an enrollment and invalidates nothing. That is not an escalation - enrolling a biometric requires the
// device credential, and the credential already unlocks such a key directly - but
// it does mean the two modes differ in more than convenience, so enroll takes the
// choice as allowCredential:
//
//   allowCredential = true (default, and forced below API 30 or with no biometric
//     enrolled): a PIN/pattern can release the password, and a newly-enrolled
//     fingerprint can unlock an existing wallet. Wallet security equals device
//     credential security.
//   allowCredential = false: biometric-only, so a new enrollment kills the key -
//     unlock clears the stored secret and the caller falls back to password login,
//     and a newly-enrolled biometric cannot unlock an existing wallet. This is what
//     iOS does unconditionally. The KeyPermanentlyInvalidatedException paths in
//     enroll/unlock only ever fire in this mode.
//
// The choice binds at generation and cannot be changed on a live key, so it lives
// on the enroll dialog rather than in Settings; keyAllowsCredential records which
// mode the stored key is in.
//
// keyName, prefsName, and userAuthRequired are injectable so instrumented tests
// can drive the real keystore and cipher paths against a throwaway alias -
// without touching a user's live enrollment, and (with userAuthRequired = false)
// without a live biometric prompt so the AES/GCM round-trip runs headless.
// Production always uses the defaults.
class BiometricAuth(
    context: Context,
    private val keyName: String = KEY_NAME,
    prefsName: String = PREFS_NAME,
    private val userAuthRequired: Boolean = true,
) {
    private val ctx = context.applicationContext
    private val prefs = ctx.getSharedPreferences(prefsName, Context.MODE_PRIVATE)

    companion object {
        private const val KEY_NAME = "caribou_pw_key"
        private const val PREFS_NAME = "caribou_biometric"
        private const val KEYSTORE = "AndroidKeyStore"
        private const val PREF_CT = "ct" // base64 ciphertext
        private const val PREF_IV = "iv" // base64 GCM IV
        private const val PREF_CRED = "cred" // device credential authorized for the key
        private const val GCM_TAG_BITS = 128

        // The device credential (PIN/pattern/password) can only be combined with
        // a CryptoObject from API 30 on. Below that the key and its prompt are
        // biometric-only whatever the user picks, and the prompt's Cancel button
        // drops them back to the SPA password screen.
        private val credentialUsable = Build.VERSION.SDK_INT >= Build.VERSION_CODES.R
    }

    // authenticators maps a fallback choice onto the constants that a key spec and
    // its prompt must agree on: build a key for one set and prompt with another and
    // the crypto operation fails at runtime. Below API 30 the choice is moot.
    private fun authenticators(allowCredential: Boolean): Int =
        if (allowCredential && credentialUsable) {
            BiometricManager.Authenticators.BIOMETRIC_STRONG or
                BiometricManager.Authenticators.DEVICE_CREDENTIAL
        } else {
            BiometricManager.Authenticators.BIOMETRIC_STRONG
        }

    // gateAuthenticators is what the presence-only UI gate accepts. The gate reveals
    // an already-resident session and decrypts nothing, so it takes the credential
    // wherever the platform allows, independent of the key's mode. This matches iOS,
    // where the passcode clears the gate but cannot release the stored password. The
    // consequence is that opting out of the fallback protects the eviction tier, not
    // the gate window.
    private fun gateAuthenticators(): Int = authenticators(allowCredential = true)

    // keyAllowsCredential reports whether the stored key was generated with the
    // device credential authorized. It describes the key, not a live preference:
    // authorizations are frozen at generation, so unlock must build its prompt from
    // this rather than from what the user might want today. Written by
    // getOrCreateKey when the key is minted and dropped by clear with it.
    private fun keyAllowsCredential(): Boolean = prefs.getBoolean(PREF_CRED, false)

    // isEnrolled reports whether a password has been stored for biometric unlock.
    fun isEnrolled(): Boolean =
        prefs.contains(PREF_CT) && prefs.contains(PREF_IV) && keyExists()

    // canAuthenticate reports whether the device can authenticate the user - an
    // enrolled biometric, or a device credential on API 30+ - so the SPA can
    // decide whether to offer the opt-in toggle.
    fun canAuthenticate(context: Context): Boolean =
        BiometricManager.from(context).canAuthenticate(gateAuthenticators()) ==
            BiometricManager.BIOMETRIC_SUCCESS

    // canEnrollWithoutCredential reports whether a biometric-only enrollment is
    // even possible here, i.e. whether a strong biometric is actually enrolled. A
    // setUserAuthenticationRequired key with no credential authorization cannot be
    // generated without one, so offering that choice on a passcode-only device
    // would just produce a keygen failure at Enable.
    fun canEnrollWithoutCredential(context: Context): Boolean =
        BiometricManager.from(context).canAuthenticate(
            BiometricManager.Authenticators.BIOMETRIC_STRONG,
        ) == BiometricManager.BIOMETRIC_SUCCESS

    // credentialFallbackApplies reports whether the fallback choice means anything
    // on this device. Below API 30 the credential cannot back a CryptoObject at
    // all, so the key is biometric-only regardless and there is nothing to ask.
    fun credentialFallbackApplies(): Boolean = credentialUsable

    // clear removes the stored secret and its Keystore key (opt-out, or after a
    // key invalidation).
    fun clear() {
        prefs.edit().remove(PREF_CT).remove(PREF_IV).remove(PREF_CRED).apply()
        try {
            keystore().deleteEntry(keyName)
        } catch (_: Exception) {
        }
    }

    // enroll captures pw behind a biometric prompt and stores the ciphertext.
    // allowCredential authorizes the device credential to release the password
    // later, alongside a biometric. Turning it off is the more secure choice: it is
    // what makes setInvalidatedByBiometricEnrollment bite (see the note at the top
    // of this file), so a newly-enrolled fingerprint can no longer unlock an
    // existing wallet - at the cost of no way back in if the sensor fails. It is
    // ignored below API 30, where the key is biometric-only either way.
    // onResult(true) on success, onResult(false) on cancel/error. Must be called
    // on the main thread (BiometricPrompt requirement). The caller owns pw and
    // should zero it afterward.
    fun enroll(
        activity: AppCompatActivity,
        pw: ByteArray,
        allowCredential: Boolean,
        onResult: (Boolean) -> Unit,
    ) {
        // A leftover key built for the other mode cannot be reused, since its
        // authorizations are frozen: getOrCreateKey would hand it back and the
        // prompt would then offer an authenticator the key rejects. Drop it. Safe
        // because enroll is only reached while not enrolled - the Settings toggle
        // routes an on-to-off flip to clear - so there is no live secret to lose.
        if (keyExists() && keyAllowsCredential() != allowCredential) clear()
        val cipher = try {
            initEncryptCipher(allowCredential)
        } catch (_: KeyPermanentlyInvalidatedException) {
            // An enrollment change killed the key. Drop it and make a fresh one:
            // getOrCreateKey would otherwise keep handing back the dead key,
            // failing every future enroll with no in-app way out. Nothing is lost
            // - enroll overwrites the stored secret anyway.
            clear()
            try {
                initEncryptCipher(allowCredential)
            } catch (_: Exception) {
                onResult(false); return
            }
        } catch (_: Exception) {
            onResult(false); return
        }
        authenticate(activity, cipher, ctx.getString(R.string.enroll_title),
            authenticators(allowCredential),
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
        // The prompt must offer exactly what the key was minted for, not what the
        // user might prefer now.
        authenticate(activity, cipher, ctx.getString(R.string.unlock_prompt_title),
            authenticators(keyAllowsCredential()),
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
        runPrompt(activity, ctx.getString(R.string.unlock_prompt_title), crypto = null,
            auths = gateAuthenticators(),
            onError = { onResult(false) },
            onSuccess = { onResult(true) })
    }

    private fun authenticate(
        activity: AppCompatActivity,
        cipher: Cipher,
        title: String,
        auths: Int,
        onError: () -> Unit,
        onSuccess: (Cipher) -> Unit,
    ) {
        runPrompt(activity, title, BiometricPrompt.CryptoObject(cipher), auths, onError) { result ->
            val c = result.cryptoObject?.cipher
            if (c == null) onError() else onSuccess(c)
        }
    }

    // runPrompt builds and shows a BiometricPrompt wired to onError/onSuccess,
    // sharing the executor and callback that the crypto path (authenticate) and
    // the presence-only UI gate (authenticateSimple) both need. Pass a
    // CryptoObject to bind a keystore Cipher, or null for the gate, which decrypts
    // nothing. A single non-match is swallowed so the system prompt stays up for a
    // retry.
    private fun runPrompt(
        activity: AppCompatActivity,
        title: String,
        crypto: BiometricPrompt.CryptoObject?,
        auths: Int,
        onError: () -> Unit,
        onSuccess: (BiometricPrompt.AuthenticationResult) -> Unit,
    ) {
        val prompt = BiometricPrompt(
            activity,
            ContextCompat.getMainExecutor(activity),
            object : BiometricPrompt.AuthenticationCallback() {
                override fun onAuthenticationError(code: Int, msg: CharSequence) = onError()
                override fun onAuthenticationFailed() {}
                override fun onAuthenticationSucceeded(result: BiometricPrompt.AuthenticationResult) =
                    onSuccess(result)
            })
        if (crypto != null) prompt.authenticate(promptInfo(title, auths), crypto)
        else prompt.authenticate(promptInfo(title, auths))
    }

    // promptInfo builds the prompt configuration. When the device credential is
    // an allowed authenticator it also serves as the fallback control, and a
    // negative button must not be set (the two are mutually exclusive); otherwise
    // a Cancel button is required.
    private fun promptInfo(title: String, auths: Int): BiometricPrompt.PromptInfo {
        val b = BiometricPrompt.PromptInfo.Builder()
            .setTitle(title)
            .setAllowedAuthenticators(auths)
        val hasCredential = auths and BiometricManager.Authenticators.DEVICE_CREDENTIAL != 0
        if (!hasCredential) b.setNegativeButtonText(ctx.getString(R.string.cancel))
        return b.build()
    }

    // allowCredential defaults to false so the tests, which build headless keys
    // with userAuthRequired = false, need not care: that path sets no
    // authentication parameters at all, making the mode irrelevant.
    @VisibleForTesting
    internal fun initEncryptCipher(allowCredential: Boolean = false): Cipher =
        Cipher.getInstance(TRANSFORMATION).apply {
            init(Cipher.ENCRYPT_MODE, getOrCreateKey(allowCredential))
        }

    @VisibleForTesting
    internal fun initDecryptCipher(iv: ByteArray): Cipher =
        Cipher.getInstance(TRANSFORMATION).apply {
            init(Cipher.DECRYPT_MODE, getKey(), GCMParameterSpec(GCM_TAG_BITS, iv))
        }

    private fun keystore(): KeyStore = KeyStore.getInstance(KEYSTORE).apply { load(null) }

    @VisibleForTesting
    internal fun keyExists(): Boolean = try {
        keystore().containsAlias(keyName)
    } catch (_: Exception) {
        false
    }

    private fun getKey(): SecretKey = keystore().getKey(keyName, null) as SecretKey

    @VisibleForTesting
    internal fun getOrCreateKey(allowCredential: Boolean = false): SecretKey {
        if (keyExists()) return getKey()
        val gen = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, KEYSTORE)
        val spec = KeyGenParameterSpec.Builder(
            keyName,
            KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT,
        )
            .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
            .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
        if (userAuthRequired) {
            spec.setUserAuthenticationRequired(true)
                .setInvalidatedByBiometricEnrollment(true)
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                // Authorize the key per operation (timeout 0) for exactly what its
                // prompt will offer. Including AUTH_DEVICE_CREDENTIAL is what lets
                // a PIN/pattern unlock decrypt - and also what silently voids the
                // setInvalidatedByBiometricEnrollment above, so the caller's choice
                // decides both at once.
                val auths = if (allowCredential) {
                    KeyProperties.AUTH_BIOMETRIC_STRONG or KeyProperties.AUTH_DEVICE_CREDENTIAL
                } else {
                    KeyProperties.AUTH_BIOMETRIC_STRONG
                }
                spec.setUserAuthenticationParameters(0, auths)
            }
        }
        gen.init(spec.build())
        val key = gen.generateKey()
        // Record the mode only once the key exists, and keyed to the key rather
        // than to the secret: a cancelled enroll leaves the key behind with no
        // ciphertext, and the next attempt must still be able to tell what this
        // key will accept.
        prefs.edit().putBoolean(PREF_CRED, allowCredential).apply()
        return key
    }

    private fun b64(b: ByteArray) = Base64.encodeToString(b, Base64.NO_WRAP)
    private fun unb64(s: String) = Base64.decode(s, Base64.NO_WRAP)
}

private const val TRANSFORMATION = "AES/GCM/NoPadding"
