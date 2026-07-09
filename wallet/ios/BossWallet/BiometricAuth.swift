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
//
// Enrollment-change handling has a platform wrinkle: .biometryCurrentSet still
// cryptographically invalidates the item when the biometric set changes (the
// secret is never released to a newly-added face/finger), but iOS 15+ no longer
// reports that through a distinct Keychain status — a read of an invalidated
// item returns errSecAuthFailed, the same as a plain mismatch. So the at-rest
// guarantee holds, but to keep isEnrolled() honest (and stop offering a dead
// unlock that always falls back to password) the change is detected out of band:
// snapshot the biometric domain-state hash at enroll time and compare it on read.
// See enrollmentInvalidated.
final class BiometricAuth {
    // service+account identify the single app-password item. Kept device-local
    // and non-syncing via the access group defaults (no kSecAttrSynchronizable).
    // Injectable so tests can drive a throwaway service without touching a real
    // enrollment; production uses the default.
    private let service: String
    private let account = "app-password"

    // Baseline of the enrolled biometric set, captured at enroll time and compared
    // on each isEnrolled() to detect an enrollment change (see enrollmentInvalid-
    // ated). It is an opaque hash, not a secret, so UserDefaults is fine; keyed by
    // service so a test's throwaway service cannot collide with production. Cleared
    // alongside the Keychain item.
    private let defaults = UserDefaults.standard
    private var domainStateKey: String { "bossw_biometry_domain_state.\(service)" }

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
        // iOS 15+ no longer reports an enrollment-change invalidation through a
        // distinct Keychain status (see the type header), so detect it here and
        // drop the now-dead enrollment: the app stops offering a broken unlock and
        // the SPA toggle reflects reality. clear() also removes the stale baseline.
        if enrollmentInvalidated() {
            clear()
            return false
        }
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
        guard SecItemAdd(add as CFDictionary, nil) == errSecSuccess else { return false }
        // Snapshot the current biometric set so a later change is detectable.
        // enroll() has just evaluated biometrics, so a fresh hash is available;
        // if it somehow is not, drop any stale baseline rather than keep an old one.
        if let hash = currentDomainStateHash() {
            defaults.set(hash, forKey: domainStateKey)
        } else {
            defaults.removeObject(forKey: domainStateKey)
        }
        return true
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
                // Any non-success status is a failed release: a cancel, a biometric
                // mismatch, a lockout, or an enrollment-change invalidation. On iOS
                // 15+ these are not distinguishable by status (an invalidated
                // .biometryCurrentSet read returns errSecAuthFailed, the same as a
                // plain mismatch), so we do NOT decide enrollment lifecycle from it
                // — a lockout must not cost the user their enrollment. Just report
                // the failure; a genuine invalidation is caught out of band by
                // enrollmentInvalidated() on the next isEnrolled(), which clears it.
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

    // clear removes the stored secret (opt-out, or after an invalidation) and the
    // domain-state baseline that tracked its enrollment, so the two never drift.
    func clear() {
        SecItemDelete(baseQuery() as CFDictionary)
        defaults.removeObject(forKey: domainStateKey)
    }

    // enrollmentInvalidated reports whether a stored enrollment is now permanently
    // unusable because the enrolled biometric set changed or was removed — the case
    // iOS 15+ no longer surfaces as a distinct Keychain status. It compares the
    // domain-state hash captured at enroll time against the current one:
    //   - current hash readable and differs -> the set changed: invalidated.
    //   - biometrics explicitly not enrolled -> nothing can ever release the item:
    //     invalidated.
    //   - a lockout, or a hash we cannot read right now -> transient/unknown: NOT
    //     invalidated (a lockout must never cost the user their enrollment).
    //   - no baseline stored (never enrolled, or enrolled before this shipped) ->
    //     nothing to compare: not invalidated.
    private func enrollmentInvalidated() -> Bool {
        guard let baseline = defaults.data(forKey: domainStateKey) else { return false }
        let ctx = LAContext()
        var err: NSError?
        if ctx.canEvaluatePolicy(.deviceOwnerAuthenticationWithBiometrics, error: &err) {
            // ctx has passed canEvaluatePolicy, so it can read the hash directly.
            guard let current = domainStateHash(ctx) else { return false }
            return current != baseline
        }
        return err?.domain == LAErrorDomain
            && err?.code == LAError.Code.biometryNotEnrolled.rawValue
    }

    // currentDomainStateHash reads the enrolled-set hash on a fresh context,
    // running canEvaluatePolicy first (it gates availability and, pre-iOS 18, is
    // what populates the property). nil if biometrics are unusable.
    private func currentDomainStateHash() -> Data? {
        let ctx = LAContext()
        guard ctx.canEvaluatePolicy(.deviceOwnerAuthenticationWithBiometrics, error: nil) else {
            return nil
        }
        return domainStateHash(ctx)
    }

    // domainStateHash reads the opaque hash identifying the currently enrolled
    // biometric set off a context that has ALREADY passed canEvaluatePolicy. The
    // hash changes whenever a face/finger is added, removed, or re-enrolled. On
    // iOS 18+ the modern domainState replaces the deprecated property.
    private func domainStateHash(_ ctx: LAContext) -> Data? {
        if #available(iOS 18.0, *) {
            // domainState and .biometry are non-optional; only stateHash is Data?
            // (nil when nothing is enrolled — but the caller has already passed
            // canEvaluatePolicy(.biometrics), so a hash is present here).
            return ctx.domainState.biometry.stateHash
        }
        return legacyDomainState(ctx)
    }

    // legacyDomainState isolates the pre-iOS 18 evaluatedPolicyDomainState so its
    // deprecation warning is confined to this one function and never reaches the
    // iOS 18+ path. Requires a prior canEvaluatePolicy on ctx to be populated.
    @available(iOS, introduced: 15.0, deprecated: 18.0)
    private func legacyDomainState(_ ctx: LAContext) -> Data? {
        ctx.evaluatedPolicyDomainState
    }

    private func baseQuery() -> [String: Any] {
        [
            kSecClass as String: kSecClassGenericPassword,
            kSecAttrService as String: service,
            kSecAttrAccount as String: account,
        ]
    }
}
