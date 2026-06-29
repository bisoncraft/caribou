package com.bisoncraft.bosswallet

import android.annotation.SuppressLint
import android.net.Uri
import android.os.Bundle
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import kotlin.concurrent.thread
import mobile.App
import mobile.Mobile

// MainActivity is the thin native shell: it boots the Go core + appserver on
// loopback (via the gomobile-bound App) and hosts the mobile SPA in a
// full-screen WebView. All app logic lives in the Go core and the React bundle;
// this class only wires the WebView to the loopback URL and manages lifecycle.
class MainActivity : AppCompatActivity() {

    private lateinit var webView: WebView

    // The Go core owns on-disk state (DB, storage engine) keyed to filesDir, so
    // only one may run per process. onDestroy tears it down on a detached thread
    // that outlives the Activity; a later recreate builds a fresh Activity whose
    // onCreate starts the core again. A per-Activity App would let the new
    // instance's start() reopen the DB while the old instance's stop() is still
    // closing it. A single process-wide App makes every start()/stop() share one
    // Go-side mutex, so a start blocks until the prior stop releases the DB (and
    // Start returns the live URL if the core is still up).
    companion object {
        private val app: App = Mobile.newApp()
    }

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        webView = WebView(this).apply {
            settings.javaScriptEnabled = true
            settings.domStorageEnabled = true
            // The WebView only ever talks to the in-process loopback server, so
            // lock it down. The wallet DB lives under filesDir; deny file:// and
            // content:// access (these default to enabled below API 30) so a
            // stray navigation or an SPA XSS cannot read app-private files, and
            // confine navigation to the loopback origin.
            settings.allowFileAccess = false
            settings.allowContentAccess = false
            settings.allowFileAccessFromFileURLs = false
            settings.allowUniversalAccessFromFileURLs = false
            webViewClient = LoopbackWebViewClient()
        }
        setContentView(webView)

        // Start the core off the main thread; wallet boot can be slow. The
        // start thread can outlive this Activity (e.g. a destroy during boot),
        // so use the application context for the Toast rather than the Activity,
        // and skip loadUrl if the Activity is already gone.
        val appCtx = applicationContext
        thread(name = "bossw-start") {
            try {
                // Network is set at build time via BuildConfig.WALLET_NET
                // (default mainnet; override with -PwalletNet=simnet). filesDir
                // is app-private storage.
                val url = app.start(filesDir.absolutePath, BuildConfig.WALLET_NET)
                // Hand the build-time network to the SPA (query param) so it can
                // surface a non-mainnet indicator; a wallet must never be
                // mistaken for one running on a different network.
                val sep = if (url.contains('?')) '&' else '?'
                val webUrl = "$url${sep}net=${BuildConfig.WALLET_NET}"
                runOnUiThread { if (!isDestroyed) webView.loadUrl(webUrl) }
            } catch (e: Exception) {
                runOnUiThread {
                    Toast.makeText(appCtx, "Failed to start: ${e.message}", Toast.LENGTH_LONG).show()
                }
            }
        }
    }

    override fun onDestroy() {
        // Only tear the core down when the Activity is actually finishing. On a
        // configuration-change recreation isFinishing is false, so the
        // process-wide core stays up and the recreated Activity's start()
        // reattaches to the live URL. This closes a stop()/start() race where
        // the outgoing Activity's stop() could tear down a core the incoming
        // Activity just (re)started.
        if (isFinishing) {
            // Tear down off the main thread (locks wallets, closes the DB in the
            // right order): stop() blocks on wallet logout and ordered core
            // shutdown, which would ANR on the UI thread. The thread is
            // non-daemon so the JVM keeps it alive to finish a clean shutdown.
            thread(name = "bossw-stop") {
                try {
                    app.stop()
                } catch (_: Exception) {
                }
            }
        }
        super.onDestroy()
    }
}

// LoopbackWebViewClient confines navigation to the loopback origin. The SPA is
// served entirely by the in-process appserver, so any attempt to navigate
// elsewhere (an external link, a redirect, a file:// URL) is refused rather
// than loaded.
private class LoopbackWebViewClient : WebViewClient() {
    override fun shouldOverrideUrlLoading(view: WebView, request: WebResourceRequest): Boolean =
        !isLoopback(request.url)

    @Deprecated("Used on API < 24, where the WebResourceRequest overload is unavailable")
    override fun shouldOverrideUrlLoading(view: WebView, url: String): Boolean =
        !isLoopback(Uri.parse(url))

    // isLoopback reports whether uri targets the in-process appserver: plaintext
    // http to 127.0.0.1 or localhost. Returning false from shouldOverrideUrlLoading
    // lets the WebView load it; true blocks the navigation.
    private fun isLoopback(uri: Uri): Boolean {
        val host = uri.host ?: return false
        return uri.scheme == "http" && (host == "127.0.0.1" || host == "localhost")
    }
}
