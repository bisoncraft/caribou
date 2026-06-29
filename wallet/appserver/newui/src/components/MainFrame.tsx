import { useState, useEffect } from 'react'
import app from '@bossw/shared/application'
import TopBar from './TopBar'
import Sidebar from './Sidebar'
import Portfolio from './Portfolio'
import Trade from './Trade'
import Settings from './Settings'
import Activity from './Activity'
import Modals from './Modals'
import { PageData, FormData } from '@bossw/shared/registry'

interface MainFrameParams {
  pd: PageData
}

export default function MainFrame ({ pd }: MainFrameParams) {
  const [formData, setFormData] = useState<FormData>({ form: '', data: {} })

  useEffect(() => {
    app.registerFormSetter(setFormData)
    return () => app.unregisterFormSetter()
  }, [])

  const renderContent = () => {
    switch (pd.pageRoot) {
      case 'portfolio': return <Portfolio />
      case 'trade': return <Trade />
      case 'settings': return <Settings />
      case 'activity': return <Activity />
      default: return (<div className="flex-center">Unknown page: {pd.pageRoot}</div>)
    }
  }
  return (
    <div className="d-flex flex-column h-100">
      <TopBar />
      <div className="d-flex flex-grow-1 align-items-stretch">
        <Sidebar />
        <div className="flex-grow-1 position-relative fade-in">{renderContent()}</div>
      </div>
      <Modals fd={formData} />
    </div>
  )
}
