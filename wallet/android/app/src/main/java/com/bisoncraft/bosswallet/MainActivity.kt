package com.bisoncraft.bosswallet

import android.annotation.SuppressLint
import android.app.AlarmManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.SystemClock
import android.view.WindowManager
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Toast
import androidx.appcompat.app.AppCompatActivity
import androidx.core.content.ContextCompat
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

        // EVICTION_IDLE_MS is how long the app may sit backgrounded before the
        // Go core is locked (app.lock(): wallets locked, keys zeroed, sessions
        // torn down). This is the expensive key-eviction tier from
        // ~/ocean/bossmobile/biometrics.md, deliberately rare: normal app
        // switching stays well under it and never re-auths. Short here for
        // testing; production is ~1 hour (and should be user-configurable).
        // TODO(biometrics): raise to ~1h and make configurable; add the cheaper
        // ~10 min UI-gate tier once BiometricPrompt lands (increment B).
        private const val EVICTION_IDLE_MS = 30_000L

        // Private broadcast action for the key-eviction alarm (see evictReceiver).
        private const val ACTION_EVICT = "com.bisoncraft.bosswallet.ACTION_EVICT"
    }

    private lateinit var alarmManager: AlarmManager

    // elapsedRealtime when the app was last backgrounded (0 = never / foreground).
    // elapsedRealtime keeps counting across device sleep, so onStart can measure
    // true wall-clock idle as a backstop even if the eviction alarm is deferred.
    private var backgroundedAt = 0L

    // evictReceiver fires the eviction while the app is still backgrounded, so
    // keys do not sit resident for the whole idle window. It is driven by an
    // AlarmManager alarm rather than a main-looper Handler so it still fires while
    // the device is dozing (a Handler's uptime clock is frozen in deep sleep). A
    // runtime-registered receiver is enough: the alarm only needs to reach a
    // still-alive backgrounded process - if the process was killed the keys went
    // with it. app.lock() is idempotent, so an onStart backstop re-lock is
    // harmless. No reload here: nothing is visible while backgrounded.
    private val evictReceiver = object : BroadcastReceiver() {
        override fun onReceive(context: Context, intent: Intent) = lockCoreAsync(null)
    }

    // evictPendingIntent is the alarm target. FLAG_IMMUTABLE is required on API
    // 31+; the explicit package keeps delivery to our own receiver.
    private val evictPendingIntent: PendingIntent by lazy {
        PendingIntent.getBroadcast(
            this,
            0,
            Intent(ACTION_EVICT).setPackage(packageName),
            PendingIntent.FLAG_UPDATE_CURRENT or PendingIntent.FLAG_IMMUTABLE,
        )
    }

    @SuppressLint("SetJavaScriptEnabled")
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)

        // Mark the window secure so backgrounded wallet content is excluded from
        // the OS recents/app-switcher thumbnail (the snapshot is taken at onStop)
        // and blocked from screenshots and screen recording.
        window.setFlags(
            WindowManager.LayoutParams.FLAG_SECURE,
            WindowManager.LayoutParams.FLAG_SECURE,
        )

        // Key-eviction alarm plumbing: register the private, non-exported receiver
        // the backgrounded eviction alarm targets (see onStop/scheduleEviction).
        // RECEIVER_NOT_EXPORTED keeps other apps on the device from reaching it.
        alarmManager = getSystemService(Context.ALARM_SERVICE) as AlarmManager
        ContextCompat.registerReceiver(
            this,
            evictReceiver,
            IntentFilter(ACTION_EVICT),
            ContextCompat.RECEIVER_NOT_EXPORTED,
        )

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

    // onStop fires when the app is no longer visible (backgrounded). Stamp the
    // time and arm the eviction timer. onStart cancels it if the user comes back
    // in time. isFinishing means a real teardown, handled by onDestroy.
    override fun onStop() {
        super.onStop()
        if (isFinishing) return
        backgroundedAt = SystemClock.elapsedRealtime()
        scheduleEviction(backgroundedAt + EVICTION_IDLE_MS)
    }

    // scheduleEviction arms the key-eviction alarm for triggerAtElapsed (an
    // elapsedRealtime deadline, matching backgroundedAt). setAndAllowWhileIdle lets
    // it fire even while the device is dozing, unlike a Handler; below API 23
    // (pre-Doze) a plain set suffices. The onStart backstop still re-checks on
    // resume in case the OS coalesced or deferred the alarm.
    private fun scheduleEviction(triggerAtElapsed: Long) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            alarmManager.setAndAllowWhileIdle(
                AlarmManager.ELAPSED_REALTIME_WAKEUP, triggerAtElapsed, evictPendingIntent)
        } else {
            alarmManager.set(
                AlarmManager.ELAPSED_REALTIME_WAKEUP, triggerAtElapsed, evictPendingIntent)
        }
    }

    // onStart fires when the app becomes visible again. Cancel any pending
    // eviction, then decide from the idle time whether this stint crossed the
    // eviction threshold. If so, ensure the core is locked (the background timer
    // usually already did it; this also covers the timer being frozen by Doze)
    // and reload the WebView into the SPA's unlock screen once the lock lands. A
    // quick switch falls under the threshold and resumes untouched.
    override fun onStart() {
        super.onStart()
        alarmManager.cancel(evictPendingIntent)

        val idleMs = if (backgroundedAt == 0L) 0L else SystemClock.elapsedRealtime() - backgroundedAt
        backgroundedAt = 0L
        if (idleMs < EVICTION_IDLE_MS) return

        // Lock (idempotent), then reload only after it completes so the SPA does
        // not briefly re-render the still-authed view before the session is torn
        // down. app.lock() invalidated the session server-side, so the reloaded
        // SPA sees itself as logged out and prompts for the password (its normal
        // /api/login flow); the capability cookie is still valid, so the shell
        // still loads. Once biometric unlock lands (increment B), a BiometricPrompt
        // gates here instead of the SPA showing its password field.
        lockCoreAsync {
            if (::webView.isInitialized) webView.reload()
        }
    }

    // lockCoreAsync runs app.lock() off the main thread (it locks wallets and can
    // block) and, if given, invokes onLocked on the main thread once it returns.
    // Concurrent calls serialize on the Go-side App mutex, and app.lock() is
    // idempotent, so the background timer and an onStart backstop can both fire
    // safely.
    private fun lockCoreAsync(onLocked: (() -> Unit)?) {
        thread(name = "bossw-lock") {
            try {
                app.lock()
            } catch (e: Exception) {
                runOnUiThread {
                    Toast.makeText(applicationContext, "Lock error: ${e.message}", Toast.LENGTH_SHORT).show()
                }
            }
            if (onLocked != null) runOnUiThread { if (!isDestroyed) onLocked() }
        }
    }

    override fun onDestroy() {
        if (::alarmManager.isInitialized) alarmManager.cancel(evictPendingIntent)
        runCatching { unregisterReceiver(evictReceiver) }
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
