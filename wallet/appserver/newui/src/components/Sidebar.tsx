import t from '@caribou/shared/intl'
import app from '@caribou/shared/application'

function sidebarButtonClass (page: string, button: string) {
  return `p-2 pe-3 d-flex align-items-center subtle mt-2 text-left fs18 ${page === button ? 'selected' : ''}`
}

export default function Sidebar () {
  const page = app.pageData.pageRoot
  return (
    <div className="sidebar flex-stretch-column p-3">
      <span className="fs12 grey">{t('VIEWS')}</span>
      <button className={sidebarButtonClass(page, 'portfolio')}
        onClick={() => app.loadPage('portfolio')}>
        <span className="ico-portfolio me-3" />
        <span>{t('Portfolio')}</span>
      </button>
      <button className={sidebarButtonClass(page, 'trade')}
        onClick={() => app.loadPage('trade')}>
        <span className="ico-barchart me-3" />
        <span>{t('Trade')}</span>
      </button>
      <button className={sidebarButtonClass(page, 'activity')}
        onClick={() => app.loadPage('activity')}>
        <span className="ico-bell me-3" />
        <span>{t('Activity')}</span>
      </button>
      <button className={sidebarButtonClass(page, 'networks')}
        onClick={() => app.loadPage('networks')}>
        <span className="ico-network me-3" />
        <span>{t('Networks')}</span>
      </button>
      <span className="mt-4 fs12 grey">{t('FAVORITES')}</span>
      <button className={sidebarButtonClass(page, '')}>
        <span className="ico-add me-3" />
        <span>{t('New')}</span>
      </button>
    </div>
  )
}
