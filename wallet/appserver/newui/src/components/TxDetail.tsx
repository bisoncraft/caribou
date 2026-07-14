import { useState, FormEvent, ReactNode } from 'react'
import t, { tvar } from '@bossw/shared/intl'
import app from '@bossw/shared/application'
import Doc from '@bossw/shared/doc'
import { WalletTransaction } from '@bossw/shared/registry'
import { txTypeLabel, txAmountSign, txShowsAmount, txIsIncoming } from '@bossw/shared/tx'
import { explorerTxURL } from '@bossw/shared/coinexplorers'
import CoinLogo from '@bossw/shared/components/CoinLogo'

interface TxDetailParams {
  assetID: number
  tx: WalletTransaction
}

const noSubmit = (e: FormEvent) => e.preventDefault()

function Row ({ label, children }: { label: string, children: ReactNode }) {
  return (
    <div className="network-stat d-flex justify-content-between align-items-center fs14 border-bottom">
      <span className="grey fs13">{label}</span>
      <span className="text-right">{children}</span>
    </div>
  )
}

// TxDetail is a modal showing full information for a single transaction: amount,
// network, status, fee, the transaction ID with a copy button, and a link to a
// block explorer.
export default function TxDetail ({ assetID, tx }: TxDetailParams) {
  const [copied, setCopied] = useState(false)

  const close = () => app.setForm({ form: '', data: {} })

  const asset = app.asset(assetID)
  const ui = asset.unitInfo
  const ticker = ui.conventional.unit

  // Fees are paid in the settlement-network asset: the parent network for
  // tokens, the asset itself for natives.
  const feeAssetID = asset.token ? asset.token.networkID : assetID
  const feeUI = app.unitInfo(feeAssetID)
  const feeTicker = feeUI.conventional.unit
  const networkName = asset.token ? app.asset(asset.token.networkID).name : asset.name

  const rate = app.idRate(assetID) ?? 0
  const feeRate = app.idRate(feeAssetID) ?? 0

  const sign = txAmountSign(tx.type)
  const amountUsd = Doc.usdString(tx.amount, ui.conventional.conversionFactor, rate)
  const feeUsd = Doc.usdString(tx.fees, feeUI.conventional.conversionFactor, feeRate)

  const status = (!tx.confirmed && tx.confirms)
    ? tvar('{{ current }}/{{ target }} confs', { current: String(tx.confirms.current), target: String(tx.confirms.target) })
    : tx.confirmed ? t('Confirmed') : t('Pending')

  const dateStr = tx.timestamp > 0 ? new Date(tx.timestamp * 1000).toLocaleString() : t('Pending')

  const explorer = explorerTxURL(assetID, tx.id)

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(tx.id)
      setCopied(true)
      setTimeout(() => setCopied(false), 2000)
    } catch {
      // clipboard API unavailable — silently ignore
    }
  }

  return (
    <form className="modal form-width-300" onSubmit={noSubmit}>
      <header>
        <CoinLogo ticker={ticker} className="mini-icon me-2" />
        <span>{txTypeLabel(tx.type)}</span>
      </header>

      <div className="px-3">
        {txShowsAmount(tx.type) && (
          <Row label={t('Amount')}>
            <span className={`mono ${txIsIncoming(tx.type) ? 'tx-amt-pos' : ''}`}>
              {sign}{Doc.formatCoinValue(tx.amount, ui)} {ticker}
            </span>
            {amountUsd && <div className="grey fs12">{amountUsd}</div>}
          </Row>
        )}
        <Row label={t('Network')}>{networkName}</Row>
        <Row label={t('Status')}>{status}</Row>
        <Row label={t('Network fee')}>
          <span className="mono">{Doc.formatCoinValue(tx.fees, feeUI)} {feeTicker}</span>
          {feeUsd && <div className="grey fs12">{feeUsd}</div>}
        </Row>
        {tx.recipient && (
          <Row label={t('Recipient')}>
            <span className="mono fs12 break-all">{tx.recipient}</span>
          </Row>
        )}
        <Row label={t('Date')}>{dateStr}</Row>
      </div>

      <div className="px-3 pt-2">
        <div className="fs12 grey">{t('Transaction ID')}</div>
        <div className="d-flex align-items-center">
          <span className="mono fs12 flex-grow-1 break-all">{tx.id}</span>
          <button type="button" className="small subtle ms-2 hoverbg" onClick={copy}>
            {copied ? t('Copied!') : t('Copy')}
          </button>
        </div>
      </div>

      <div className="flex-stretch-column p-3">
        {explorer && (
          <a
            href={explorer}
            target="_blank"
            rel="noopener noreferrer"
            className="text-center fs13 pb-3 accent-blue-text"
          >
            {t('View in block explorer')}
          </a>
        )}
        <button type="button" onClick={close}>{t('Close')}</button>
      </div>
    </form>
  )
}
