import { useEffect } from 'react'

// Count of open dialogs with an operation in flight. Module state rather than
// React state: the reader (Modals' dismissal handlers) and the writers (the
// dialogs) are far apart in the tree, and the value never drives a render.
let busyCount = 0

/*
 * modalBusy reports whether any open dialog has declared an in-flight
 * operation via useModalBusy. The Modals dismissal paths (Escape, backdrop
 * click) consult it so they respect the same guard as a dialog's disabled
 * Cancel button.
 */
export function modalBusy (): boolean {
  return busyCount > 0
}

/*
 * useModalBusy declares an in-flight operation while busy is true, blocking
 * the shared dismissal paths. Dialogs pair this with disabling their own
 * Cancel control so every dismissal affordance agrees.
 */
export function useModalBusy (busy: boolean) {
  useEffect(() => {
    if (!busy) return
    busyCount++
    return () => { busyCount-- }
  }, [busy])
}
