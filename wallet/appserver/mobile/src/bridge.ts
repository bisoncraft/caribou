// The native bridge the mobile shell injects as window.caribou. It is absent
// in a plain browser, so every use is guarded. No method takes or returns the
// app password: enrollBiometrics captures it in a native field, and unlock
// releases it natively, so the secret never reaches this JS.
export interface CaribouBridge {
  // biometricAvailable reports whether the device can ENROLL biometric
  // key-release (iOS: actual biometrics only, since the Keychain item needs
  // .biometryCurrentSet). Gates the enroll toggle.
  biometricAvailable(): boolean
  // canGate reports whether the cheap UI gate has any mechanism to clear with:
  // biometrics OR the device passcode. On Android this equals
  // biometricAvailable (both are canAuthenticate); on iOS it is broader, so a
  // passcode-only device can still configure and run the gate. Gates the
  // "Lock after" row.
  canGate(): boolean
  biometricEnrolled(): boolean
  enrollBiometrics(): void
  disableBiometrics(): void
  // Idle-lock thresholds in minutes: gateMin (0 = require unlock immediately),
  // evictMin (0 = never evict). getLockConfig returns JSON {gateMin, evictMin}.
  getLockConfig(): string
  setLockConfig(gateMin: number, evictMin: number): void
  // shareText opens the system share sheet with the given text. Optional:
  // shells that predate it lack the method.
  shareText?(text: string): void
  // shareAppLog hands the application log to the system share sheet.
  // Optional, as above.
  shareAppLog?(): void
  // openURL opens an external http(s) URL in the system browser. Optional,
  // as above. WebViews generally ignore target=_blank, so without it an
  // external link may do nothing.
  openURL?(url: string): void
}

declare global {
  interface Window { caribou?: CaribouBridge }
}

export function bridge (): CaribouBridge | undefined {
  return typeof window !== 'undefined' ? window.caribou : undefined
}

// The native shells only let the WebView navigate to the loopback origin, and
// the Android shell has no download handler, so a plain link or download is a
// dead control inside a shell. Each capability is keyed on the specific
// bridge method; the plain web fallbacks apply only with no shell at all (a
// desktop browser during development).

// canOpenExternal reports whether external URLs can be opened: through the
// bridge's openURL inside a shell, or as an ordinary link outside one.
export function canOpenExternal (): boolean {
  const b = bridge()
  return !b || typeof b.openURL === 'function'
}

// canExportAppLog reports whether the application log can be exported:
// through the bridge's shareAppLog inside a shell, or as a download outside
// one.
export function canExportAppLog (): boolean {
  const b = bridge()
  return !b || typeof b.shareAppLog === 'function'
}

// canShareText reports whether shareText has a backend: the native bridge, or
// the Web Share API where the WebView or browser provides it.
export function canShareText (): boolean {
  return typeof bridge()?.shareText === 'function' || typeof navigator.share === 'function'
}

// ShareResult is the outcome of shareText: the sheet opened, the user
// dismissed it, or no share backend worked.
export type ShareResult = 'shared' | 'dismissed' | 'failed'

// shareText opens a share sheet for text, through the native bridge or the
// Web Share API. An embedded WebView may expose navigator.share yet reject
// the call (no user activation, no permission), which reports 'failed' so
// the caller can fall back rather than leave a dead button.
export async function shareText (text: string): Promise<ShareResult> {
  const b = bridge()
  if (typeof b?.shareText === 'function') {
    b.shareText(text)
    return 'shared'
  }
  if (typeof navigator.share === 'function') {
    try {
      await navigator.share({ text })
      return 'shared'
    } catch (e) {
      return e instanceof DOMException && e.name === 'AbortError' ? 'dismissed' : 'failed'
    }
  }
  return 'failed'
}

// openExternal opens an external URL through the native bridge when the
// shell provides openURL, returning whether it did. Callers fall back to the
// browser's own link handling otherwise.
export function openExternal (url: string): boolean {
  const b = bridge()
  if (typeof b?.openURL !== 'function') return false
  b.openURL(url)
  return true
}

// copyText writes text to the clipboard, returning whether it succeeded.
export async function copyText (text: string): Promise<boolean> {
  try {
    await navigator.clipboard.writeText(text)
    return true
  } catch {
    return false
  }
}

// readClipboard returns the clipboard text, or '' when it is unavailable or
// permission is denied.
export async function readClipboard (): Promise<string> {
  try {
    return (await navigator.clipboard.readText()) ?? ''
  } catch {
    return ''
  }
}
