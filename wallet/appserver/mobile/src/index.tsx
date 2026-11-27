import './scss/application.scss'
import * as ReactDOM from 'react-dom/client'
import { applyTheme, storedTheme } from '@caribou/shared/theme'

import { App } from './App'

// Apply the persisted theme before the first render so the initial paint
// (including the unlock and setup screens) is never in the wrong palette.
applyTheme(storedTheme())

const root = ReactDOM.createRoot(document.getElementById('root') as HTMLElement)
root.render(<App />)
