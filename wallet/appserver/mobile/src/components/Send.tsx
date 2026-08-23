import t, { tvar } from '@caribou/shared/intl'
import { AssetVariant, networkAsset, isVariantActive } from '@caribou/shared/assets'
import { useSendForm } from '@caribou/shared/useSendForm'
import app from '@caribou/shared/application'
import Doc from '@caribou/shared/doc'
import CoinLogo from '@caribou/shared/components/CoinLogo'

interface SendProps {
  ticker?: string
  onClose: () => void
}

// Send walks asset -> network -> form. The shared useSendForm hook owns the
// state, address validation, fee estimation, Max and submit; this component is
// the single-column mobile layout over it.
export default function Send ({ ticker, onClose }: SendProps) {
  const {
    ta, variant, address, setAddress, amount, subtract, setSubtract, destTag,
    addrValid, txFee, feeKnown, feeErr, estimating, pw, setPW, sending, err, netErr,
    requirePW, isWithdrawer, available, feeUI, overBalance, canSubmit, supportsDestTag,
    amountChanged, destTagChanged, pickAsset, pickVariant, setMax, submit
  } = useSendForm({ ticker, onSent: onClose })

  let body
  let title = t('Send')
  if (!ta) {
    const list = (app.tickerList ?? []).filter(item => item.avail > 0)
    body = list.length === 0
      ? <p className='mb-muted mb-pad'>{t('No funds available to send.')}</p>
      : (
        <ul className='mb-asset-list'>
          {list.map(item => (
            <li key={item.ticker} className='mb-asset-row mb-tappable' onClick={() => pickAsset(item)}>
              <CoinLogo ticker={item.ticker} alt={item.name} className='mb-coin-logo' />
              <div className='mb-asset-id'>
                <span className='mb-asset-ticker'>{item.ticker}</span>
                <span className='mb-asset-name mb-muted'>{item.name}</span>
              </div>
            </li>
          ))}
        </ul>
        )
  } else if (!variant) {
    title = tvar('Send {{ name }}', { name: ta.name })
    const vs = ta.variants.filter(isVariantActive)
    body = vs.length === 0
      ? <p className='mb-muted mb-pad'>{t('No active network for this asset. Activate one first.')}</p>
      : (
      <>
        <p className='mb-muted mb-pad'>{t('Choose the network to send from.')}</p>
        <ul className='mb-asset-list'>
          {vs.map((v: AssetVariant) => {
            const net = networkAsset(v)
            return (
              <li key={v.assetID} className='mb-asset-row mb-tappable' onClick={() => pickVariant(v)}>
                <CoinLogo ticker={net?.unitInfo.conventional.unit ?? ''} className='mb-coin-logo' />
                <div className='mb-asset-id'><span className='mb-asset-ticker'>{net?.name ?? ''}</span></div>
                <div className='mb-asset-bal'>
                  <span className='mb-asset-amt mono'>{Doc.formatCoinValue(v.bal.available, v.ui)} {v.ticker}</span>
                </div>
                {netErr[v.assetID] && <div className='mb-err mb-row-err'>{netErr[v.assetID]}</div>}
              </li>
            )
          })}
        </ul>
      </>
        )
  } else {
    title = tvar('Send {{ name }}', { name: ta.name })
    body = (
      <form className='mb-send' onSubmit={(e) => { e.preventDefault(); submit() }}>
        <label className='mb-field-label'>{t('Recipient address')}</label>
        <input
          className={addrValid === false ? 'mb-input mb-input-bad' : 'mb-input'}
          type='text'
          spellCheck={false}
          autoComplete='off'
          autoFocus
          placeholder={t('address')}
          value={address}
          onChange={(e) => setAddress(e.target.value)}
        />

        {supportsDestTag && (
          <>
            <div className='mb-field-row'>
              <label className='mb-field-label'>{t('Destination tag')}</label>
              <span className='mb-muted'>{t('optional')}</span>
            </div>
            <input
              className='mb-input'
              type='text'
              inputMode='numeric'
              placeholder={t('tag')}
              value={destTag}
              onChange={destTagChanged}
            />
          </>
        )}

        <div className='mb-field-row'>
          <label className='mb-field-label'>{t('Amount')}</label>
          <span className='mb-link' onClick={setMax}>
            {t('Available:')} {Doc.formatCoinValue(available, variant.ui)} {variant.ticker}
          </span>
        </div>
        <div className='mb-amt'>
          <input
            className='mb-input'
            type='text'
            inputMode='decimal'
            placeholder='0.00'
            value={amount}
            onChange={amountChanged}
          />
          <button type='button' className='mb-btn mb-btn-small mb-btn-secondary' onClick={setMax}>{t('Max')}</button>
        </div>

        {isWithdrawer && (
          <label className='mb-check-row'>
            <input type='checkbox' checked={subtract} onChange={(e) => setSubtract(e.target.checked)} />
            <span>{t('Subtract fee from amount')}</span>
          </label>
        )}

        <div className='mb-send-summary'>
          <div className='mb-summary-row'>
            <span className='mb-muted'>{t('Network fee')}</span>
            <span>{estimating ? '…' : feeKnown ? `${Doc.formatCoinValue(txFee, feeUI)} ${feeUI?.conventional.unit}` : '—'}</span>
          </div>
          {feeErr && <div className='mb-err'>{feeErr}</div>}
        </div>

        {requirePW && (
          <input
            className='mb-input'
            type='password'
            autoComplete='current-password'
            placeholder={t('App password')}
            value={pw}
            onChange={(e) => setPW(e.target.value)}
            disabled={sending}
          />
        )}

        {overBalance && <div className='mb-err'>{t('Amount exceeds available balance.')}</div>}
        <div className='mb-err' aria-live='polite'>{err}</div>

        <button className='mb-btn' type='submit' disabled={!canSubmit}>
          {sending ? t('Sending...') : t('Send')}
        </button>
      </form>
    )
  }

  return (
    <div className='mb-overlay'>
      <header className='mb-overlay-head'>
        <button className='mb-back' type='button' onClick={onClose} aria-label={t('Back')}>‹</button>
        <span>{title}</span>
      </header>
      <div className='mb-overlay-body'>{body}</div>
    </div>
  )
}
