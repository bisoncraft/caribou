import t, { tvar } from '@bossw/shared/intl'

// Trade is a placeholder. DEX trading is not yet implemented in this UI.
export default function Trade () {
  return (
    <div className="fill-abs flex-center grey">
      {tvar('{{ feature }} (coming soon)', { feature: t('Trade') })}
    </div>
  )
}
