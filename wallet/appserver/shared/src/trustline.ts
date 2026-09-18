import { useCallback, useReducer, useState } from 'react'
import t from './intl'
import app from './application'
import { postJSON } from './http'
import Doc from './doc'
import { useCoreSubscriptions } from './hooks'
import { TickerAsset, AssetVariant, networkAsset, normalizedTicker } from './assets'
import { ApprovalStatus, CoreNote, TrustlineState, WalletNote, WalletState } from './registry'

// Trust-line gating for the receive flow. A token whose wallet reports a
// trust-line state (an XRPL issued currency) cannot receive until the account
// has opted in on-chain, so the deposit address is withheld behind a gate
// that explains the cost, offers the opt-in, or points at the network asset
// when the account cannot afford it. The state and API calls live here; each
// bundle renders its own markup over them, the same split as useSendForm.

// trustlineGate returns the variant's trust-line state when its wallet needs
// an on-chain opt-in that has not confirmed yet, else null. Only tokens whose
// wallet reports a trust-line state are gated; every other variant's address
// is usable as soon as its wallet exists.
export function trustlineGate (v: AssetVariant): TrustlineState | null {
  const tl = v.token ? app.user?.assets[v.assetID]?.wallet?.trustline : undefined
  return tl && tl.status !== ApprovalStatus.Approved ? tl : null
}

// minDepositNote returns the minimum first deposit for a native variant whose
// account does not exist on its ledger yet (zero balance on a network with a
// base reserve), else 0. A first deposit below it is rejected by the network at
// the sender, so the address is shown with that requirement.
export function minDepositNote (v: AssetVariant, w: WalletState | undefined): number {
  if (v.token || !w || !w.minDeposit) return 0
  const b = w.balance
  const total = b ? b.available + b.locked + b.immature : 0
  return total === 0 ? w.minDeposit : 0
}

// networkTickerAsset returns the ticker asset of v's settlement network and
// the network's native variant within it, for re-targeting a receive flow at
// the network asset (the gate's "Receive XRP" shortcut). Null when the network
// is not loaded.
export function networkTickerAsset (v: AssetVariant): { ta: TickerAsset, variant: AssetVariant | null } | null {
  const net = networkAsset(v)
  if (!net) return null
  const ta = app.tickerMap?.[normalizedTicker(net)]
  if (!ta) return null
  return { ta, variant: ta.variantLookup[net.id] ?? null }
}

// useReceiveUpdates re-renders the caller when wallet state relevant to
// receiving on variant changes: balance updates (the native minimum-deposit
// note keys on balance), wallet-state and wallet-config notes, and trust-line
// notes for the variant or its network.
export function useReceiveUpdates (variant: AssetVariant | null): void {
  const [, tick] = useReducer((n: number) => n + 1, 0)
  useCoreSubscriptions({
    onBalance: tick,
    onNote: (note: CoreNote) => {
      if (!variant) return
      const ids = [variant.assetID, variant.token?.networkID]
      switch (note.type) {
        case 'walletstate':
        case 'walletconfig':
          tick()
          break
        case 'walletnote': {
          const n = note as WalletNote
          if (n.payload.route === 'trustline' && ids.includes(n.payload.assetID)) tick()
          break
        }
      }
    }
  }, [variant])
}

// TrustlineGateView is what a gate component renders: the derived copy
// inputs, the establishment action with its password gating, and the
// pending flag.
export interface TrustlineGateView {
  // pending is true while an establishing transaction awaits validation.
  pending: boolean
  // token is the token ticker; netName and netTicker describe the network.
  token: string
  netName: string
  netTicker: string
  // reserve is the network amount the line keeps held, formatted; shortfall
  // is what the account still needs before it can establish, formatted, or
  // empty when the line is affordable now.
  reserve: string
  shortfall: string
  // Password gating mirrors Send: the app password is required when the
  // send-requires-password setting is on.
  requirePW: boolean
  pw: string
  setPW: (pw: string) => void
  submitting: boolean
  err: string
  // canEstablish is true when the line is affordable, no request is in
  // flight, and any required password has been entered.
  canEstablish: boolean
  // establish submits the opt-in. On success the wallet announces Pending
  // (or Approved, if the line already existed) and the caller re-renders
  // through useReceiveUpdates.
  establish: () => Promise<void>
}

// useTrustlineGate derives the gate view for a variant whose wallet reports
// state (a non-Approved trust-line state, per trustlineGate).
export function useTrustlineGate (variant: AssetVariant, state: TrustlineState): TrustlineGateView {
  const network = networkAsset(variant)
  const netUI = network?.unitInfo
  const requirePW = app.appSettings.sendRequiresPassword
  const [pw, setPW] = useState('')
  const [submitting, setSubmitting] = useState(false)
  const [err, setErr] = useState('')

  const establish = useCallback(async () => {
    setSubmitting(true)
    setErr('')
    const r = await postJSON('/api/establishtrustline', { assetID: variant.assetID, pass: pw })
    setSubmitting(false)
    setPW('')
    if (!r.ok) setErr(r.msg || t('Failed to establish trust line.'))
  }, [variant.assetID, pw])

  return {
    pending: state.status === ApprovalStatus.Pending,
    token: variant.ticker,
    netName: network?.name ?? '',
    netTicker: netUI?.conventional.unit ?? '',
    reserve: Doc.formatCoinValue(state.reserve, netUI),
    shortfall: state.shortfall > 0 ? Doc.formatCoinValue(state.shortfall, netUI) : '',
    requirePW,
    pw,
    setPW,
    submitting,
    err,
    canEstablish: state.shortfall === 0 && !submitting && (!requirePW || pw !== ''),
    establish
  }
}
