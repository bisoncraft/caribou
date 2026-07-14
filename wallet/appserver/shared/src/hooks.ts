import { useState, useCallback, useEffect, DependencyList } from 'react'
import app from './application'
import { CoreNote } from './registry'

// useForceUpdate returns a stable callback that triggers a re-render. It backs
// components that read app state held outside React (balances, fiat rates, note
// feed) and need to redraw when that state changes.
export function useForceUpdate (): () => void {
  const [, setTick] = useState(0)
  return useCallback(() => setTick(x => x + 1), [])
}

// useCoreSubscriptions registers app listeners for the component's lifetime,
// unregistering all of them on unmount or when deps change. Each listener is
// optional: onBalance and onRate fire on balance and fiat-rate updates, and
// onNote receives the raw note feed. deps follows the usual useEffect contract.
export function useCoreSubscriptions (listeners: {
  onBalance?: () => void
  onRate?: () => void
  onNote?: (note: CoreNote) => void
}, deps: DependencyList): void {
  const { onBalance, onRate, onNote } = listeners
  useEffect(() => {
    const unregister: Array<() => void> = []
    if (onBalance) {
      const k = app.registerBalanceUpdater(onBalance)
      unregister.push(() => app.unregisterBalanceUpdater(k))
    }
    if (onRate) {
      const k = app.registerFiatRateUpdater(onRate)
      unregister.push(() => app.unregisterFiatRateUpdater(k))
    }
    if (onNote) {
      const k = app.registerNoteFeed(onNote)
      unregister.push(() => app.unregisterNoteFeed(k))
    }
    return () => unregister.forEach(u => u())
  }, deps)
}
