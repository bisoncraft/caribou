import t, { tvar } from '@caribou/shared/intl'
import { AssetVariant } from '@caribou/shared/assets'
import { TrustlineState } from '@caribou/shared/registry'
import { useTrustlineGate } from '@caribou/shared/trustline'

interface TrustlineGateProps {
  // variant is a token whose wallet reports a trust-line state other than
  // Approved. The gate replaces the deposit address until the line exists.
  variant: AssetVariant
  state: TrustlineState
  // onReceiveNetwork re-targets the receive flow at the network asset, so
  // the account can be funded to cover the line.
  onReceiveNetwork: () => void
}

// TrustlineGate is the mobile receive step for a token that needs an
// on-chain opt-in (an XRPL trust line) before its address can accept
// deposits. The shared useTrustlineGate hook owns the state and the
// establish call; this is the single-column layout over it.
export default function TrustlineGate ({ variant, state, onReceiveNetwork }: TrustlineGateProps) {
  const g = useTrustlineGate(variant, state)

  if (g.pending) {
    return (
      <div className='mb-receive'>
        <div className='mb-spinner' />
        <div>{t('Establishing trust line...')}</div>
        <div className='mb-muted'>
          {tvar('Your {{ token }} address will appear once the network confirms the trust line.', { token: g.token })}
        </div>
      </div>
    )
  }

  return (
    <div className='mb-receive'>
      <p className='mb-muted'>
        {tvar('Receiving {{ token }} on {{ network }} requires a trust line. Establishing one keeps {{ reserve }} {{ ticker }} of your balance reserved for as long as you can hold {{ token }}, plus a small network fee. The reserve is released if you later remove the trust line.',
          { token: g.token, network: g.netName, reserve: g.reserve, ticker: g.netTicker })}
      </p>
      {g.shortfall
        ? (
          <>
            <p>
              {tvar('Your {{ network }} wallet needs at least {{ amount }} more {{ ticker }} before it can hold {{ token }}.',
                { network: g.netName, amount: g.shortfall, ticker: g.netTicker, token: g.token })}
            </p>
            <button className='mb-btn' type='button' onClick={onReceiveNetwork}>
              {tvar('Receive {{ ticker }}', { ticker: g.netTicker })}
            </button>
          </>
          )
        : (
          <>
            {g.requirePW && (
              <input
                className='mb-input'
                type='password'
                autoComplete='current-password'
                placeholder={t('App password')}
                value={g.pw}
                onChange={(e) => g.setPW(e.target.value)}
                disabled={g.submitting}
              />
            )}
            <div className='mb-err' aria-live='polite'>{g.err}</div>
            <button className='mb-btn' type='button' onClick={g.establish} disabled={!g.canEstablish}>
              {g.submitting ? t('Establishing trust line...') : t('Establish trust line')}
            </button>
          </>
          )}
    </div>
  )
}
