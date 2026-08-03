import './scss/application.scss'
import * as ReactDOM from 'react-dom/client'

import { App } from './App'
import { applyTheme, storedTheme } from './theme'

// Apply the persisted theme choice before the first render so the initial
// paint is never in the wrong palette.
applyTheme(storedTheme())

const root = ReactDOM.createRoot(document.getElementById('root') as HTMLElement)
root.render(<App />)
