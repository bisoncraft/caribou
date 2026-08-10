import Foundation
import SwiftUI
import UIKit
import WebKit
import Mobile

// Process-wide bound App handle. The Go core owns on-disk state (DB, storage
// engine) keyed to the data dir, so only one may run per process; this is the
// iOS analog of MainActivity's companion-object App. The gomobile binding is
// safe for concurrent use (lifecycle calls are serialized Go-side), so it lives
// outside the MainActor-isolated model and can be touched from background tasks.
private let boundApp: MobileApp? = MobileNewApp()

// AppModel owns the bound App lifecycle and the native session-lock state
// machine (see ~/ocean/bossmobile/biometrics.md). It publishes the loopback URL
// the WebView loads, the UI-gate overlay flag, and the app-switcher privacy
// cover flag. The lock logic mirrors Android's MainActivity: a two-tier,
// idle-driven gate (a cheap biometric UI gate, then expensive key eviction),
// fed by the SwiftUI scene lifecycle rather than JS.
@MainActor
final class AppModel: ObservableObject {
    @Published var url: URL?
    @Published var errorMessage: String?

    // gated drives the opaque LockOverlay painted over the WebView during the UI
    // gate (and while an eviction re-auth is in flight). obscured drives the
    // PrivacyCover shown whenever the app is not active, so wallet content stays
    // out of the OS app-switcher snapshot.
    @Published var gated = false
    @Published var obscured = false

    // unlocking is true during the eviction re-auth (core locked, biometric
    // release + App.Unlock in flight). The overlay renders differently then: a
    // passive "Unlocking…" spinner rather than the gate's tappable "Unlock"
    // button, because during eviction the biometric is the automatic system
    // dialog + grant-URL reload — a manual button here would fire a second,
    // redundant presence check on top of the automatic unlock.
    @Published var unlocking = false

    // Build-time network, read from Info.plist (WalletNet <- WALLET_NET, set per
    // network by Config/<net>.xcconfig). Empty/nil means mainnet. This mirrors
    // Android's BuildConfig.WALLET_NET.
    private let net: String = {
        let n = Bundle.main.object(forInfoDictionaryKey: "WalletNet") as? String
        return (n?.isEmpty == false) ? n! : "mainnet"
    }()

    private var started = false

    private let biometric = BiometricAuth()

    // Weak refs into the hosted WebView so the lock flow can reload it (eviction),
    // load a post-unlock grant URL, and push bridge state into it. Set by WebView
    // in makeUIView; nil until then and after teardown.
    private weak var webView: WKWebView?
    private weak var userContentController: WKUserContentController?

    // Idle thresholds, persisted in UserDefaults so the SPA Settings pane can tune
    // them via the bridge and they survive restarts. Read fresh each lifecycle
    // callback so a change takes effect on the next background/resume. Two tiers,
    // measured as continuous wall-clock time backgrounded (biometrics.md):
    //   - Gate: the cheap UI gate. Past it a resume paints a biometric overlay but
    //     the core stays logged in and keys stay resident, so clearing it is
    //     instant. A gate of 0 means "require unlock immediately".
    //   - Eviction: the expensive tier. Past it the core is locked (App.lock:
    //     wallets locked, keys zeroed, session torn down). evictionNever disables
    //     it (gate only).
    private let defaults = UserDefaults.standard
    private let kGateMs = "caribou_gate_ms"
    private let kEvictMs = "caribou_evict_ms"
    private let defaultGateMs = 10.0 * 60_000     // 10 min
    private let defaultEvictMs = 60.0 * 60_000    // 1 hour
    private let evictionNever = Double.greatestFiniteMagnitude
    // Pause: the cheapest tier. After a short grace, disconnect wallets (App.pause)
    // so polling stops and the radio can sleep, keeping the session and keys
    // resident so a foreground resume needs no re-auth. The grace only exists to
    // avoid thrashing the network on a quick app switch.
    // Shorter than Android's 30s grace, and necessarily so: here the grace runs
    // inside a beginBackgroundTask assertion, and iOS reclaims that window about
    // 30s after backgrounding. A 30s grace loses that race on essentially every
    // background, dropping the pause onto the expiration handler — which must
    // return promptly and so cannot hold the assertion across App.pause, leaving
    // the disconnect to run as the process is being suspended. At 15s the pause
    // fires with half the window still in hand (measured on an iPhone SE 3rd gen,
    // iOS 26.5: t+15.7s, 14.5s remaining; in the Simulator, a 30s grace had
    // already fallen to the expiration handler, at t+26s with 4.5s left). The
    // window cannot be measured and clamped to instead: backgroundTimeRemaining
    // still reads greatestFiniteMagnitude when the scene phase turns .background,
    // even with the assertion already begun.
    private let pauseMs = 15.0 * 1_000            // 15 sec

    private func gateMs() -> Double { defaults.object(forKey: kGateMs) as? Double ?? defaultGateMs }
    private func evictMs() -> Double { defaults.object(forKey: kEvictMs) as? Double ?? defaultEvictMs }

    // Wall-clock instant the app was last backgrounded (nil = never / foreground).
    private var backgroundedAt: Date?

    // gateGeneration disambiguates overlapping UI-gate biometric prompts. Each
    // prompt snapshots it; a stronger tier taking over (evictAndReauth) or a fresh
    // prompt bumps it, so a stale completion from a superseded prompt cannot
    // clearGate() a gate it no longer owns. There is no LAContext.invalidate() to
    // lean on here, so this is the guard against a late callback dropping the
    // overlay while a newer reauth is still in flight.
    private var gateGeneration = 0

    // revealOnLoad keeps the lock overlay up across an eviction reload: eviction
    // raises the overlay before the (blocking) lock, then loads the grant URL or
    // reloads the logged-out SPA; the overlay only clears once that page finishes
    // loading (onPageFinished), so no authed frame flashes through in between. It
    // does not clear the UI gate, which does no page load.
    private var revealOnLoad = false

    // Best-effort background-eviction plumbing. iOS gives no AlarmManager-style
    // future wakeup while suspended, so these run only within a short background
    // execution window; the real mechanism is the resume-time backstop.
    private var bgTaskId: UIBackgroundTaskIdentifier = .invalid
    private var evictWork: DispatchWorkItem?

    // Background pause state: whether wallets are currently disconnected by the
    // pause tier, the timer that arms it pauseMs after backgrounding, and the
    // background-task assertion that keeps the process alive across the grace.
    // The assertion is the pause tier's own, separate from bgTaskId: eviction's
    // window does not exist at all in the gate-only configuration (evictMs ==
    // never), and beginBgTask/endBgTask manage a single identifier, so sharing
    // one would let either tier tear down the other's window.
    private var paused = false
    private var pauseWork: DispatchWorkItem?
    private var pauseBgTaskId: UIBackgroundTaskIdentifier = .invalid

    // walletLifecycleQueue serializes App.pause against App.resume. Ordering, not
    // just mutual exclusion, is the requirement: the Go App mutex guarantees only
    // that one runs at a time, and two Task.detached calls have no ordering
    // between them. On wake, a pause whose grace elapsed while the process was
    // suspended is dispatched with no defined order against the .active scene
    // phase, so a resume can be issued moments after it. If that resume won the
    // mutex, the pause would land last and leave every wallet disconnected in the
    // foreground, with nothing to re-arm until the next background cycle. A serial
    // queue makes the pair FIFO, so the pause always settles before the resume.
    private let walletLifecycleQueue = DispatchQueue(label: "com.bisoncraft.caribou.wallet-lifecycle",
                                                     qos: .userInitiated)

    // MARK: - Lifecycle

    // start boots the core off the main actor (wallet boot can be slow) and
    // publishes the loopback bootstrap URL. Idempotent: App.start hands back the
    // live URL if the core is already up, so a re-entrant call just reattaches.
    func start() {
        guard !started else { return }
        started = true
        let net = self.net
        // Strong self: this task is short-lived and AppModel lives for the app's
        // lifetime (owned by the @StateObject), so there is no cycle and no leak.
        // AppModel is @MainActor, hence Sendable, so the detached closure can hold
        // it directly.
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
                await MainActor.run {
                    guard let parsed = self.appURL(from: base) else { return }
                    self.url = parsed
                    self.maybeColdStartUnlock()
                }
            } catch {
                await MainActor.run {
                    // Allow a later retry (e.g. on the next foreground).
                    self.started = false
                    self.errorMessage = error.localizedDescription
                }
            }
        }
    }

    // maybeColdStartUnlock offers biometric unlock on a cold start (a fresh launch,
    // where the core boots logged-out) when enrolled, so the user unlocks with
    // Face/Touch ID instead of retyping their password — the "unlock once at launch"
    // from biometrics.md. The boot URL (the logged-out SPA) loads behind the
    // overlay; on success App.Unlock swaps it for an authed grant page, on cancel
    // the overlay drops to reveal the password screen. Unlike the eviction tier
    // there is nothing to lock first, so it goes straight to the biometric prompt.
    private func maybeColdStartUnlock() {
        guard biometric.isEnrolled() else { return }
        gated = true
        unlocking = true
        promptBiometricUnlock(onFallback: { [weak self] in
            // Cancelled/failed: the logged-out SPA is already loaded behind the
            // overlay, so just drop it to reveal the password screen.
            self?.clearGate()
        })
    }

    // scenePhaseChanged feeds the idle timer from the SwiftUI scene lifecycle.
    // background/active are the tier triggers; inactive only raises the privacy
    // cover. Crucially none of these lock on their own — locking is decided by how
    // long we were backgrounded — so normal app switching never re-auths.
    func scenePhaseChanged(_ phase: ScenePhase) {
        switch phase {
        case .background:
            obscured = true
            enterBackground()
        case .inactive:
            // App switcher, an incoming call, or a system prompt (including our
            // own biometric sheet) makes the scene inactive. Cover content so the
            // app-switcher snapshot is clean. Do NOT stamp the idle timer here —
            // only a real background does — so a transient inactive never starts an
            // eviction countdown or double-triggers the resume tiers.
            obscured = true
        case .active:
            resumeFromBackground()
            obscured = false
        @unknown default:
            break
        }
    }

    // enterBackground stamps the time and arms the best-effort pause and eviction
    // timers. Pause is armed first and unconditionally, so wallets disconnect even
    // when eviction is set to Never (the gate-only configuration).
    private func enterBackground() {
        backgroundedAt = Date()
        schedulePause()
        let evict = evictMs()
        guard evict != evictionNever else { return }
        scheduleEviction(afterMs: evict)
    }

    // schedulePause arms the background pause timer under its own background-task
    // assertion. The assertion is what makes the tier work at all: without one iOS
    // suspends the process within a few seconds of .background, so a pauseMs grace
    // would never elapse and the wallets would never disconnect — least of all in
    // the gate-only configuration this tier most needs to cover, where eviction is
    // Never and holds no window of its own. Measured with the assertion removed
    // and no eviction window (iPhone SE 3rd gen, iOS 26.5): execution stopped
    // between 1 and 2s after .background and did not resume for 89s, until
    // foreground, at which point the long-elapsed grace fired the pause in the
    // foreground instead — wallets stayed connected for the whole background. The assertion covers the grace and the
    // App.pause call that follows it, and is released as soon as that returns. If
    // the system reclaims it first, the expiration handler pauses immediately
    // rather than losing the work. A quick return cancels the timer in
    // resumeFromBackground before it runs.
    private func schedulePause() {
        pauseWork?.cancel()
        beginPauseBgTask()
        let work = DispatchWorkItem { [weak self] in
            // asyncAfter runs this on the main queue, the MainActor's executor;
            // assert the isolation as scheduleEviction does.
            MainActor.assumeIsolated {
                self?.firePause()
            }
        }
        pauseWork = work
        DispatchQueue.main.asyncAfter(deadline: .now() + pauseMs / 1000.0, execute: work)
    }

    // firePause disconnects the wallets and holds the background-task assertion
    // until App.pause returns, so the process is not suspended partway through the
    // per-wallet shutdown. Reached from the grace timer or, if the system reclaims
    // the window first, from its expiration handler. Guarded so the two cannot
    // double-fire.
    private func firePause() {
        guard !paused else { return }
        pauseWork?.cancel(); pauseWork = nil
        paused = true
        // Release the assertion this pause runs under, not whichever is current
        // when App.pause returns: a background/foreground/background cycle can arm
        // a fresh one meanwhile, and ending that would cut the new grace short.
        let id = pauseBgTaskId
        pauseCore { [weak self] in self?.endPauseBgTask(id) }
    }

    private func beginPauseBgTask() {
        endPauseBgTask()
        pauseBgTaskId = UIApplication.shared.beginBackgroundTask(withName: "caribou-pause") { [weak self] in
            // Out of time. Pause now rather than lose it, then release the
            // assertion — the handler must return promptly or the app is killed.
            // If the process is suspended mid-pause the freeze quiets the radio
            // anyway and App.pause finishes on the next wake.
            self?.firePause()
            self?.endPauseBgTask()
        }
    }

    // endPauseBgTask releases the pause assertion, defaulting to the current one.
    // An explicit id that no longer matches belongs to a superseded grace that was
    // already ended, so it is dropped: ending one identifier twice is a UIKit
    // error, not a no-op.
    private func endPauseBgTask(_ id: UIBackgroundTaskIdentifier? = nil) {
        let target = id ?? pauseBgTaskId
        guard target != .invalid, target == pauseBgTaskId else { return }
        UIApplication.shared.endBackgroundTask(target)
        pauseBgTaskId = .invalid
    }

    // pauseCore drops wallet connections off the main actor (App.pause blocks on
    // per-wallet shutdown), then runs onPaused on the main actor once it returns.
    // Unlike lockCore it keeps the session resident: no lock, no re-auth, nothing
    // user-visible. resumeCore reconnects on foreground; the two share
    // walletLifecycleQueue so they can never land out of order, and App.resume
    // no-ops once the session was evicted, so a pause and resume racing across
    // tiers are safe.
    private func pauseCore(then onPaused: (() -> Void)?) {
        walletLifecycleQueue.async {
            do {
                try boundApp?.pause()
            } catch {
                NSLog("caribou: pause error: \(error.localizedDescription)")
            }
            guard let onPaused else { return }
            DispatchQueue.main.async { MainActor.assumeIsolated { onPaused() } }
        }
    }

    private func resumeCore() {
        walletLifecycleQueue.async {
            do {
                try boundApp?.resume()
            } catch {
                NSLog("caribou: resume skipped: \(error.localizedDescription)")
            }
        }
    }

    // scheduleEviction arms a best-effort key-eviction timer. Unlike Android's
    // AlarmManager, iOS gives no way to run code at an arbitrary future time while
    // the process is suspended, and it suspends us shortly after backgrounding. So
    // request a background execution window and fire a timer within it: for short
    // windows (or while the OS still lets us run) this evicts on schedule; for long
    // windows the process is suspended and the resume-time backstop in
    // resumeFromBackground() does the eviction instead — or the OS reclaims the
    // suspended app under memory pressure, evicting the keys for free. This is the
    // "best-effort, resume re-checks" policy from biometrics.md.
    private func scheduleEviction(afterMs ms: Double) {
        beginBgTask()
        let work = DispatchWorkItem { [weak self] in
            // asyncAfter runs this on the main queue, which is the MainActor's
            // executor, but DispatchWorkItem is not statically isolated. Assert the
            // isolation to call the MainActor-bound methods without an async hop —
            // the same pattern as the WebKit callbacks (WebView.didFinish,
            // BridgeProxy). Correct under strict (Swift 6) concurrency checking.
            MainActor.assumeIsolated {
                self?.lockCore(then: nil)
                self?.endBgTask()
            }
        }
        evictWork = work
        DispatchQueue.main.asyncAfter(deadline: .now() + ms / 1000.0, execute: work)
    }

    private func beginBgTask() {
        endBgTask()
        bgTaskId = UIApplication.shared.beginBackgroundTask(withName: "caribou-evict") { [weak self] in
            self?.endBgTask()
        }
    }

    private func endBgTask() {
        if bgTaskId != .invalid {
            UIApplication.shared.endBackgroundTask(bgTaskId)
            bgTaskId = .invalid
        }
    }

    // resumeFromBackground cancels the pending eviction, then picks a tier from how
    // long we were backgrounded:
    //   - under GATE: quick switch, resume untouched.
    //   - GATE..EVICTION with an auth mechanism: the cheap UI gate (overlay +
    //     biometric, core still logged in, keys resident, so success is instant).
    //   - past EVICTION, or in the gate band with no auth mechanism: full eviction
    //     (lock + re-auth).
    private func resumeFromBackground() {
        evictWork?.cancel(); evictWork = nil
        endBgTask()

        // Cancel a still-pending pause (quick return, no disconnect happened) or,
        // if the grace elapsed and we disconnected, reconnect. Orthogonal to the
        // gate/eviction tiers below: this restores connections, they restore the
        // session. If eviction already logged the core out, App.resume no-ops and
        // the eviction tier's Unlock reconnects.
        if let work = pauseWork {
            // The grace never elapsed, so nothing was disconnected and nothing
            // holds the assertion any more; release it here. The paused branch
            // below deliberately does not, because there App.pause is either in
            // flight or done and owns the release.
            work.cancel()
            pauseWork = nil
            endPauseBgTask()
        }
        if paused {
            paused = false
            resumeCore()
        }

        guard let bg = backgroundedAt else { return } // not a resume (first launch)
        backgroundedAt = nil
        let idleMs = Date().timeIntervalSince(bg) * 1000.0

        // The tier decision itself is the pure lockTier() (unit-tested in
        // LockTierTests); this only wires each tier to its side effects.
        switch lockTier(idleMs: idleMs, gateMs: gateMs(), evictMs: evictMs(),
                        canGate: biometric.canGate()) {
        case .evict:
            // evictAndReauth sets gated/unlocking itself, so no clearGate() first.
            evictAndReauth()
        case .gate:
            enterUiGate()
        case .none:
            if gated && !unlocking {
                // Under the gate window but the UI-gate overlay is already up (we
                // backgrounded mid-gate): re-present the prompt so a quick return
                // re-prompts instead of stranding the user on the manual retry
                // button. Skip this while unlocking (an eviction / cold-start
                // re-auth is in flight, driven by its own automatic prompt — a
                // presence check here would be the wrong tier).
                uiGateBiometric()
            }
        }
    }

    // MARK: - UI gate (cheap tier)

    // enterUiGate paints the overlay over the still-authed WebView and asks for a
    // biometric/passcode. On success the overlay clears and the resident session
    // shows through — no reload, no re-login.
    private func enterUiGate() {
        gated = true
        // Reset unlocking so the overlay shows the interactive gate (retry +
        // password escape hatch), not the passive "Unlocking…" spinner. It can be
        // stale-true if an eviction / cold-start re-auth was interrupted without
        // ever reaching clearGate (e.g. its reveal reload never finished before the
        // app was backgrounded); leaving it set would strand the user on a spinner
        // with no controls. Android does the same via setOverlayUnlocking(false).
        unlocking = false
        uiGateBiometric()
    }

    private func uiGateBiometric() {
        guard gated else { return }
        gateGeneration += 1
        let generation = gateGeneration
        biometric.authenticateSimple(reason: LockStrings.unlockPromptTitle) { [weak self] ok in
            guard let self else { return }
            // Ignore a stale success: a newer prompt or an escalation to the
            // eviction tier has since bumped gateGeneration, so this callback no
            // longer owns the overlay and must not clear it out from under the
            // tier now in charge.
            guard ok, generation == self.gateGeneration else { return }
            self.clearGate()
        }
    }

    private func clearGate() {
        gated = false
        unlocking = false
    }

    // retryGate re-presents the UI-gate biometric from the overlay's manual retry
    // button (for when the system prompt was dismissed).
    func retryGate() {
        uiGateBiometric()
    }

    // usePasswordInstead is the overlay escape hatch: drop to full eviction so the
    // user can re-auth with the app password if biometrics are unavailable.
    func usePasswordInstead() {
        clearGate()
        evictAndReauth()
    }

    // MARK: - Eviction (expensive tier)

    // evictAndReauth locks the core, then re-establishes a session. If biometric
    // unlock is enrolled (the expected opted-in path) it tries that first and loads
    // straight into the authed grant URL; otherwise it reloads the logged-out SPA
    // so the user can enter their password.
    private func evictAndReauth() {
        // Raise the overlay before the (blocking) lock so the authed WebView is not
        // left on screen while App.lock() runs. It stays up until the follow-up
        // page (grant URL or reloaded logged-out SPA) paints (onPageFinished).
        // unlocking = true so the overlay shows the passive "Unlocking…" spinner,
        // not the gate's tappable Unlock button (which would fire a second prompt).
        // Bump gateGeneration so any in-flight UI-gate biometric completion is
        // stale: this stronger tier now owns the overlay and a late gate success
        // must not clearGate() while this reauth is still in flight.
        gateGeneration += 1
        gated = true
        unlocking = true
        lockCore { [weak self] in
            guard let self else { return }
            if self.biometric.isEnrolled() {
                self.promptBiometricUnlock(onFallback: { [weak self] in self?.reloadToLoggedOutSpa() })
            } else {
                self.reloadToLoggedOutSpa()
            }
        }
    }

    // lockCore runs App.lock() off the main actor (it locks wallets and can block)
    // and, if given, invokes onLocked on the main actor once it returns. Concurrent
    // calls serialize on the Go-side App mutex and App.lock() is idempotent, so the
    // background timer and a resume backstop can both fire safely.
    private func lockCore(then onLocked: (() -> Void)?) {
        Task.detached(priority: .userInitiated) {
            do {
                try boundApp?.lock()
            } catch {
                NSLog("caribou: lock error: \(error.localizedDescription)")
            }
            if let onLocked {
                await MainActor.run { onLocked() }
            }
        }
    }

    // promptBiometricUnlock shows the biometric prompt and, on success, uses the
    // released password to re-unlock the core, then loads the returned grant URL so
    // the WebView regains an authed session. On cancel/failure it runs onFallback
    // (reload the logged-out SPA so the user can type the password).
    private func promptBiometricUnlock(onFallback: @escaping () -> Void) {
        biometric.unlock(reason: LockStrings.unlockPromptTitle) { [weak self] pwData in
            guard let self else { return }
            guard var pw = pwData else { onFallback(); return } // cancelled/failed
            // gomobile takes a Swift String we cannot wipe; the Go side (secretBytes)
            // authoritatively zeros the buffer it uses. Best-effort-wipe our copy
            // once the String is built.
            let pwString = String(decoding: pw, as: UTF8.self)
            pw.resetBytes(in: 0..<pw.count)

            Task.detached(priority: .userInitiated) {
                var unlockErr: NSError?
                let grantURL = boundApp?.unlock(pwString, error: &unlockErr)
                // Copy the out-param into a let before crossing into MainActor.run
                // so the closure captures an immutable value, not the var.
                let err = unlockErr
                await MainActor.run {
                    if let err {
                        self.handleUnlockFailure(pw: pwString, error: err, onFallback: onFallback)
                        return
                    }
                    guard let grantURL, let parsed = self.appURL(from: grantURL) else {
                        onFallback(); return
                    }
                    // The reveal is driven by the grant page's load finishing
                    // (onPageFinished clears revealOnLoad). With no WebView attached
                    // there is nothing to load it into and nothing to fire that
                    // callback, so setting revealOnLoad would strand the overlay on
                    // the spinner. Fall back instead (drops to the password screen /
                    // clears the gate). Not reachable in practice — the biometric
                    // prompt's latency guarantees attach() has run — but there is no
                    // other backstop for this path.
                    guard self.webView != nil else { onFallback(); return }
                    // Clear the overlay once the authed grant page paints.
                    self.revealOnLoad = true
                    self.load(parsed)
                }
            }
        }
    }

    // handleUnlockFailure decides whether an App.unlock error should cost the user
    // their enrollment. Only a genuinely stale password (e.g. changed after
    // enrolling) should — a transient failure (a wallet failing to open, core
    // mid-restart) must not. The biometric release already succeeded, so
    // BiometricAuth cannot tell the two apart; VerifyPassword re-checks just the
    // password, isolating it from wallet/session errors.
    private func handleUnlockFailure(pw: String, error: NSError, onFallback: @escaping () -> Void) {
        Task.detached(priority: .userInitiated) {
            let stale: Bool
            do { try boundApp?.verifyPassword(pw); stale = false }
            catch { stale = true }
            await MainActor.run {
                if stale {
                    self.biometric.clear()
                    self.pushLockState()
                }
                NSLog("caribou: unlock failed: \(error.localizedDescription)")
                onFallback()
            }
        }
    }

    // reloadToLoggedOutSpa reloads the WebView into the SPA unlock screen after an
    // eviction. App.lock() invalidated the session server-side, so the reloaded SPA
    // sees itself as logged out; the capability cookie is still valid, so the shell
    // still loads. onPageFinished clears the overlay once it paints (revealOnLoad).
    private func reloadToLoggedOutSpa() {
        // The overlay clears on the reload finishing (onPageFinished). With no
        // WebView to reload, that callback never fires, so don't raise revealOnLoad
        // — just drop the overlay so it can't stick on the spinner. (Defensive: the
        // WebView is attached throughout an eviction re-auth in practice.)
        guard let webView else { clearGate(); return }
        revealOnLoad = true
        webView.reload()
    }

    // MARK: - WebView wiring

    // onPageFinished clears the eviction overlay once the post-lock page has
    // painted, and re-pushes lock state so the SPA's synchronous bridge getters see
    // current values even though the reload wiped JS state.
    func onPageFinished() {
        if revealOnLoad {
            revealOnLoad = false
            clearGate()
        }
        pushLockState()
    }

    // onPageLoadFailed is the safety net for a navigation that errors while we were
    // waiting on it to reveal (e.g. a post-eviction grant load). Rather than strand
    // the user on the "Unlocking…" overlay, reload the base loopback URL so they
    // land on the logged-out SPA and can enter their password.
    func onPageLoadFailed() {
        guard revealOnLoad else { return }
        NSLog("caribou: page load failed while revealing; reloading base URL")
        revealOnLoad = false
        if let url = self.url {
            webView?.load(URLRequest(url: url))
        }
        clearGate()
    }

    // attach wires the hosted WKWebView into the lock flow. Called from WebView's
    // makeUIView once the view and its content controller exist.
    func attach(webView: WKWebView, userContentController: WKUserContentController) {
        self.webView = webView
        self.userContentController = userContentController
    }

    private func load(_ url: URL) {
        webView?.load(URLRequest(url: url))
    }

    // appURL appends the build-time network as a query param so the SPA can surface
    // a non-mainnet indicator; a wallet must never be mistaken for one on a
    // different network. Both the boot URL and the post-unlock grant URL go through
    // here so neither path can drop the indicator.
    private func appURL(from base: String) -> URL? {
        let sep = base.contains("?") ? "&" : "?"
        return URL(string: "\(base)\(sep)net=\(net)")
    }

    // MARK: - Native bridge (window.caribou)

    // makeUserContentController builds the content controller the WebView is
    // configured with: the message handler for the void bridge actions plus the
    // injected user scripts that define window.caribou. See BridgeProxy and
    // installUserScripts.
    func makeUserContentController() -> WKUserContentController {
        let ucc = WKUserContentController()
        ucc.add(BridgeProxy(model: self), name: "caribou")
        installUserScripts(into: ucc)
        return ucc
    }

    // installUserScripts injects, at document start, (1) the current lock state as
    // window.__caribou_lock and (2) the window.caribou shim that reads it. iOS message
    // handlers are async/void, but the SPA calls biometricAvailable()/getLockConfig()
    // synchronously, so the getters read this native-pushed state object while only
    // the void actions (enroll/disable/setLockConfig) post messages. The state
    // script is added first so the shim sees it already set.
    private func installUserScripts(into ucc: WKUserContentController) {
        let state = WKUserScript(source: "window.__caribou_lock=\(lockStateJSON());",
                                 injectionTime: .atDocumentStart, forMainFrameOnly: true)
        let shim = WKUserScript(source: AppModel.caribouShimJS,
                                injectionTime: .atDocumentStart, forMainFrameOnly: true)
        ucc.addUserScript(state)
        ucc.addUserScript(shim)
    }

    // pushLockState updates the live state object in the current page and fires the
    // change event the SPA listens for, then rebuilds the injected user scripts so a
    // future load also starts with current values (a reload re-runs user scripts,
    // not evaluateJavaScript).
    private func pushLockState() {
        let js = "window.__caribou_lock=\(lockStateJSON());" +
            "window.dispatchEvent(new Event('caribou-biometric-changed'));"
        webView?.evaluateJavaScript(js, completionHandler: nil)
        if let ucc = userContentController {
            ucc.removeAllUserScripts()
            installUserScripts(into: ucc)
        }
    }

    // lockStateJSON is the {available, canGate, enrolled, gateMin, evictMin} object
    // literal the bridge getters read. available gates the enroll toggle (biometrics
    // only, since enrollment needs .biometryCurrentSet); canGate gates the "Lock
    // after" row (biometrics OR the device passcode), so a passcode-only device can
    // still tune the gate that already runs for it. gateMin 0 = require unlock
    // immediately; evictMin 0 = never evict.
    private func lockStateJSON() -> String {
        let available = biometric.biometricsAvailable()
        let canGate = biometric.canGate()
        let enrolled = biometric.isEnrolled()
        let gateMin = Int(gateMs() / 60_000)
        let ev = evictMs()
        let evictMin = ev == evictionNever ? 0 : Int(ev / 60_000)
        return "{available:\(available),canGate:\(canGate),enrolled:\(enrolled),gateMin:\(gateMin),evictMin:\(evictMin)}"
    }

    // handleBridgeMessage dispatches a window.caribou void action posted from the SPA.
    // No message carries the app password: enrollBiometrics captures it in a native
    // sheet and unlock releases it natively, so the secret never crosses this
    // boundary.
    func handleBridgeMessage(_ body: Any) {
        guard let dict = body as? [String: Any], let m = dict["m"] as? String else { return }
        switch m {
        case "enroll":
            showEnrollDialog()
        case "disable":
            biometric.clear()
            pushLockState()
        case "setLockConfig":
            let g = (dict["gateMin"] as? NSNumber)?.intValue ?? 0
            let e = (dict["evictMin"] as? NSNumber)?.intValue ?? 0
            setLockConfig(gateMin: g, evictMin: e)
            pushLockState()
        default:
            break
        }
    }

    // setLockConfig persists the idle thresholds (minutes). gateMin 0 means gate on
    // every resume; evictMin 0 means never evict. The gate is the cheaper tier and
    // must sit strictly below eviction, or its band is empty and it never fires; the
    // SPA enforces this, but clamp defensively so a stale pair cannot disable the
    // gate.
    private func setLockConfig(gateMin: Int, evictMin: Int) {
        let gate0 = Double(max(0, gateMin)) * 60_000
        let evict = evictMin <= 0 ? evictionNever : Double(evictMin) * 60_000
        let gate = (evict != evictionNever && gate0 >= evict) ? evict - 60_000 : gate0
        defaults.set(gate, forKey: kGateMs)
        defaults.set(evict, forKey: kEvictMs)
    }

    // showEnrollDialog captures the app password in a native secure field (never
    // through JS), verifies it against the core, then stores it behind a biometric
    // prompt.
    private func showEnrollDialog() {
        guard let vc = AppModel.topViewController() else { return }
        let alert = UIAlertController(title: LockStrings.enrollTitle,
                                      message: LockStrings.enrollMessage,
                                      preferredStyle: .alert)
        alert.addTextField { tf in
            tf.isSecureTextEntry = true
            tf.placeholder = LockStrings.passwordPlaceholder
            tf.textContentType = .password
        }
        alert.addAction(UIAlertAction(title: LockStrings.cancel, style: .cancel))
        alert.addAction(UIAlertAction(title: LockStrings.enable, style: .default) { [weak self, weak alert] _ in
            let pw = alert?.textFields?.first?.text ?? ""
            self?.verifyThenEnroll(pw)
        })
        vc.present(alert, animated: true)
    }

    // verifyThenEnroll confirms pw against the core off the main actor, then (on
    // success) stores it behind a biometric prompt.
    private func verifyThenEnroll(_ pw: String) {
        Task.detached(priority: .userInitiated) {
            let ok: Bool
            do { try boundApp?.verifyPassword(pw); ok = true }
            catch { ok = false }
            await MainActor.run {
                guard ok else { self.toast(LockStrings.incorrectPassword); return }
                let pwData = Data(pw.utf8)
                self.biometric.enroll(pw: pwData, reason: LockStrings.enrollTitle) { [weak self] enrolled in
                    self?.pushLockState()
                    self?.toast(enrolled ? LockStrings.biometricEnabled : LockStrings.enrollmentCancelled)
                }
            }
        }
    }

    // toast shows a brief, auto-dismissing message — the iOS stand-in for Android's
    // Toast for the enroll/unlock status feedback.
    private func toast(_ msg: String) {
        guard let vc = AppModel.topViewController() else { NSLog("caribou: \(msg)"); return }
        let a = UIAlertController(title: nil, message: msg, preferredStyle: .alert)
        vc.present(a, animated: true)
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.6) { a.dismiss(animated: true) }
    }

    // MARK: - Teardown / storage

    // stopForTermination tears the core down (locks wallets, then closes the
    // txDB/storage engine in order). Wired to willTerminate as best-effort cleanup;
    // the call is idempotent Go-side. nonisolated: it only touches the bound App
    // global (itself concurrency-safe), so the willTerminate observer can call it
    // directly without a MainActor hop.
    //
    // The observer is registered on the main queue and App.stop blocks - on the
    // wallet logout, then on an ordered storage close - so run it off the main
    // thread and wait, rather than parking the UI thread for the whole shutdown.
    // Android runs the same call on its own thread for the same reason. This is
    // the only bound-App call the shell makes from the main thread; every other
    // one already goes through Task.detached or walletLifecycleQueue.
    //
    // The wait is bounded because willTerminate is: the OS kills the process a
    // few seconds in, so a shutdown still running at the deadline is not going to
    // finish, and holding main until then buys nothing. 3s leaves headroom under
    // the termination deadline. Overrunning it means the storage close did not
    // complete, which is what the idempotent Go-side stop and the DB's own
    // recovery are there for - the same outcome as the OS killing a suspended app
    // outright, which it may do with no callback at all.
    nonisolated static func stopForTermination() {
        let done = DispatchSemaphore(value: 0)
        DispatchQueue.global(qos: .userInitiated).async {
            try? boundApp?.stop()
            done.signal()
        }
        if done.wait(timeout: .now() + 3.0) == .timedOut {
            NSLog("caribou: core shutdown did not finish before the termination deadline")
        }
    }

    // dataDir returns the app-private storage path passed to App.start. It uses
    // Application Support (app-private, not user-visible) and excludes it from
    // iCloud/iTunes backups so wallet secrets at rest never leave the device — the
    // iOS analog of Android's allowBackup=false.
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
            NSLog("caribou: failed to exclude data dir from backup: \(error)")
        }
        return dir.path
    }

    // topViewController finds the frontmost presented view controller so the native
    // enroll sheet / toast can be presented over the WebView.
    static func topViewController() -> UIViewController? {
        let scenes = UIApplication.shared.connectedScenes.compactMap { $0 as? UIWindowScene }
        let window = scenes.flatMap { $0.windows }.first { $0.isKeyWindow }
        var top = window?.rootViewController
        while let presented = top?.presentedViewController { top = presented }
        return top
    }

    enum ShellError: LocalizedError {
        case message(String)
        var errorDescription: String? {
            switch self {
            case .message(let m): return m
            }
        }
    }

    // caribouShimJS defines window.caribou at document start. The synchronous getters
    // read the native-pushed window.__caribou_lock; the void actions post to the
    // native message handler. Kept in sync with the CaribouBridge interface the SPA
    // declares (wallet/appserver/mobile/src/components/Settings.tsx).
    private static let caribouShimJS = """
    (function () {
      if (!window.__caribou_lock) {
        window.__caribou_lock = { available: false, canGate: false, enrolled: false, gateMin: 10, evictMin: 60 };
      }
      var post = function (msg) { window.webkit.messageHandlers.caribou.postMessage(msg); };
      window.caribou = {
        biometricAvailable: function () { return !!window.__caribou_lock.available; },
        canGate: function () { return !!window.__caribou_lock.canGate; },
        biometricEnrolled: function () { return !!window.__caribou_lock.enrolled; },
        getLockConfig: function () {
          return JSON.stringify({
            gateMin: window.__caribou_lock.gateMin,
            evictMin: window.__caribou_lock.evictMin
          });
        },
        enrollBiometrics: function () { post({ m: 'enroll' }); },
        disableBiometrics: function () { post({ m: 'disable' }); },
        setLockConfig: function (g, e) {
          // The SPA reads getLockConfig() back synchronously right after this call
          // (it relies on Android's bridge being synchronous). Our native handler
          // is async, so update the cached state here first — applying the same
          // clamp native uses (the gate must sit strictly below eviction) — so the
          // immediate readback is correct. The posted message persists the same
          // values natively, where the identical clamp runs again.
          var gate = Math.max(0, g | 0);
          var evict = (e | 0) <= 0 ? 0 : (e | 0);
          if (evict !== 0 && gate >= evict) { gate = evict - 1; }
          window.__caribou_lock.gateMin = gate;
          window.__caribou_lock.evictMin = evict;
          post({ m: 'setLockConfig', gateMin: g, evictMin: e });
        }
      };
    })();
    """
}

// BridgeProxy relays window.caribou void-action messages to AppModel. It holds the
// model weakly so the WKUserContentController -> handler retain does not pin the
// model (or, through it, the WebView) alive.
final class BridgeProxy: NSObject, WKScriptMessageHandler {
    weak var model: AppModel?
    init(model: AppModel) { self.model = model }

    func userContentController(_ controller: WKUserContentController,
                               didReceive message: WKScriptMessage) {
        // WebKit delivers message-handler callbacks on the main thread; hop into
        // the MainActor-isolated model without an async round-trip.
        MainActor.assumeIsolated {
            model?.handleBridgeMessage(message.body)
        }
    }
}
