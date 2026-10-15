import { afterEach, describe, expect, it } from 'vitest'
import { cleanup, render } from '@testing-library/react'
import LegalText from './LegalText'

describe('LegalText', () => {
  afterEach(cleanup)

  // A wrapped paragraph is normal markdown; it must not split into several
  // paragraphs on screen.
  it('joins consecutive lines into one paragraph', () => {
    const { container } = render(<LegalText markdown={'You hold\nyour own **keys**.\n\nNo telemetry.'} />)
    const paras = container.querySelectorAll('p')
    expect(paras.length).toBe(2)
    expect(paras[0].textContent).toBe('You hold your own keys.')
    expect(paras[1].textContent).toBe('No telemetry.')
  })

  it('ends a paragraph at a heading or bullet', () => {
    const { container } = render(<LegalText markdown={'# Terms\nintro line\n## Keys\ntext\n- one\n- two'} />)
    expect(container.querySelector('h3')?.textContent).toBe('Terms')
    expect(container.querySelector('h4')?.textContent).toBe('Keys')
    expect(Array.from(container.querySelectorAll('p')).map(p => p.textContent)).toEqual(['intro line', 'text'])
    expect(container.querySelectorAll('li').length).toBe(2)
  })
})
