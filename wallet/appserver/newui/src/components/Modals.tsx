import { MouseEvent } from 'react'
import Receive from './Receive'
import Send from './Send'
import Swap from './Swap'
import ActivateAsset from './ActivateAsset'
import { FormData } from '../js/registry'
import app from '../js/application'

interface ModalsParams {
  fd: FormData
}

const onBackgroundClick = (e: MouseEvent) => {
  if (e.target === e.currentTarget) {
    app.setForm({ form: '', data: {} })
  }
}

export default function Modals ({ fd }: ModalsParams) {
  const renderContent = () => {
    switch (fd.form) {
      case 'receive': return <Receive ticker={fd.data?.ticker} />
      case 'send': return <Send ticker={fd.data?.ticker} />
      case 'swap': return <Swap />
      case 'activateasset': return <ActivateAsset ticker={fd.data.ticker} />
      default: return (<div className="flex-center">Unknown form: {fd.form}</div>)
    }
  }

  return (
    <div className={`fill-abs flex-center modals ${fd.form ? '' : 'd-none'}`}
      onClick={onBackgroundClick}>
      {fd.form && renderContent()}
    </div>
  )
}
