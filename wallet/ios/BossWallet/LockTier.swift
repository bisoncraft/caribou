import Foundation

// LockTier is the re-auth decision resumeFromBackground makes on a resume, chosen
// from how long the app was backgrounded. It is pulled out of AppModel as a pure,
// dependency-free value (Foundation only) so the tier logic — the security-
// critical part of the session lock — can be unit-tested without AppModel's
// UIKit / WebKit / Go-core dependencies. See LockTierTests.
enum LockTier: Equatable {
    // Under the gate window: a quick app switch, resume the session untouched.
    case none
    // Gate..eviction with an auth mechanism: the cheap UI gate (overlay +
    // biometric/passcode, core still logged in and keys resident, so success is
    // instant).
    case gate
    // Past eviction, or the gate band with no auth mechanism available: full
    // eviction (App.lock + re-auth).
    case evict
}

// lockTier maps idle time (milliseconds spent backgrounded) to the tier to run on
// resume. gateMs must sit strictly below evictMs — setLockConfig's clamp
// guarantees this, so the gate band [gateMs, evictMs) is non-empty. canGate is
// whether the device can authenticate the user at all (biometry or the device
// passcode); without it the gate band escalates to full eviction rather than
// leave the authed session exposed past the gate window — even when eviction is
// set to Never, which assumes the gate is the thing doing the locking.
func lockTier(idleMs: Double, gateMs: Double, evictMs: Double, canGate: Bool) -> LockTier {
    if idleMs >= evictMs { return .evict }
    if idleMs >= gateMs { return canGate ? .gate : .evict }
    return .none
}
