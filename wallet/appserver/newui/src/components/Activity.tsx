import t from '@caribou/shared/intl'

export default function Activity () {
  return (
    <div className="fill-abs flex-center grey">
      <div className="text-center">
        <span className="ico-bell fs50 d-block mb-3" />
        <div className="fs18">{t('Activity')}</div>
        <div className="fs13 mt-2">{t('Transaction history coming soon.')}</div>
      </div>
    </div>
  )
}
