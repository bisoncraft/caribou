import { MouseEvent, useRef } from 'react'
import Receive from './Receive'
import Send from './Send'
import Swap from './Swap'
import ActivateAsset from './ActivateAsset'
import { FormData } from '../js/registry'
import app from '../js/application'

interface ModalsParams {
  fd: FormData
}

export default function Modals ({ fd }: ModalsParams) {
  // Only close when a full click both starts and ends on the backdrop. The
  // click event fires on the common ancestor of the mousedown and mouseup
  // targets, so a drag that starts inside the form (e.g. selecting text in an
  // input) and releases on the backdrop would otherwise close the modal. We
  // record whether the press began on the backdrop and require that too.
  const downOnBackground = useRef(false)

  const onBackgroundMouseDown = (e: MouseEvent) => {
    downOnBackground.current = e.target === e.currentTarget
  }

  const onBackgroundClick = (e: MouseEvent) => {
    const shouldClose = downOnBackground.current && e.target === e.currentTarget
    downOnBackground.current = false
    if (shouldClose) app.setForm({ form: '', data: {} })
  }

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
      onMouseDown={onBackgroundMouseDown}
      onClick={onBackgroundClick}>
      {fd.form && renderContent()}
    </div>
  )
}
