import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { cleanup, fireEvent, render, screen } from '@testing-library/react'
import { resetApp } from '@caribou/shared/testUtils'
import Sidebar from './Sidebar'

beforeEach(() => {
  resetApp()
})

afterEach(() => {
  cleanup()
  document.body.classList.remove('sidebar-collapsed')
  document.body.style.removeProperty('--sidebar-width')
  window.localStorage.removeItem('sidebarCollapsed')
})

describe('Sidebar', () => {
  it('collapses and expands, persisting the choice', () => {
    render(<Sidebar />)
    const toggle = screen.getByRole('button', { name: /collapse/i })
    expect(toggle.getAttribute('aria-expanded')).toBe('true')
    expect(document.body.classList.contains('sidebar-collapsed')).toBe(false)

    fireEvent.click(toggle)
    expect(document.body.classList.contains('sidebar-collapsed')).toBe(true)
    expect(JSON.parse(window.localStorage.getItem('sidebarCollapsed')!)).toBe(true)
    expect(toggle.getAttribute('aria-expanded')).toBe('false')
    expect(toggle.getAttribute('aria-label')).toBe('Expand')

    fireEvent.click(toggle)
    expect(document.body.classList.contains('sidebar-collapsed')).toBe(false)
    expect(JSON.parse(window.localStorage.getItem('sidebarCollapsed')!)).toBe(false)
    expect(toggle.getAttribute('aria-label')).toBe('Collapse')
    // The expanded width is content-measured and applied as an inline var.
    expect(document.body.style.getPropertyValue('--sidebar-width')).toMatch(/px$/)
  })

  it('restores the persisted collapsed state on mount', () => {
    window.localStorage.setItem('sidebarCollapsed', 'true')
    render(<Sidebar />)
    const toggle = screen.getByRole('button', { name: /expand/i })
    expect(toggle.getAttribute('aria-expanded')).toBe('false')
  })
})
