import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { cleanup, fireEvent, render } from '@testing-library/react'
import app from '@caribou/shared/application'
import { resetApp } from '@caribou/shared/testUtils'
import Modals from './Modals'
import { useModalBusy } from './modalBusy'

beforeEach(() => {
  resetApp()
})

afterEach(() => {
  cleanup()
})

describe('Modals', () => {
  it('closes the open dialog on Escape', () => {
    const setForm = vi.spyOn(app, 'setForm')
    render(<Modals fd={{ form: 'receive', data: {} }} />)

    fireEvent.keyDown(window, { key: 'Escape' })
    expect(setForm).toHaveBeenCalledWith({ form: '', data: {} })
    setForm.mockRestore()
  })

  it('ignores Escape while a dialog has an operation in flight', () => {
    // Stands in for a dialog that declares an in-flight operation, e.g. an
    // activation dialog mid-request.
    function Busy (): null {
      useModalBusy(true)
      return null
    }
    const setForm = vi.spyOn(app, 'setForm')
    const { unmount } = render(
      <>
        <Busy />
        <Modals fd={{ form: 'receive', data: {} }} />
      </>
    )

    fireEvent.keyDown(window, { key: 'Escape' })
    expect(setForm).not.toHaveBeenCalled()

    // The guard lifts when the busy dialog unmounts.
    unmount()
    render(<Modals fd={{ form: 'receive', data: {} }} />)
    fireEvent.keyDown(window, { key: 'Escape' })
    expect(setForm).toHaveBeenCalledWith({ form: '', data: {} })
    setForm.mockRestore()
  })

  it('does not listen for Escape while no dialog is open', () => {
    const setForm = vi.spyOn(app, 'setForm')
    render(<Modals fd={{ form: '', data: {} }} />)

    fireEvent.keyDown(window, { key: 'Escape' })
    expect(setForm).not.toHaveBeenCalled()
    setForm.mockRestore()
  })
})
