import SwiftUI
import UIKit

// CaribouApp is the thin native shell entry point. Like Android's
// MainActivity it boots the Go core + appserver on loopback (via the
// gomobile-bound App) and hosts the mobile SPA in a full-screen WebView. All app
// logic lives in the Go core (wallet/mobile) and the React bundle
// (wallet/appserver/mobile); this shell only wires the WebView to the loopback
// URL and manages lifecycle.
@main
struct CaribouApp: App {
    @StateObject private var model = AppModel()
    @Environment(\.scenePhase) private var scenePhase

    init() {
        // iOS has no reliable "app is closing" callback (the OS may kill a
        // suspended process without warning), so tear the core down on
        // willTerminate as a best-effort clean shutdown. Stop is idempotent
        // Go-side, and the OS reclaims memory if it kills us first.
        NotificationCenter.default.addObserver(
            forName: UIApplication.willTerminateNotification,
            object: nil,
            queue: .main
        ) { _ in
            AppModel.stopForTermination()
        }
    }

    var body: some Scene {
        WindowGroup {
            ContentView(model: model)
                // The SPA paints edge to edge and handles notch / home-indicator
                // insets itself via CSS env(safe-area-inset-*).
                .ignoresSafeArea()
                .onAppear { model.start() }
        }
        .onChange(of: scenePhase) { newPhase in
            model.scenePhaseChanged(newPhase)
        }
    }
}
