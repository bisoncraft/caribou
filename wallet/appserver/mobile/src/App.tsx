import { useCallback, useEffect, useMemo, useReducer, useState } from 'react'
import t from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { AppState } from '@caribou/shared/registry'
import { Icon, NetStrip, Spinner } from './components/ui'
import { NavContext, Nav, Tab, Layer, isFlow } from './nav'
import UnlockForm from './components/UnlockForm'
import InitWizard from './components/InitWizard'
import TermsScreen from './components/TermsScreen'
import Portfolio from './components/Portfolio'
import AssetScreen from './components/AssetScreen'
import Activity from './components/Activity'
import Networks from './components/Networks'
import NetworkScreen from './components/NetworkScreen'
import Settings from './components/Settings'
import Send from './components/Send'
import Receive from './components/Receive'
import Swap from './components/Swap'
import SwapProgressScreen from './components/SwapProgress'

// Disable form submissions (handled in components) at a global level.
document.addEventListener('submit', e => e.preventDefault())

const TABS: { id: Tab, label: () => string, icon: string }[] = [
  { id: 'portfolio', label: () => t('Portfolio'), icon: 'portfolio' },
  { id: 'activity', label: () => t('Activity'), icon: 'bell' },
  { id: 'networks', label: () => t('Networks'), icon: 'network' },
  { id: 'settings', label: () => t('Settings'), icon: 'gear' }
]

function renderTab (tab: Tab) {
  switch (tab) {
    case 'portfolio': return <Portfolio />
    case 'activity': return <Activity />
    case 'networks': return <Networks />
    case 'settings': return <Settings />
  }
}

function renderLayer (l: Layer) {
  switch (l.kind) {
    case 'asset': return <AssetScreen key={l.ticker} ticker={l.ticker} />
    case 'network': return <NetworkScreen key={l.assetID} assetID={l.assetID} />
    case 'receive': return <Receive ticker={l.ticker} assetID={l.assetID} />
    case 'send': return <Send ticker={l.ticker} assetID={l.assetID} />
    case 'swap': return <Swap ticker={l.ticker} />
    case 'swapProgress': return <SwapProgressScreen key={l.depositAddress} depositAddress={l.depositAddress} />
  }
}

// App is the mobile shell. It reuses the shared core lifecycle (app.start over
// the loopback WS/HTTP transport) and routes between the first-run screens,
// the bottom-tab shell, pushed screens, and full-screen flows.
export const App = () => {
  const [, reRender] = useReducer(x => x + 1, 0)
  const [appState, setAppState] = useState<AppState | null>(null)
  const [tab, setTabState] = useState<Tab>('portfolio')
  const [stack, setStack] = useState<Layer[]>([])

  const resetAppState = async () => {
    setAppState(await app.fetchAppState())
  }

  const start = async () => {
    // The mobile shell does its own routing, so page-load data is unused.
    await app.start(() => {}, reRender)
    await resetAppState()
  }

  useEffect(() => { start() }, [])

  const setTab = useCallback((next: Tab) => {
    setStack([])
    setTabState(next)
  }, [])
  const push = useCallback((l: Layer) => setStack(s => [...s, l]), [])
  const replace = useCallback((l: Layer) => setStack(s => [...s.slice(0, -1), l]), [])
  const back = useCallback(() => setStack(s => s.slice(0, -1)), [])
  const nav: Nav = useMemo(() => ({ tab, setTab, push, replace, back }), [tab, setTab, push, replace, back])

  const onUnlocked = () => {
    setStack([])
    setTabState('portfolio')
    resetAppState()
  }

  let screen
  if (appState === null) {
    screen = <div className='center-col'><Spinner large /></div>
  } else if (!appState.termsAccepted && (appState.user || !appState.inited)) {
    // A fresh install accepts the terms before creating a wallet. An existing
    // wallet is asked after it unlocks, and again whenever the terms version
    // changes.
    screen = <TermsScreen onAccepted={resetAppState} />
  } else if (!appState.user) {
    screen = appState.inited
      ? <UnlockForm onUnlocked={onUnlocked} />
      : <InitWizard setInited={onUnlocked} />
  } else {
    const top = stack[stack.length - 1]
    if (top && isFlow(top)) {
      screen = renderLayer(top)
    } else {
      screen = (
        <div className='frame'>
          <main className='frame' style={{ flex: 1, minHeight: 0 }}>
            {top ? renderLayer(top) : renderTab(tab)}
          </main>
          <nav className='tabbar' aria-label={t('Primary')}>
            {TABS.map(tb => (
              <button
                key={tb.id}
                type='button'
                className={tab === tb.id ? 'tab on' : 'tab'}
                aria-current={tab === tb.id && !top ? 'page' : undefined}
                onClick={() => setTab(tb.id)}
              >
                <span className='pill'><Icon name={tb.icon} /></span>
                {tb.label()}
              </button>
            ))}
          </nav>
        </div>
      )
    }
  }

  return (
    <NavContext.Provider value={nav}>
      <NetStrip />
      <div className='app-body'>{screen}</div>
    </NavContext.Provider>
  )
}
