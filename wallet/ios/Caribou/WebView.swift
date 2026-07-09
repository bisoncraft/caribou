import SwiftUI
import WebKit

// WebView wraps a WKWebView and confines navigation to the in-process loopback
// appserver. The SPA is served entirely from loopback, so any attempt to
// navigate elsewhere (an external link, a redirect, a file:// URL) is refused
// rather than loaded — the iOS analog of Android's LoopbackWebViewClient.
//
// It is also where the native session-lock bridge attaches: the configuration
// carries the window.caribou message handler + injected shim (AppModel.make-
// UserContentController), the model gets a handle to the live WKWebView for the
// eviction reload / grant-URL load / bridge state push (AppModel.attach), and
// page-load completion is reported back so the lock overlay can clear at the
// right moment (AppModel.onPageFinished).
struct WebView: UIViewRepresentable {
    let url: URL
    @ObservedObject var model: AppModel

    func makeCoordinator() -> Coordinator {
        Coordinator(model: model)
    }

    func makeUIView(context: Context) -> WKWebView {
        let configuration = WKWebViewConfiguration()
        // The window.caribou bridge (message handler + injected getter shim) lives on
        // the content controller.
        let ucc = model.makeUserContentController()
        configuration.userContentController = ucc

        let webView = WKWebView(frame: .zero, configuration: configuration)
        webView.navigationDelegate = context.coordinator
        // The shell only ever talks to the loopback server; there is nothing to
        // go back to, so disable the swipe-back gesture.
        webView.allowsBackForwardNavigationGestures = false
        webView.scrollView.bounces = false
        webView.isOpaque = false
        webView.backgroundColor = .black

        // Debug builds opt the WebView into Safari Web Inspector (Develop menu)
        // so the SPA can be inspected on-device; left off in release. iOS 16.4+
        // gates WKWebView inspection behind this flag.
        #if DEBUG
        if #available(iOS 16.4, *) {
            webView.isInspectable = true
        }
        #endif

        // Hand the model a handle for reload / grant-URL load / bridge state push.
        model.attach(webView: webView, userContentController: ucc)

        context.coordinator.loadedURL = url
        webView.load(URLRequest(url: url))
        return webView
    }

    func updateUIView(_ webView: WKWebView, context: Context) {
        // The shell loads exactly one URL through SwiftUI (the loopback bootstrap).
        // Only reload if it actually changed (a fresh Start after teardown). The
        // lock flow's grant-URL loads and reloads go through the model's webView
        // handle directly, not this path, so they never fight the diffing here.
        if context.coordinator.loadedURL != url {
            context.coordinator.loadedURL = url
            webView.load(URLRequest(url: url))
        }
    }

    final class Coordinator: NSObject, WKNavigationDelegate {
        var loadedURL: URL?
        let model: AppModel

        init(model: AppModel) {
            self.model = model
        }

        func webView(_ webView: WKWebView,
                     decidePolicyFor navigationAction: WKNavigationAction,
                     decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
            let target = navigationAction.request.url
            let ok = target.map(Coordinator.isLoopback) ?? false
            decisionHandler(ok ? .allow : .cancel)
        }

        func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
            // WebKit delivers navigation callbacks on the main thread; hop into the
            // MainActor-isolated model without an async round-trip.
            MainActor.assumeIsolated {
                model.onPageFinished()
            }
        }

        func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) {
            guard !Coordinator.isCancelled(error) else { return }
            MainActor.assumeIsolated {
                NSLog("caribou: nav didFailProvisional: \(error.localizedDescription)")
                model.onPageLoadFailed()
            }
        }

        func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) {
            guard !Coordinator.isCancelled(error) else { return }
            MainActor.assumeIsolated {
                NSLog("caribou: nav didFail: \(error.localizedDescription)")
                model.onPageLoadFailed()
            }
        }

        // isCancelled reports whether an error is just WebKit superseding one load
        // with another (NSURLErrorCancelled), not a real navigation failure. This
        // fires on the cold-start reveal: setting url starts the boot-URL load, and
        // a biometric unlock that completes before it finishes issues the grant-URL
        // load, cancelling the boot navigation. Treating that as a failure would run
        // onPageLoadFailed and drop the just-unlocked session back to the login
        // screen, so ignore it — the superseding load will report its own outcome.
        static func isCancelled(_ error: Error) -> Bool {
            (error as NSError).code == NSURLErrorCancelled
        }

        // isLoopback reports whether url targets the in-process appserver:
        // plaintext http to 127.0.0.1 or localhost.
        static func isLoopback(_ url: URL) -> Bool {
            guard url.scheme == "http" else { return false }
            let host = url.host
            return host == "127.0.0.1" || host == "localhost"
        }
    }
}
