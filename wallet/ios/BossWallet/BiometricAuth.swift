import Foundation
import LocalAuthentication
import Security

// BiometricAuth stores the app password in the iOS Keychain so a biometric
// (Face ID / Touch ID) check can release it, letting the shell re-unlock the
// core (App.unlock) without the user retyping their password after an idle
// eviction. It is the iOS analog of Android's BiometricAuth.kt.
//
// The password is written as a generic-password Keychain item whose access
// control requires biometry (.biometryCurrentSet): the Secure Enclave only
// releases the item behind a live Face ID / Touch ID match, and
// .biometryCurrentSet invalidates it the moment the enrolled biometric set
// changes (a new fingerprint/face is added, or biometrics are reset) — the
// analog of Android's setInvalidatedByBiometricEnrollment, so a newly-enrolled
// biometric cannot unlock an existing wallet. kSecAttrAccessibleWhenPasscodeSet-
// ThisDeviceOnly additionally requires a device passcode to exist, never syncs
// the item to iCloud, and binds it to this device, so the secret at rest never
// leaves the phone (the analog of allowBackup=false / a device-bound Keystore
// key).
//
// There is no separate key to manage (unlike Android's Keystore AES/GCM key)
// because on iOS the Keychain item is itself the biometric-gated secret store.
final class BiometricAuth {
    // service+account identify the single app-password item. Kept device-local
    // and non-syncing via the access group defaults (no kSecAttrSynchronizable).
    // Injectable so tests can drive a throwaway service without touching a real
    // enrollment; production uses the default.
    private let service: String
    private let account = "app-password"

    init(service: String = "com.bisoncraft.bosswallet.biometric") {
        self.service = service
    }

    // biometricsAvailable reports whether the device has usable enrolled biometry
    // (Face ID / Touch ID), so the SPA can decide whether to show the opt-in
    // toggle. Enrollment stores behind .biometryCurrentSet, which needs biometry,
    // so the toggle is biometry-gated: a passcode-only device can still run the
    // cheap UI gate (see canGate) but cannot enroll a stored password.
    func biometricsAvailable() -> Bool {
        LAContext().canEvaluatePolicy(.deviceOwnerAuthenticationWithBiometrics, error: nil)
    }

    // canGate reports whether the device can authenticate the user at all —
    // biometry or the device passcode — so the UI gate always has a mechanism to
    // clear, even on a passcode-only device. Mirrors Android's canAuthenticate
    // (BIOMETRIC_STRONG or DEVICE_CREDENTIAL).
    func canGate() -> Bool {
        LAContext().canEvaluatePolicy(.deviceOwnerAuthentication, error: nil)
    }

    // isEnrolled reports whether a password has been stored for biometric unlock.
    // Attribute-only lookup with the auth UI suppressed, so it never prompts.
    func isEnrolled() -> Bool {
        // interactionNotAllowed makes a data read fail fast instead of prompting;
        // we only ask for attributes here, so the lookup never shows UI.
        let context = LAContext()
        context.interactionNotAllowed = true
        var query = baseQuery()
        query[kSecUseAuthenticationContext as String] = context
        query[kSecReturnAttributes as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        var out: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &out)
        // errSecInteractionNotAllowed means the item exists but reading its data
        // needs auth — which still counts as enrolled.
        return status == errSecSuccess || status == errSecInteractionNotAllowed
    }

    // enroll captures pw behind a biometric prompt and stores the ciphertext.
    // completion(true) on success, completion(false) on cancel/error. The caller
    // owns pw. Prompts biometry once up front so opt-in gives immediate feedback
    // and fails fast if biometrics are unusable; writing the item itself does not
    // prompt (only reads do), so this is the only prompt. Runs completion on the
    // main queue.
    func enroll(pw: Data, reason: String, completion: @escaping (Bool) -> Void) {
        let ctx = LAContext()
        ctx.evaluatePolicy(.deviceOwnerAuthenticationWithBiometrics, localizedReason: reason) { ok, _ in
            DispatchQueue.main.async {
                guard ok else { completion(false); return }
                completion(self.store(pw))
            }
        }
    }

    // store writes pw as a biometry-gated Keychain item, replacing any prior one.
    private func store(_ pw: Data) -> Bool {
        guard let access = SecAccessControlCreateWithFlags(
            nil,
            kSecAttrAccessibleWhenPasscodeSetThisDeviceOnly,
            .biometryCurrentSet,
            nil
        ) else { return false }
        // Replace any existing item so re-enroll is clean and idempotent.
        SecItemDelete(baseQuery() as CFDictionary)
        var add = baseQuery()
        add[kSecValueData as String] = pw
        add[kSecAttrAccessControl as String] = access
        return SecItemAdd(add as CFDictionary, nil) == errSecSuccess
    }

    // unlock releases the stored password behind a biometric prompt.
    // completion(pw) on success — the caller MUST zero pw after use — or
    // completion(nil) on cancel/error/invalidation. The Keychain read blocks the
    // calling thread while the system biometric UI is up, so it runs off the main
    // queue; completion is delivered on the main queue.
    func unlock(reason: String, completion: @escaping (Data?) -> Void) {
        // The prompt string rides on the authentication context (kSecUse-
        // OperationPrompt is deprecated); the Keychain presents Face ID / Touch ID
        // when the biometry-gated item is read.
        let context = LAContext()
        context.localizedReason = reason
        DispatchQueue.global(qos: .userInitiated).async {
            var query = self.baseQuery()
            query[kSecReturnData as String] = true
            query[kSecMatchLimit as String] = kSecMatchLimitOne
            query[kSecUseAuthenticationContext as String] = context
            var out: CFTypeRef?
            let status = SecItemCopyMatching(query as CFDictionary, &out)
            DispatchQueue.main.async {
                if status == errSecSuccess, let data = out as? Data {
                    completion(data)
                    return
                }
                // errSecUserCanceled (the user dismissed the prompt) and
                // errSecAuthFailed (a biometric that did not match after the
                // system's own retries) are transient — keep the enrollment for a
                // later retry, mirroring Android swallowing onAuthenticationFailed
                // and only ending on a hard error. Any other status means the
                // stored item is unusable — most often invalidated by a biometric
                // enrollment change (the analog of KeyPermanentlyInvalidated-
                // Exception), where the item reads back as gone — so drop it to
                // flip isEnrolled false and fall back to password login.
                if status != errSecUserCanceled && status != errSecAuthFailed {
                    self.clear()
                }
                completion(nil)
            }
        }
    }

    // authenticateSimple runs a presence check with no Keychain access. It is for
    // the UI gate, which only confirms the user is present before revealing the
    // already-resident session — it releases no secret. Allows the device
    // passcode as well as biometry so the gate can always clear. completion(true)
    // on success, completion(false) on cancel/error, on the main queue.
    func authenticateSimple(reason: String, completion: @escaping (Bool) -> Void) {
        // A fresh context per call so a prior evaluation is never reused.
        LAContext().evaluatePolicy(.deviceOwnerAuthentication, localizedReason: reason) { ok, _ in
            DispatchQueue.main.async { completion(ok) }
        }
    }

    // clear removes the stored secret (opt-out, or after an invalidation).
    func clear() {
        SecItemDelete(baseQuery() as CFDictionary)
    }

    private func baseQuery() -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
    }
}
