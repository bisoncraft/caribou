import { MouseEvent, useEffect, useRef } from 'react'
import Receive from './Receive'
import Send from './Send'
import Swap from './Swap'
import ActivateAsset from './ActivateAsset'
import ActivateNetwork from './ActivateNetwork'
import TxDetail from './TxDetail'
import { FormData } from '@caribou/shared/registry'
import app from '@caribou/shared/application'
import { modalBusy } from './modalBusy'

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
    if (shouldClose && !modalBusy()) app.setForm({ form: '', data: {} })
  }

  // Escape dismisses the open dialog, matching the backdrop click. Bound only
  // while a form is open so the listener isn't a permanent global. Both paths
  // defer to modalBusy so a dialog that disables its Cancel mid-operation
  // can't be dismissed around the guard.
  useEffect(() => {
    if (!fd.form) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !modalBusy()) app.setForm({ form: '', data: {} })
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [fd.form])

  const renderContent = () => {
    switch (fd.form) {
      case 'receive': return <Receive ticker={fd.data?.ticker} />
      case 'send': return <Send ticker={fd.data?.ticker} />
      case 'swap': return <Swap />
      case 'activateasset': return <ActivateAsset ticker={fd.data.ticker} />
      case 'activatenetwork': return <ActivateNetwork assetID={fd.data.assetID} />
      case 'txdetail': return <TxDetail assetID={fd.data.assetID} tx={fd.data.tx} />
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
