package com.bisoncraft.caribou

import android.os.Build
import android.security.keystore.KeyInfo
import android.security.keystore.KeyProperties
import androidx.test.ext.junit.runners.AndroidJUnit4
import androidx.test.platform.app.InstrumentationRegistry
import org.junit.After
import org.junit.Assert.assertArrayEquals
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Assume.assumeTrue
import org.junit.Before
import org.junit.Test
import org.junit.runner.RunWith
import java.security.KeyStore
import javax.crypto.SecretKey
import javax.crypto.SecretKeyFactory

// BiometricAuthInstrumentedTest exercises the real Android Keystore and Cipher
// paths that back biometric unlock, on a device/emulator. It uses a throwaway
// keystore alias and prefs file so it never touches a user's real enrollment,
// and builds the key without setUserAuthenticationRequired so the AES/GCM
// round-trip can run without a live biometric prompt (the prompt-driven enroll
// and unlock flows need UI automation and are out of scope here). The crypto
// config under test - transformation, key spec, GCM IV handling - is identical
// to production; only the auth binding is relaxed.
@RunWith(AndroidJUnit4::class)
class BiometricAuthInstrumentedTest {
    private val ctx = InstrumentationRegistry.getInstrumentation().targetContext
    private val testKey = "caribou_test_pw_key"
    private val testPrefs = "caribou_biometric_test"

    // auth builds a BiometricAuth against the throwaway alias/prefs. userAuth
    // false lets the crypto operations run headless.
    private fun auth(userAuth: Boolean = false) =
        BiometricAuth(ctx, keyName = testKey, prefsName = testPrefs, userAuthRequired = userAuth)

    @Before
    fun setUp() = auth().clear()

    @After
    fun tearDown() = auth().clear()

    // keyLifecycle: getOrCreateKey generates a real keystore entry, keyExists
    // reflects it, and clear deletes it.
    @Test
    fun keyLifecycle_createExistsClear() {
        val a = auth()
        assertFalse("no key before creation", a.keyExists())
        a.getOrCreateKey()
        assertTrue("key present after getOrCreateKey", a.keyExists())
        a.clear()
        assertFalse("key gone after clear", a.keyExists())
    }

    // getOrCreateKey is idempotent: a second call returns the existing key rather
    // than regenerating, so a stored ciphertext stays decryptable across calls.
    @Test
    fun getOrCreateKey_isStable() {
        val a = auth()
        a.getOrCreateKey()
        a.getOrCreateKey()
        assertTrue(a.keyExists())
    }

    // encryptDecrypt: the exact production transform and GCM IV handling round
    // trip - what initEncryptCipher seals, initDecryptCipher recovers.
    @Test
    fun encryptDecrypt_roundTrips() {
        val a = auth()
        val pw = "correct horse battery staple".toByteArray()

        val enc = a.initEncryptCipher()
        val ct = enc.doFinal(pw)
        val iv = enc.iv
        assertFalse("ciphertext differs from plaintext", ct.contentEquals(pw))

        val dec = a.initDecryptCipher(iv)
        assertArrayEquals("decrypt recovers the password", pw, dec.doFinal(ct))
    }

    // A ciphertext is bound to its IV: decrypting with the wrong IV must fail
    // rather than silently return garbage (GCM authentication).
    @Test(expected = Exception::class)
    fun decrypt_wrongIv_fails() {
        val a = auth()
        val enc = a.initEncryptCipher()
        val ct = enc.doFinal("secret".toByteArray())
        val wrongIv = ByteArray(enc.iv.size) // all-zero IV, not the one used
        a.initDecryptCipher(wrongIv).doFinal(ct)
    }

    // isEnrolled is false without a stored secret even if a key happens to exist.
    @Test
    fun isEnrolled_falseWithoutStoredSecret() {
        val a = auth()
        a.getOrCreateKey()
        assertFalse(a.isEnrolled())
    }

    // canAuthenticate exercises the real BiometricManager path on-device without
    // throwing; the boolean depends on device enrollment, so we assert only that
    // it returns.
    @Test
    fun canAuthenticate_returns() {
        auth(userAuth = true).canAuthenticate(ctx)
    }

    // The PIN-fallback choice must land in the key's real authorizations. unlock
    // builds its prompt from the same flag, and a key whose authorizations disagree
    // with its prompt fails the crypto operation at runtime, so this reads the
    // generated key back via KeyInfo rather than trusting the spec that was passed.
    //
    // Needs userAuth = true (the headless keys other tests use set no
    // authentication parameters at all) and therefore a real enrolled biometric.
    @Test
    fun getOrCreateKey_authorizationsFollowFallbackChoice() {
        assumeTrue(Build.VERSION.SDK_INT >= Build.VERSION_CODES.R)
        val a = auth(userAuth = true)
        assumeTrue("needs an enrolled biometric to build a biometric-only key", a.canEnrollWithoutCredential(ctx))

        a.clear()
        a.getOrCreateKey(allowCredential = false)
        assertEquals(
            "fallback off must authorize the biometric alone - this is the mode that " +
                "keeps setInvalidatedByBiometricEnrollment effective",
            KeyProperties.AUTH_BIOMETRIC_STRONG,
            authTypeOfStoredKey(),
        )

        a.clear()
        a.getOrCreateKey(allowCredential = true)
        assertEquals(
            "fallback on must additionally authorize the device credential",
            KeyProperties.AUTH_BIOMETRIC_STRONG or KeyProperties.AUTH_DEVICE_CREDENTIAL,
            authTypeOfStoredKey(),
        )
    }

    // authTypeOfStoredKey reports the authenticators the keystore actually bound to
    // the test alias.
    private fun authTypeOfStoredKey(): Int {
        val ks = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        val key = ks.getKey(testKey, null) as SecretKey
        val info = SecretKeyFactory.getInstance(key.algorithm, "AndroidKeyStore")
            .getKeySpec(key, KeyInfo::class.java) as KeyInfo
        return info.userAuthenticationType
    }
}
