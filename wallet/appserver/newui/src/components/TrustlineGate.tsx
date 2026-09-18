import t, { tvar } from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { AssetVariant } from '@caribou/shared/assets'
import { TrustlineState } from '@caribou/shared/registry'
import { useTrustlineGate } from '@caribou/shared/trustline'
import PasswordInput from './PasswordInput'
import Spinner from './Spinner'

interface TrustlineGateProps {
  // variant is a token whose wallet reports a trust-line state other than
  // Approved. The gate replaces the deposit address until the line exists.
  variant: AssetVariant
  state: TrustlineState
}

// TrustlineGate is the Receive step for a token that needs an on-chain opt-in
// (an XRPL trust line) before its address can accept deposits. It explains the
// cost, offers the opt-in when the account can afford it, points at the
// network asset when it cannot, and shows progress while the opt-in confirms.
// The shared useTrustlineGate hook owns the state and the establish call;
// this is the desktop layout over it. State changes arrive through the
// wallet's trust-line note; the parent re-renders on them.
export default function TrustlineGate ({ variant, state }: TrustlineGateProps) {
  const g = useTrustlineGate(variant, state)
  const receiveNetwork = () => app.setForm({ form: 'receive', data: { ticker: g.netTicker } })

  if (g.pending) {
    return (
      <div className="px-3 pb-3 text-center">
        <div className="d-flex align-items-center justify-content-center fs14 pb-2">
          <Spinner size={14} className="me-2" />
          {t('Establishing trust line...')}
        </div>
        <div className="grey fs13">
          {tvar('Your {{ token }} address will appear once the network confirms the trust line.', { token: g.token })}
        </div>
      </div>
    )
  }

  return (
    <div className="px-3 pb-3">
      <div className="fs13 pb-2">
        {tvar('Receiving {{ token }} on {{ network }} requires a trust line. Establishing one keeps {{ reserve }} {{ ticker }} of your balance reserved for as long as you can hold {{ token }}, plus a small network fee. The reserve is released if you later remove the trust line.',
          { token: g.token, network: g.netName, reserve: g.reserve, ticker: g.netTicker })}
      </div>
      {g.shortfall
        ? (
          <>
            <div className="fs13 pb-2 gold">
              {tvar('Your {{ network }} wallet needs at least {{ amount }} more {{ ticker }} before it can hold {{ token }}.',
                { network: g.netName, amount: g.shortfall, ticker: g.netTicker, token: g.token })}
            </div>
            <div className="flex-stretch-column">
              <button type="button" onClick={receiveNetwork}>
                {tvar('Receive {{ ticker }}', { ticker: g.netTicker })}
              </button>
            </div>
          </>
          )
        : (
          <>
            {g.requirePW && (
              <div className="pb-2">
                <label className="fs12 grey">{t('App password')}</label>
                <PasswordInput value={g.pw} onChange={e => g.setPW(e.target.value)} disabled={g.submitting} />
              </div>
            )}
            {g.err && <div key={g.err} className="errcolor fs12 pb-2 text-center">{g.err}</div>}
            <div className="flex-stretch-column">
              <button type="button" className="feature" onClick={g.establish} disabled={!g.canEstablish}>
                {g.submitting
                  ? <span className="d-inline-flex align-items-center justify-content-center"><Spinner className="me-2" />{t('Establishing trust line...')}</span>
                  : t('Establish trust line')}
              </button>
            </div>
          </>
          )}
    </div>
  )
}
