package com.bisoncraft.bosswallet

import android.annotation.SuppressLint
import android.app.AlarmManager
import android.app.PendingIntent
import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.content.IntentFilter
import android.content.res.ColorStateList
import android.graphics.Color
import android.net.Uri
import android.os.Build
import android.os.Bundle
import android.os.SystemClock
import android.text.InputType
import android.util.TypedValue
import android.view.Gravity
import android.view.View
import android.view.WindowManager
import android.webkit.JavascriptInterface
import android.webkit.WebResourceRequest
import android.webkit.WebView
import android.webkit.WebViewClient
import android.widget.Button
import android.widget.EditText
import android.widget.FrameLayout
import android.widget.LinearLayout
import android.widget.ProgressBar
import android.widget.TextView
import android.widget.Toast
import androidx.appcompat.app.AlertDialog
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

        // Two idle tiers from ~/ocean/bossmobile/biometrics.md, measured as
        // continuous time backgrounded. Normal app switching stays well under the
        // gate and never re-auths. Both thresholds are user-configurable (see
        // gateIdleMs/evictionIdleMs, set from the SPA via the bridge); these are
        // the defaults.
        //
        // Gate: the cheap UI gate. Past it, a resume paints a native biometric
        // overlay but the core stays logged in and keys stay resident, so clearing
        // it is instant (no re-login). Only applies when the device has
        // biometrics; without them the gate would need the password, which is
        // eviction, so a non-biometric device escalates straight to eviction. A
        // gate of 0 means "require unlock immediately" (gate on every resume).
        //
        // Eviction: the expensive key-eviction tier. Past it the core is locked
        // (app.lock(): wallets locked, keys zeroed, sessions torn down), fired on
        // a background timer so keys do not sit resident for the whole window,
        // with a resume-time backstop. EVICTION_NEVER disables it (gate only).
        private const val DEFAULT_GATE_MS = 10 * 60 * 1000L // 10 min
        private const val DEFAULT_EVICTION_MS = 60 * 60 * 1000L // 1 hour
        private const val EVICTION_NEVER = Long.MAX_VALUE
        private const val LOCK_PREFS = "bossw_lock"
        private const val PREF_GATE_MS = "gate_ms"
        private const val PREF_EVICT_MS = "evict_ms"

        // Private broadcast action for the key-eviction alarm (see evictReceiver).
        private const val ACTION_EVICT = "com.bisoncraft.bosswallet.ACTION_EVICT"
    }

    private lateinit var alarmManager: AlarmManager
    private lateinit var biometricAuth: BiometricAuth

    // lockOverlay is the opaque native view painted over the WebView during the
    // UI gate, so the still-resident session's content is hidden behind a
    // biometric check. gated tracks whether it is up.
    private lateinit var lockOverlay: View
    private var gated = false

    // Overlay pieces toggled by setOverlayUnlocking: the passive "Unlocking…"
    // progress shown while a cold-start / eviction re-auth is in flight vs. the
    // interactive UI-gate controls (retry button + password escape hatch).
    private lateinit var lockStatusText: TextView
    private lateinit var lockSpinner: ProgressBar
    private lateinit var lockUnlockButton: Button
    private lateinit var lockUsePassword: TextView

    // revealOnLoad keeps the lock overlay up across the eviction reload: eviction
    // raises the overlay before the (blocking) lock, then reloads into the
    // logged-out SPA; the overlay only clears once that page has finished loading
    // (onWebViewPageFinished), so no authed frame flashes through in between. It
    // does not clear the UI gate, which does no page load.
    private var revealOnLoad = false

    // Idle thresholds, persisted so the SPA Settings pane can tune them via the
    // bridge and they survive restarts. Read fresh each lifecycle callback so a
    // change takes effect on the next background/resume.
    private val lockPrefs by lazy { getSharedPreferences(LOCK_PREFS, MODE_PRIVATE) }
    private fun gateIdleMs(): Long = lockPrefs.getLong(PREF_GATE_MS, DEFAULT_GATE_MS)
    private fun evictionIdleMs(): Long = lockPrefs.getLong(PREF_EVICT_MS, DEFAULT_EVICTION_MS)

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

        biometricAuth = BiometricAuth(this)

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
            webViewClient = LoopbackWebViewClient { onWebViewPageFinished() }
            // Expose the biometric opt-in bridge to the SPA as window.bossw. The
            // WebView only ever loads our own SPA over locked-down loopback, and
            // no bridge method returns a secret or accepts a password (the
            // password is captured in a native field), so this surface is safe.
            addJavascriptInterface(BiometricBridge(), "bossw")
        }
        // Host the WebView in a FrameLayout so the lock overlay can sit on top of
        // it during the UI gate.
        val root = FrameLayout(this)
        root.addView(webView, FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT)
        lockOverlay = buildLockOverlay().apply { visibility = View.GONE }
        root.addView(lockOverlay, FrameLayout.LayoutParams.MATCH_PARENT, FrameLayout.LayoutParams.MATCH_PARENT)
        setContentView(root)

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
                runOnUiThread { coldStart(url) }
            } catch (e: Exception) {
                runOnUiThread {
                    Toast.makeText(appCtx, getString(R.string.start_failed, e.message), Toast.LENGTH_LONG).show()
                }
            }
        }
    }

    // loadAppUrl loads a loopback app URL in the WebView, always appending the
    // build-time network as a query param so the SPA can surface a non-mainnet
    // indicator; a wallet must never be mistaken for one on a different network.
    // Both the boot URL and the post-unlock grant URL go through here so neither
    // path can drop the indicator. Safe to call from any thread.
    private fun loadAppUrl(url: String) {
        val sep = if (url.contains('?')) '&' else '?'
        val webUrl = "$url${sep}net=${BuildConfig.WALLET_NET}"
        runOnUiThread { if (!isDestroyed && ::webView.isInitialized) webView.loadUrl(webUrl) }
    }

    // coldStart shows the freshly-booted SPA. On a cold launch the core is logged
    // out, so if biometric unlock is enrolled, offer it (the "unlock once at
    // launch" from biometrics.md) instead of leaving the user to retype their
    // password: the logged-out SPA loads behind the overlay, a biometric unlock
    // swaps it for an authed grant page, and a cancel drops the overlay to reveal
    // the password screen. Unlike the eviction tier there is nothing to lock first,
    // so it goes straight to the biometric prompt. Must run on the UI thread
    // (BiometricPrompt requirement).
    private fun coldStart(url: String) {
        loadAppUrl(url)
        if (!biometricAuth.isEnrolled()) return
        // Passive overlay: the biometric prompt drives the unlock, so show the
        // "Unlocking…" spinner rather than the gate's tappable controls (which
        // would fire a second prompt / drop to the wrong tier).
        setOverlayUnlocking(true)
        lockOverlay.visibility = View.VISIBLE
        // On cancel/failure the logged-out SPA is already loaded behind the overlay,
        // so just drop it to reveal the password screen.
        promptBiometricUnlock(::clearGate)
    }

    // onStop fires when the app is no longer visible (backgrounded). Stamp the
    // time and arm the eviction timer. onStart cancels it if the user comes back
    // in time. isFinishing means a real teardown, handled by onDestroy.
    override fun onStop() {
        super.onStop()
        if (isFinishing) return
        backgroundedAt = SystemClock.elapsedRealtime()
        val evict = evictionIdleMs()
        if (evict != EVICTION_NEVER) scheduleEviction(backgroundedAt + evict)
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

    // onStart fires when the app becomes visible again. Cancel the pending
    // eviction, then pick a tier from how long we were backgrounded:
    //   - under GATE: quick switch, resume untouched.
    //   - GATE..EVICTION with biometrics: the cheap UI gate - overlay + biometric,
    //     core still logged in, keys resident, so success is instant.
    //   - past EVICTION, or in the gate band with no biometrics: full eviction
    //     (lock + SPA unlock, biometric-assisted if enrolled).
    // If the overlay is already up (we backgrounded mid-gate), a sub-GATE return
    // leaves it up rather than clearing it.
    override fun onStart() {
        super.onStart()
        alarmManager.cancel(evictPendingIntent)

        val bg = backgroundedAt
        backgroundedAt = 0L
        // bg == 0 means this is not a resume from background (e.g. first launch);
        // never gate. Guards a 0 gate ("require unlock immediately") from firing
        // on the initial start.
        if (bg == 0L) return
        val idleMs = SystemClock.elapsedRealtime() - bg

        when {
            idleMs >= evictionIdleMs() -> { clearGate(); evictAndReauth() }
            idleMs >= gateIdleMs() -> {
                if (biometricAuth.canAuthenticate(this)) {
                    enterUiGate()
                } else {
                    // No biometric to run the cheap UI gate with (never enrolled,
                    // or biometrics/credentials were removed after enrolling).
                    // Escalate to full eviction rather than leave the authed
                    // session exposed past the gate window - even when eviction is
                    // set to Never, since Never assumes the biometric gate is the
                    // thing doing the locking.
                    evictAndReauth()
                }
            }
            else -> {
                // Under the gate window. If the overlay is already up (we
                // backgrounded mid-gate), re-present the biometric so a quick
                // return re-prompts instead of leaving the user on the manual
                // retry button; otherwise resume as-is.
                if (gated) uiGateBiometric()
            }
        }
    }

    // enterUiGate paints the overlay over the still-authed WebView and asks for a
    // biometric. On success the overlay clears and the resident session shows
    // through - no reload, no re-login. If already gated (we backgrounded
    // mid-gate and came back), it re-presents the prompt rather than stranding
    // the user on the overlay's manual retry button.
    private fun enterUiGate() {
        gated = true
        setOverlayUnlocking(false)
        lockOverlay.visibility = View.VISIBLE
        uiGateBiometric()
    }

    // uiGateBiometric runs the presence check for the UI gate (no CryptoObject:
    // it decrypts nothing, since the session is still resident).
    private fun uiGateBiometric() {
        if (!gated) return
        biometricAuth.authenticateSimple(this) { ok -> if (ok) clearGate() }
    }

    private fun clearGate() {
        gated = false
        if (::lockOverlay.isInitialized) lockOverlay.visibility = View.GONE
    }

    // evictAndReauth is the expensive tier: lock the core, then re-establish a
    // session. If biometric unlock is enrolled (the expected opted-in path), try
    // it first and skip the logged-out SPA entirely - on success we load straight
    // into the authed grant URL. Only if biometrics are absent, cancelled, or fail
    // do we reload into the SPA unlock screen (reloadToLoggedOutSpa), so the common
    // path avoids a wasted full SPA boot and the login-screen flash behind the
    // biometric dialog.
    private fun evictAndReauth() {
        // Raise the overlay before the (blocking) lock so the authed WebView is
        // not left on screen while app.lock() runs. It stays up until the follow-up
        // page (grant URL or logged-out SPA) lands (onWebViewPageFinished clears it).
        // Passive "Unlocking…" look: the re-auth is automatic, so no tappable control.
        setOverlayUnlocking(true)
        if (::lockOverlay.isInitialized) lockOverlay.visibility = View.VISIBLE
        lockCoreAsync {
            if (biometricAuth.isEnrolled()) {
                promptBiometricUnlock(::reloadToLoggedOutSpa)
            } else {
                reloadToLoggedOutSpa()
            }
        }
    }

    // reloadToLoggedOutSpa reloads the WebView into the SPA unlock screen after an
    // eviction. app.lock() invalidated the session server-side, so the reloaded SPA
    // sees itself as logged out; the capability cookie is still valid, so the shell
    // still loads. Safe to call from any thread. onWebViewPageFinished clears the
    // overlay once it paints (revealOnLoad).
    private fun reloadToLoggedOutSpa() = runOnUiThread {
        if (isDestroyed) return@runOnUiThread
        revealOnLoad = true
        if (::webView.isInitialized) webView.reload()
    }

    // onWebViewPageFinished clears the eviction overlay once the post-lock reload
    // has painted the logged-out SPA. Gated only by revealOnLoad so an unrelated
    // page load (or the UI gate, which does no load) never drops the overlay.
    private fun onWebViewPageFinished() {
        if (revealOnLoad) {
            revealOnLoad = false
            clearGate()
        }
    }

    // buildLockOverlay is the opaque cover painted over the WebView. It has two
    // looks, switched by setOverlayUnlocking: the interactive UI gate (a "locked"
    // panel with a retry button and a password escape hatch, in case the biometric
    // prompt is dismissed or the sensor is unusable) and, while a cold-start /
    // eviction re-auth is in flight, a passive "Unlocking…" spinner with no
    // tappable control. It builds in the interactive look; enterUiGate keeps it,
    // coldStart/evictAndReauth flip it to the passive look.
    private fun buildLockOverlay(): View {
        fun dp(v: Int) = TypedValue.applyDimension(
            TypedValue.COMPLEX_UNIT_DIP, v.toFloat(), resources.displayMetrics).toInt()

        return LinearLayout(this).apply {
            orientation = LinearLayout.VERTICAL
            gravity = Gravity.CENTER
            setBackgroundColor(Color.parseColor("#111417"))
            // Swallow taps so nothing behind the overlay is interactable.
            isClickable = true

            addView(TextView(this@MainActivity).apply {
                setText(R.string.lock_overlay_title)
                setTextColor(Color.WHITE)
                textSize = 24f
                gravity = Gravity.CENTER
            })
            // Passive-mode spinner, hidden in the interactive UI gate.
            lockSpinner = ProgressBar(this@MainActivity).apply {
                isIndeterminate = true
                indeterminateTintList = ColorStateList.valueOf(Color.WHITE)
                setPadding(0, dp(16), 0, 0)
                visibility = View.GONE
            }
            addView(lockSpinner)
            lockStatusText = TextView(this@MainActivity).apply {
                setText(R.string.lock_overlay_locked)
                setTextColor(Color.parseColor("#8a9099"))
                textSize = 14f
                gravity = Gravity.CENTER
                setPadding(0, dp(8), 0, dp(24))
            }
            addView(lockStatusText)
            lockUnlockButton = Button(this@MainActivity).apply {
                setText(R.string.lock_overlay_unlock)
                setOnClickListener { uiGateBiometric() }
            }
            addView(lockUnlockButton)
            lockUsePassword = TextView(this@MainActivity).apply {
                setText(R.string.lock_overlay_use_password)
                setTextColor(Color.parseColor("#8a9099"))
                textSize = 14f
                gravity = Gravity.CENTER
                setPadding(0, dp(16), 0, 0)
                // Escape hatch: drop to full eviction so the user can re-auth with
                // the app password if biometrics are unavailable.
                setOnClickListener { clearGate(); evictAndReauth() }
            }
            addView(lockUsePassword)
        }
    }

    // setOverlayUnlocking switches the overlay between its passive "Unlocking…"
    // look (a spinner, no tappable control) while a cold-start / eviction re-auth
    // is in flight, and its interactive UI-gate look (retry button + password
    // escape hatch). The passive look matches iOS's `unlocking` state: the system
    // biometric prompt drives the re-auth, so an Unlock button here would fire a
    // second, redundant presence check on top of it, and the gate's password
    // escape hatch is the wrong tier when the core is already being unlocked.
    private fun setOverlayUnlocking(unlocking: Boolean) {
        if (!::lockStatusText.isInitialized) return
        lockStatusText.setText(
            if (unlocking) R.string.lock_overlay_unlocking else R.string.lock_overlay_locked)
        lockSpinner.visibility = if (unlocking) View.VISIBLE else View.GONE
        lockUnlockButton.visibility = if (unlocking) View.GONE else View.VISIBLE
        lockUsePassword.visibility = if (unlocking) View.GONE else View.VISIBLE
    }

    // promptBiometricUnlock shows the system biometric prompt and, on success,
    // uses the released password to re-unlock the core, then loads the returned
    // grant URL so the WebView regains an authed session. If the prompt is
    // cancelled, the biometric fails, or the unlock errors, it runs onFallback
    // (which reloads the logged-out SPA so the user can enter the password).
    private fun promptBiometricUnlock(onFallback: () -> Unit) {
        biometricAuth.unlock(this) { pw ->
            if (pw == null) { onFallback(); return@unlock } // cancelled/failed
            thread(name = "bossw-unlock") {
                val url = try {
                    app.unlock(String(pw))
                } catch (e: Exception) {
                    // app.unlock() failed. Only drop the stored secret if the
                    // password itself is wrong (most likely the app password was
                    // changed after enrolling): a transient failure - a wallet
                    // failing to open, core mid-restart - must not cost the user
                    // their enrollment. The biometric decrypt already succeeded, so
                    // BiometricAuth cannot tell a stale password from a hiccup;
                    // verifyPassword re-checks just the password, isolating it from
                    // wallet/session errors, so we clear only when it is genuinely
                    // stale and otherwise leave enrollment intact for a retry.
                    val stalePassword = try {
                        app.verifyPassword(String(pw))
                        false
                    } catch (_: Exception) {
                        true
                    }
                    if (stalePassword) {
                        biometricAuth.clear()
                        notifyBiometricChanged()
                    }
                    runOnUiThread {
                        Toast.makeText(applicationContext, getString(R.string.unlock_failed, e.message), Toast.LENGTH_SHORT).show()
                    }
                    null
                } finally {
                    pw.fill(0)
                }
                runOnUiThread {
                    if (url != null) {
                        // Clear the overlay once the authed grant page paints.
                        revealOnLoad = true
                        loadAppUrl(url)
                    } else {
                        // Unlock failed: fall back to the logged-out SPA.
                        onFallback()
                    }
                }
            }
        }
    }

    // BiometricBridge is the window.bossw interface exposed to the SPA so its
    // Settings pane can drive the biometric opt-in. Methods are invoked on a
    // WebView binder thread, so UI work is posted to the main thread. No method
    // accepts or returns the app password: the password is captured natively in
    // showEnrollDialog and released natively in promptBiometricUnlock, never
    // crossing the JS boundary.
    private inner class BiometricBridge {
        // biometricAvailable reports whether the device has enrolled biometric
        // hardware, so the SPA can decide whether to show the opt-in toggle.
        @JavascriptInterface
        fun biometricAvailable(): Boolean = biometricAuth.canAuthenticate(this@MainActivity)

        // canGate reports whether the cheap UI gate has any mechanism to clear with
        // (biometric or device credential), so the SPA can decide whether to show
        // the "Lock after" row. On Android this equals biometricAvailable - both are
        // canAuthenticate, which already includes the device credential on API 30+.
        // It is a distinct getter only so iOS, where enrollment needs biometrics but
        // the gate accepts the device passcode, can report the two independently.
        @JavascriptInterface
        fun canGate(): Boolean = biometricAuth.canAuthenticate(this@MainActivity)

        // biometricEnrolled reports whether biometric unlock is currently on.
        @JavascriptInterface
        fun biometricEnrolled(): Boolean = biometricAuth.isEnrolled()

        // enrollBiometrics starts the opt-in (native password sheet + biometric).
        @JavascriptInterface
        fun enrollBiometrics() {
            runOnUiThread { showEnrollDialog() }
        }

        // disableBiometrics turns biometric unlock off and drops the stored secret.
        @JavascriptInterface
        fun disableBiometrics() {
            biometricAuth.clear()
            notifyBiometricChanged()
        }

        // getLockConfig returns the idle thresholds as JSON minutes for the SPA
        // Settings pane: gateMin (0 = require unlock immediately), evictMin
        // (0 = never evict).
        @JavascriptInterface
        fun getLockConfig(): String {
            val gateMin = gateIdleMs() / 60_000L
            val evict = evictionIdleMs()
            val evictMin = if (evict == EVICTION_NEVER) 0L else evict / 60_000L
            return "{\"gateMin\":$gateMin,\"evictMin\":$evictMin}"
        }

        // setLockConfig persists the idle thresholds (minutes). gateMin 0 means
        // gate on every resume; evictMin 0 means never evict.
        @JavascriptInterface
        fun setLockConfig(gateMin: Int, evictMin: Int) {
            val gateMs0 = gateMin.coerceAtLeast(0).toLong() * 60_000L
            val evictMs = if (evictMin <= 0) EVICTION_NEVER else evictMin.toLong() * 60_000L
            // The gate is the cheaper tier and must sit strictly below eviction, or
            // its band is empty and it never fires. The SPA enforces this, but
            // clamp defensively so a stale or out-of-band pair cannot disable the
            // gate. Eviction options are >= 30 min, so evictMs - 1 min stays > 0.
            val gateMs = if (evictMs != EVICTION_NEVER && gateMs0 >= evictMs) evictMs - 60_000L else gateMs0
            lockPrefs.edit().putLong(PREF_GATE_MS, gateMs).putLong(PREF_EVICT_MS, evictMs).apply()
        }
    }

    // showEnrollDialog captures the app password in a native field (never through
    // JS), verifies it against the core, then stores it behind a biometric prompt.
    private fun showEnrollDialog() {
        val input = EditText(this).apply {
            inputType = InputType.TYPE_CLASS_TEXT or InputType.TYPE_TEXT_VARIATION_PASSWORD
            hint = getString(R.string.password_hint)
        }
        AlertDialog.Builder(this)
            .setTitle(R.string.enroll_title)
            .setMessage(R.string.enroll_message)
            .setView(input)
            .setNegativeButton(R.string.cancel, null)
            .setPositiveButton(R.string.enable) { _, _ -> verifyThenEnroll(input.text.toString()) }
            .show()
    }

    // verifyThenEnroll confirms pw against the core off-thread, then (on success)
    // stores it behind a biometric prompt on the main thread.
    private fun verifyThenEnroll(pw: String) {
        thread(name = "bossw-verify") {
            val ok = try {
                app.verifyPassword(pw) // throws on a wrong password
                true
            } catch (_: Exception) {
                false
            }
            runOnUiThread {
                if (!ok) {
                    Toast.makeText(applicationContext, R.string.incorrect_password, Toast.LENGTH_SHORT).show()
                    return@runOnUiThread
                }
                val pwb = pw.toByteArray()
                biometricAuth.enroll(this, pwb) { enrolled ->
                    pwb.fill(0)
                    val msg = if (enrolled) R.string.biometric_enabled else R.string.enrollment_cancelled
                    Toast.makeText(applicationContext, msg, Toast.LENGTH_SHORT).show()
                    notifyBiometricChanged()
                }
            }
        }
    }

    // notifyBiometricChanged nudges the SPA to re-read the opt-in state after an
    // enroll/disable so its toggle reflects reality.
    private fun notifyBiometricChanged() {
        runOnUiThread {
            if (::webView.isInitialized) {
                webView.evaluateJavascript(
                    "window.dispatchEvent(new Event('bossw-biometric-changed'))", null)
            }
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
                    Toast.makeText(applicationContext, getString(R.string.lock_error, e.message), Toast.LENGTH_SHORT).show()
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
private class LoopbackWebViewClient(private val onLoaded: () -> Unit) : WebViewClient() {
    override fun onPageFinished(view: WebView, url: String) = onLoaded()

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
