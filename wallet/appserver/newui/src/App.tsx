import { useEffect, useReducer, useState } from 'react'
import app from '@caribou/shared/application'
import UnlockForm from './components/UnlockForm'
import MainFrame from './components/MainFrame'
import InitWizard from './components/InitWizard'
import TermsScreen from './components/TermsScreen'
import Loading from './components/Loading'
import {
  AppState,
  PageData
} from '@caribou/shared/registry'

// Disable form submissions (we handle them ourselves) at a global level.
document.addEventListener('submit', e => e.preventDefault())

export const App = () => {
  const [, reRender] = useReducer(x => x + 1, 0)
  const [appState, setAppState] = useState<AppState | null>(null)
  const [pageData, setPageData] = useState<PageData | null>(null)

  const resetAppState = async () => {
    setAppState(await app.fetchAppState())
  }

  const start = async () => {
    await app.start(setPageData, reRender)
    await resetAppState()
    if (!app.user && !pageData) app.loadPage('') // reset URL
  }

  useEffect(() => { start() }, [])

  if (appState === null) return <Loading />

  // A fresh install accepts before the wizard. An existing wallet is asked
  // after it unlocks, and again whenever the terms version changes.
  if (appState.user) {
    if (!appState.termsAccepted) return <TermsScreen onAccepted={resetAppState} />
    return <MainFrame pd={pageData} />
  } else if (appState.inited) {
    return <UnlockForm setUnlocked={() => { resetAppState(); app.loadPage('portfolio') }} />
  } else if (!appState.termsAccepted) {
    return <TermsScreen onAccepted={resetAppState} />
  } else return <InitWizard setInited={() => { resetAppState(); app.loadPage('portfolio') }} />
}
