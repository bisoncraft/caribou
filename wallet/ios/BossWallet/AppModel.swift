import Foundation
import SwiftUI
import Mobile

// Process-wide bound App handle. The Go core owns on-disk state (DB, storage
// engine) keyed to the data dir, so only one may run per process; this is the
// iOS analog of MainActivity's companion-object App. The gomobile binding is
// safe for concurrent use (lifecycle calls are serialized Go-side), so it lives
// outside the MainActor-isolated model and can be touched from background tasks.
private let boundApp: MobileApp? = MobileNewApp()

// AppModel owns the bound App lifecycle and publishes the loopback URL the
// WebView should load.
@MainActor
final class AppModel: ObservableObject {
    @Published var url: URL?
    @Published var errorMessage: String?

    // Build-time network, read from Info.plist (WalletNet <- WALLET_NET, set per
    // network by Config/<net>.xcconfig). Empty/nil means mainnet. This mirrors
    // Android's BuildConfig.WALLET_NET.
    private let net: String = {
        let n = Bundle.main.object(forInfoDictionaryKey: "WalletNet") as? String
        return (n?.isEmpty == false) ? n! : "mainnet"
    }()

    private var started = false

    // start boots the core off the main actor (wallet boot can be slow) and
    // publishes the loopback bootstrap URL. Idempotent: App.start hands back the
    // live URL if the core is already up, so a re-entrant call just reattaches.
    func start() {
        guard !started else { return }
        started = true
        let net = self.net
        Task.detached(priority: .userInitiated) {
            do {
                let dataDir = try AppModel.dataDir()
                guard let app = boundApp else {
                    throw ShellError.message("failed to create app")
                }
                // App.start returns the loopback bootstrap URL carrying the
                // capability token (?k=...). gomobile binds the Go (string, error)
                // return as a non-throwing method with an explicit NSError out
                // param (the non-null string return doesn't fit Swift's throwing
                // convention), so check the error pointer rather than `try`.
                var startErr: NSError?
                let base = app.start(dataDir, net: net, error: &startErr)
                if let startErr { throw startErr }
                // Hand the build-time network to the SPA as a query param so it
                // can surface a non-mainnet indicator; a wallet must never be
                // mistaken for one on a different network.
                let sep = base.contains("?") ? "&" : "?"
                let full = "\(base)\(sep)net=\(net)"
                guard let parsed = URL(string: full) else {
                    throw ShellError.message("invalid loopback URL")
                }
                await MainActor.run { self.url = parsed }
            } catch {
                await MainActor.run {
                    // Allow a later retry (e.g. on the next foreground).
                    self.started = false
                    self.errorMessage = error.localizedDescription
                }
            }
        }
    }

    // scenePhaseChanged is the seam for the future session-lock work (see
    // ~/ocean/bossmobile/biometrics.md). background/active are high-frequency OS
    // events (every app switch, notification, screen-off); they must NOT
    // themselves lock the wallet, or the user re-auths dozens of times a day.
    // They are meant to feed an idle timer that gates re-auth. v1 deliberately
    // keeps the core running across background/foreground (matching Android,
    // which only tears down on a real finish), so this currently does nothing
    // but mark where that logic lands.
    func scenePhaseChanged(_ phase: ScenePhase) {
        switch phase {
        case .background:
            // TODO(biometrics): stamp "left at" and start the idle/eviction
            // timer here. Do NOT call stop() — that would lock on every app
            // switch.
            break
        case .active:
            // TODO(biometrics): on return, check the idle timer and gate (paint
            // the biometric overlay) if the timeout elapsed; if the core was
            // evicted, retrieve the password from the Keychain and Unlock. start()
            // already reattaches to the live core.
            break
        default:
            break
        }
    }

    // stopForTermination tears the core down (locks wallets, then closes the
    // txDB/storage engine in order). Wired to willTerminate as best-effort
    // cleanup; the call is idempotent Go-side.
    static func stopForTermination() {
        try? boundApp?.stop()
    }

    // dataDir returns the app-private storage path passed to App.start. It uses
    // Application Support (app-private, not user-visible) and excludes it from
    // iCloud/iTunes backups so wallet secrets at rest never leave the device —
    // the iOS analog of Android's allowBackup=false.
    nonisolated static func dataDir() throws -> String {
        let fm = FileManager.default
        var dir = try fm.url(for: .applicationSupportDirectory,
                             in: .userDomainMask,
                             appropriateFor: nil,
                             create: true)
        var values = URLResourceValues()
        values.isExcludedFromBackup = true
        do {
            try dir.setResourceValues(values)
        } catch {
            // Non-fatal: the dir is usable either way, so don't abort startup.
            // But this flag is what keeps the (encrypted) wallet DB out of
            // iCloud/iTunes backups, so a silent lapse of that guarantee should
            // at least be visible.
            NSLog("bossw: failed to exclude data dir from backup: \(error)")
        }
        return dir.path
    }

    enum ShellError: LocalizedError {
        case message(String)
        var errorDescription: String? {
            switch self {
            case .message(let m): return m
            }
        }
    }
}
