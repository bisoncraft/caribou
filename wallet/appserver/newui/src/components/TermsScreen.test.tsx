import { vi } from 'vitest'
import { describeTermsScreen } from '@caribou/shared/termsScreenSuite'
import TermsScreen from './TermsScreen'

vi.mock('@caribou/shared/http', () => ({ getJSON: vi.fn(), postJSON: vi.fn() }))

describeTermsScreen(TermsScreen)
