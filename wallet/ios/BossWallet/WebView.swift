import SwiftUI
import WebKit

// WebView wraps a WKWebView and confines navigation to the in-process loopback
// appserver. The SPA is served entirely from loopback, so any attempt to
// navigate elsewhere (an external link, a redirect, a file:// URL) is refused
// rather than loaded — the iOS analog of Android's LoopbackWebViewClient.
struct WebView: UIViewRepresentable {
    let url: URL

    func makeCoordinator() -> Coordinator {
        Coordinator()
    }

    func makeUIView(context: Context) -> WKWebView {
        let webView = WKWebView(frame: .zero, configuration: WKWebViewConfiguration())
        webView.navigationDelegate = context.coordinator
        // The shell only ever talks to the loopback server; there is nothing to
        // go back to, so disable the swipe-back gesture.
        webView.allowsBackForwardNavigationGestures = false
        webView.scrollView.bounces = false
        webView.isOpaque = false
        webView.backgroundColor = .black

        context.coordinator.loadedURL = url
        webView.load(URLRequest(url: url))
        return webView
    }

    func updateUIView(_ webView: WKWebView, context: Context) {
        // The shell loads exactly one URL (the loopback bootstrap). Only reload
        // if it actually changed (a fresh Start after teardown).
        if context.coordinator.loadedURL != url {
            context.coordinator.loadedURL = url
            webView.load(URLRequest(url: url))
        }
    }

    final class Coordinator: NSObject, WKNavigationDelegate {
        var loadedURL: URL?

        func webView(_ webView: WKWebView,
                     decidePolicyFor navigationAction: WKNavigationAction,
                     decisionHandler: @escaping (WKNavigationActionPolicy) -> Void) {
            if let target = navigationAction.request.url, Coordinator.isLoopback(target) {
                decisionHandler(.allow)
            } else {
                decisionHandler(.cancel)
            }
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
