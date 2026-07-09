import SwiftUI

// ContentView shows the boot spinner, a start-failure message, or the WebView
// once the loopback URL is ready, with two native overlays on top: the lock
// overlay during the UI gate / eviction re-auth, and the privacy cover whenever
// the app is not active (so wallet content stays out of the app-switcher
// snapshot).
struct ContentView: View {
    @ObservedObject var model: AppModel

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()

            if let url = model.url {
                WebView(url: url, model: model)
                    .ignoresSafeArea()
            } else if let message = model.errorMessage {
                // Boot failed (e.g. the core could not start). Surface it rather
                // than leaving a blank screen; the analog of Android's
                // start-failure Toast.
                VStack(spacing: 12) {
                    Text("Failed to start")
                        .font(.headline)
                        .foregroundStyle(.white)
                    Text(message)
                        .font(.callout)
                        .multilineTextAlignment(.center)
                        .foregroundStyle(.secondary)
                }
                .padding()
            } else {
                // Core booting; wallet boot can be slow.
                ProgressView()
                    .tint(.white)
            }

            // UI gate: opaque cover over the WebView while the biometric gate or an
            // eviction re-auth is in flight. Retry re-presents the biometric;
            // "use password" drops to full eviction.
            if model.gated {
                LockOverlay(
                    retry: { model.retryGate() },
                    usePassword: { model.usePasswordInstead() }
                )
                .transition(.opacity)
            }

            // Privacy cover: topmost so the OS app-switcher snapshot (taken as the
            // scene resigns active) never shows wallet content, even over the gate.
            if model.obscured {
                PrivacyCover()
            }
        }
    }
}
