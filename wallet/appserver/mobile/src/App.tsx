import { useEffect, useReducer, useState } from 'react'
import app from '@caribou/shared/application'
import { AppState } from '@caribou/shared/registry'
import UnlockForm from './components/UnlockForm'
import InitWizard from './components/InitWizard'
import TermsScreen from './components/TermsScreen'
import Portfolio from './components/Portfolio'
import Settings from './components/Settings'
import Send from './components/Send'
import Receive from './components/Receive'
import Activate from './components/Activate'

// Disable form submissions (we handle them ourselves) at a global level.
document.addEventListener('submit', e => e.preventDefault())

type Tab = 'portfolio' | 'activity' | 'settings'
type Overlay = 'send' | 'receive' | 'activate' | null

const TABS: { id: Tab, label: string, icon: string }[] = [
  { id: 'portfolio', label: 'Portfolio', icon: '◈' },
  { id: 'activity', label: 'Activity', icon: '↹' },
  { id: 'settings', label: 'Settings', icon: '⚙' }
]

// The native shell appends ?net=<network> to the loopback URL so a non-mainnet
// build is always visibly flagged. A wallet must never be mistaken for one
// running on a different network. Mainnet (or no param) shows nothing.
const NETWORK = new URLSearchParams(window.location.search).get('net') ?? ''
const NET_BADGE = NETWORK !== '' && NETWORK !== 'mainnet'
  ? <div className='mb-net-badge'>{NETWORK}</div>
  : null

// App is the mobile shell. It reuses the shared core lifecycle (app.start over
// the loopback WS/HTTP transport) but renders a bottom-tab, single-column
// layout instead of the desktop sidebar paradigm. Send / Receive / Activate
// open as full-screen overlays over the tab shell.
export const App = () => {
  const [, reRender] = useReducer(x => x + 1, 0)
  const [appState, setAppState] = useState<AppState | null>(null)
  const [tab, setTab] = useState<Tab>('portfolio')
  const [overlay, setOverlay] = useState<Overlay>(null)

  const resetAppState = async () => {
    setAppState(await app.fetchAppState())
  }

  const start = async () => {
    // The mobile shell does its own tab routing, so page-load data is unused.
    await app.start(() => {}, reRender)
    await resetAppState()
  }

  useEffect(() => { start() }, [])

  const closeOverlay = () => setOverlay(null)

  let screen
  if (appState === null) {
    screen = (
      <div className='mb-screen mb-center'>
        <div className='mb-spinner' />
      </div>
    )
  } else if (!appState.user && !appState.inited && !appState.termsAccepted) {
    // A fresh install accepts the terms before creating a wallet.
    screen = <TermsScreen onAccepted={resetAppState} />
  } else if (appState.user && !appState.termsAccepted) {
    // An existing wallet is asked after it unlocks, and again whenever the
    // terms version changes.
    screen = <TermsScreen onAccepted={resetAppState} />
  } else if (!appState.user) {
    screen = appState.inited
      ? <UnlockForm onUnlocked={() => { resetAppState(); app.loadPage('portfolio') }} />
      : <InitWizard setInited={() => { resetAppState(); app.loadPage('portfolio') }} />
  } else if (overlay === 'send') {
    screen = <Send onClose={closeOverlay} />
  } else if (overlay === 'receive') {
    screen = <Receive onClose={closeOverlay} />
  } else if (overlay === 'activate') {
    screen = <Activate onClose={closeOverlay} />
  } else {
    screen = (
      <div className='mb-app'>
        <main className='mb-content'>
          {tab === 'portfolio' && (
            <Portfolio
              onSend={() => setOverlay('send')}
              onReceive={() => setOverlay('receive')}
              onActivate={() => setOverlay('activate')}
            />
          )}
          {tab === 'activity' && (
            <section><h2>Activity</h2><p className='mb-muted'>Coming soon.</p></section>
          )}
          {tab === 'settings' && <Settings />}
        </main>
        <nav className='mb-tabbar'>
          {TABS.map(tb => (
            <button
              key={tb.id}
              className={tab === tb.id ? 'mb-tab active' : 'mb-tab'}
              onClick={() => setTab(tb.id)}
            >
              <span className='mb-tab-icon'>{tb.icon}</span>
              <span className='mb-tab-label'>{tb.label}</span>
            </button>
          ))}
        </nav>
      </div>
    )
  }

  return <>{NET_BADGE}{screen}</>
}
