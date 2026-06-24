import SwiftUI

// ContentView shows the boot spinner, a start-failure message, or the WebView
// once the loopback URL is ready.
struct ContentView: View {
    @ObservedObject var model: AppModel

    var body: some View {
        ZStack {
            Color.black.ignoresSafeArea()

            if let url = model.url {
                WebView(url: url)
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
        }
    }
}
