import './scss/application.scss'
import * as ReactDOM from 'react-dom/client'

import { App } from './App'
import { applyTheme, storedTheme } from './theme'
import { applySidebarCollapsed, storedSidebarCollapsed } from './layout'

// Apply the persisted theme and layout choices before the first render so
// the initial paint is never in the wrong palette or sidebar state.
applyTheme(storedTheme())
applySidebarCollapsed(storedSidebarCollapsed())

const root = ReactDOM.createRoot(document.getElementById('root') as HTMLElement)
root.render(<App />)
