import t, { tvar } from '../js/intl'

// Trade is a placeholder. DEX trading is not yet implemented in this UI.
export default function Trade () {
  return (
    <div className="fill-abs flex-center grey">
      {tvar('{{ feature }} (coming soon)', { feature: t('Trade') })}
    </div>
  )
}
