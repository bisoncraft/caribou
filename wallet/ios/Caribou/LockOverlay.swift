import SwiftUI

// LockStrings are the few native shell strings for the lock / biometric flows.
// The SPA handles its own i18n; these cover only the native surfaces (the lock
// overlay, the biometric prompts, and the enroll sheet), mirroring the Android
// strings.xml entries. iOS localization of these (an .xcstrings catalog) can
// follow the same slice Android did; for now they are English constants,
// matching the rest of the still-unlocalized iOS shell.
enum LockStrings {
    static let overlayTitle = "Caribou"
    static let overlayLocked = "Locked"
    static let overlayUnlocking = "Unlocking…"
    static let overlayUnlock = "Unlock"
    static let overlayUsePassword = "Use password instead"

    static let unlockPromptTitle = "Unlock Caribou"
    static let enrollTitle = "Enable biometric unlock"
    static let enrollMessage = "Confirm your wallet password to store it securely."
    static let passwordPlaceholder = "password"
    static let cancel = "Cancel"
    static let enable = "Enable"
    static let incorrectPassword = "Incorrect password"
    static let biometricEnabled = "Biometric unlock enabled"
    static let enrollmentCancelled = "Enrollment cancelled"
}

// LockOverlay is the opaque native view painted over the WebView during the UI
// gate, so the still-resident session's content is hidden behind a biometric
// check. The iOS analog of Android's buildLockOverlay(). It offers a manual
// retry and a password escape hatch in case the biometric prompt is dismissed
// or the sensor is unusable.
struct LockOverlay: View {
    // unlocking = true during the eviction re-auth: the biometric is the automatic
    // system dialog and the core is re-logging-in, so the overlay is passive (a
    // spinner) with no button — offering an "Unlock" here would fire a second,
    // redundant presence check on top of the automatic unlock. When false (the UI
    // gate), the overlay offers the retry + password escape hatch.
    let unlocking: Bool
    // retry re-presents the biometric prompt (the UI gate). usePassword drops to
    // full eviction so the user can re-auth with the app password instead.
    let retry: () -> Void
    let usePassword: () -> Void

    var body: some View {
        ZStack {
            // Opaque background so nothing behind the overlay shows or is tappable.
            Color(red: 0x11 / 255, green: 0x14 / 255, blue: 0x17 / 255)
                .ignoresSafeArea()
            VStack(spacing: 8) {
                Text(LockStrings.overlayTitle)
                    .font(.system(size: 24, weight: .semibold))
                    .foregroundStyle(.white)
                if unlocking {
                    // Eviction re-auth in flight: passive progress, no tappable
                    // control (the system biometric dialog drives it).
                    ProgressView()
                        .tint(.white)
                        .padding(.top, 16)
                    Text(LockStrings.overlayUnlocking)
                        .font(.callout)
                        .foregroundStyle(Color(red: 0x8a / 255, green: 0x90 / 255, blue: 0x99 / 255))
                        .padding(.top, 8)
                } else {
                    Text(LockStrings.overlayLocked)
                        .font(.callout)
                        .foregroundStyle(Color(red: 0x8a / 255, green: 0x90 / 255, blue: 0x99 / 255))
                        .padding(.bottom, 16)
                    Button(action: retry) {
                        Text(LockStrings.overlayUnlock)
                            .font(.body.weight(.semibold))
                            .padding(.horizontal, 24)
                            .padding(.vertical, 10)
                    }
                    .buttonStyle(.borderedProminent)
                    Button(action: usePassword) {
                        Text(LockStrings.overlayUsePassword)
                            .font(.callout)
                            .foregroundStyle(Color(red: 0x8a / 255, green: 0x90 / 255, blue: 0x99 / 255))
                    }
                    .padding(.top, 8)
                }
            }
            .padding()
        }
        // Swallow taps so nothing behind the overlay is interactable.
        .contentShape(Rectangle())
    }
}

// PrivacyCover is the opaque brand screen shown whenever the app is not active
// (app switcher, an incoming system prompt, transitioning to background). It
// keeps wallet content out of the OS app-switcher snapshot, which iOS captures
// as the scene resigns active — the closest iOS analog of Android's FLAG_SECURE
// for the recents thumbnail. (iOS has no API to block user-initiated
// screenshots the way FLAG_SECURE does; this covers only the automatic
// snapshot.)
struct PrivacyCover: View {
    var body: some View {
        ZStack {
            Color(red: 0x11 / 255, green: 0x14 / 255, blue: 0x17 / 255)
                .ignoresSafeArea()
            Text(LockStrings.overlayTitle)
                .font(.system(size: 24, weight: .semibold))
                .foregroundStyle(.white)
        }
    }
}
