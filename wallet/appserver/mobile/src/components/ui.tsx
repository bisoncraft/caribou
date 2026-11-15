import { KeyboardEvent as ReactKeyboardEvent, PointerEvent as ReactPointerEvent, ReactNode, useEffect, useRef, useState } from 'react'
import t from '@caribou/shared/intl'
import { openExternal } from '../bridge'

// Icon renders an icon-font glyph by its ico-* name.
export function Icon ({ name, className }: { name: string, className?: string }) {
  return <span className={`ico-${name}${className ? ` ${className}` : ''}`} aria-hidden='true' />
}

interface NavHeaderProps {
  title?: ReactNode
  onBack?: () => void
  // close renders an X instead of a back chevron, for full-screen flows.
  close?: boolean
  right?: ReactNode
}

// NavHeader is the top bar of a pushed screen or flow: back (or close) on the
// left, a centered title, and an optional action on the right.
export function NavHeader ({ title, onBack, close, right }: NavHeaderProps) {
  return (
    <header className='navh'>
      {onBack
        ? (
          <button type='button' className='ib' onClick={onBack} aria-label={close ? t('Close') : t('Back')}>
            <Icon name={close ? 'close' : 'chevron-left'} />
          </button>
          )
        : <span className='slot' />}
      <h1>{title}</h1>
      {right ?? <span className='slot' />}
    </header>
  )
}

interface SheetProps {
  title?: ReactNode
  onClose: () => void
  // tall pins the sheet near the top of the frame (long, scrolling lists).
  tall?: boolean
  label?: string
  children: ReactNode
  foot?: ReactNode
}

// Sheet is a bottom sheet over a scrim. Tapping the scrim or the close button
// dismisses it.
export function Sheet ({ title, onClose, tall, label, children, foot }: SheetProps) {
  // Escape dismisses, like the scrim, for hardware keyboards.
  const closeRef = useRef(onClose)
  closeRef.current = onClose
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') closeRef.current() }
    document.addEventListener('keydown', onKey)
    return () => document.removeEventListener('keydown', onKey)
  }, [])
  return (
    <>
      <div className='scrim' onClick={onClose} aria-hidden='true' />
      <section className={tall ? 'sheet tall' : 'sheet'} role='dialog' aria-modal='true' aria-label={label ?? (typeof title === 'string' ? title : undefined)}>
        <span className='grab' aria-hidden='true' />
        <div className='sheet-head'>
          {title ? <h1>{title}</h1> : <span />}
          <button type='button' className='ib tile sm' onClick={onClose} aria-label={t('Close')}>
            <Icon name='close' />
          </button>
        </div>
        <div className='sheet-body'>{children}</div>
        {foot && <div className='sheet-foot'>{foot}</div>}
      </section>
    </>
  )
}

interface ToggleProps {
  on: boolean
  onChange: (on: boolean) => void
  label: string
  disabled?: boolean
}

// Toggle is an on/off switch.
export function Toggle ({ on, onChange, label, disabled }: ToggleProps) {
  return (
    <button
      type='button'
      role='switch'
      aria-checked={on}
      aria-label={label}
      className={on ? 'toggle on' : 'toggle'}
      disabled={disabled}
      onClick={() => onChange(!on)}
    />
  )
}

// Check is the visual checkbox square. The surrounding control owns the
// click and the checkbox role.
export function Check ({ on, small }: { on: boolean, small?: boolean }) {
  return (
    <span className={`chk${on ? ' on' : ''}${small ? ' sm' : ''}`} aria-hidden='true'>
      {on && <Icon name='check' />}
    </span>
  )
}

interface CheckRowProps {
  checked: boolean
  onChange: (checked: boolean) => void
  disabled?: boolean
  children: ReactNode
}

// CheckRow is a full-width checkbox with a text label.
export function CheckRow ({ checked, onChange, disabled, children }: CheckRowProps) {
  return (
    <button
      type='button'
      role='checkbox'
      aria-checked={checked}
      className='check-row'
      disabled={disabled}
      onClick={() => onChange(!checked)}
    >
      <Check on={checked} />
      <span>{children}</span>
    </button>
  )
}

export function Spinner ({ gold, small, large }: { gold?: boolean, small?: boolean, large?: boolean }) {
  return <span className={`spin${gold ? ' gold' : ''}${small ? ' sm' : ''}${large ? ' lg' : ''}`} aria-hidden='true' />
}

interface PasswordFieldProps {
  value: string
  onChange: (v: string) => void
  placeholder?: string
  autoComplete?: string
  autoFocus?: boolean
  disabled?: boolean
  id?: string
  label?: string
  adornment?: ReactNode
  bad?: boolean
}

// PasswordField is a password input with a show/hide toggle.
export function PasswordField ({ value, onChange, placeholder, autoComplete, autoFocus, disabled, id, label, adornment, bad }: PasswordFieldProps) {
  const [shown, setShown] = useState(false)
  return (
    <div className={bad ? 'field bad' : 'field'}>
      <input
        id={id}
        type={shown ? 'text' : 'password'}
        value={value}
        onChange={e => onChange(e.target.value)}
        placeholder={placeholder}
        aria-label={label ?? placeholder}
        autoComplete={autoComplete ?? 'current-password'}
        autoCapitalize='off'
        autoCorrect='off'
        spellCheck={false}
        autoFocus={autoFocus}
        disabled={disabled}
      />
      {adornment ?? (
        <button
          type='button'
          className='ib grey'
          onClick={() => setShown(!shown)}
          aria-label={shown ? t('Hide password') : t('Show password')}
          aria-pressed={shown}
        >
          <Icon name={shown ? 'eyeclosed' : 'eyeopen'} />
        </button>
      )}
    </div>
  )
}

// Segments is the setup progress bar: `of` segments, the first `n` filled.
export function Segments ({ n, of }: { n: number, of: number }) {
  return (
    <div className='segs' role='progressbar' aria-label={t('Setup progress')} aria-valuemin={1} aria-valuemax={of} aria-valuenow={n}>
      {Array.from({ length: of }, (_, i) => <span key={i} className={i < n ? 'on' : ''} />)}
    </div>
  )
}

// Toast shows a short confirmation that fades after a moment. Render it
// conditionally; it owns no timer.
export function Toast ({ children }: { children: ReactNode }) {
  return <div className='toast' role='status'>{children}</div>
}

// useNow re-renders every intervalMS and returns the current time, for
// relative-time and countdown labels.
export function useNow (intervalMS: number): number {
  const [now, setNow] = useState(Date.now())
  useEffect(() => {
    const id = setInterval(() => setNow(Date.now()), intervalMS)
    return () => clearInterval(id)
  }, [intervalMS])
  return now
}

// Glyphs the icon font lacks, drawn as inline SVG at 1em in currentColor.

const svgProps = {
  width: '1em',
  height: '1em',
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.8,
  strokeLinecap: 'round' as const,
  strokeLinejoin: 'round' as const
}

export function PasteIcon () {
  return <svg {...svgProps} aria-hidden='true'><path d='M9 3.5h6v3H9zM9 5H6.5a1 1 0 0 0-1 1v14a1 1 0 0 0 1 1h11a1 1 0 0 0 1-1V6a1 1 0 0 0-1-1H15' /></svg>
}

export function ShareIcon () {
  return <svg {...svgProps} aria-hidden='true'><path d='M12 3v12M8 7l4-4 4 4M5 12v7a1 1 0 0 0 1 1h12a1 1 0 0 0 1-1v-7' /></svg>
}

export function RefreshIcon () {
  return (
    <svg width='1em' height='1em' viewBox='0 0 16 16' aria-hidden='true'>
      <circle cx='8' cy='8' r='6' fill='none' stroke='currentColor' strokeOpacity='.3' strokeWidth='2' />
      <path d='M8 2a6 6 0 0 1 5.2 9' fill='none' stroke='currentColor' strokeWidth='2' strokeLinecap='round' />
    </svg>
  )
}

// The native shell appends ?net=<network> to the loopback URL so a non-mainnet
// build is always visibly flagged: a wallet must never be mistaken for one
// running on a different network. Mainnet (or no param) shows nothing.
const NETWORK = new URLSearchParams(window.location.search).get('net') ?? ''

// NetStrip is the non-mainnet network flag pinned above every screen, or
// null on mainnet.
export function NetStrip () {
  if (NETWORK === '' || NETWORK === 'mainnet') return null
  return <div className='net-strip'><span className='net-pill'>{NETWORK}</span></div>
}

interface ExternalLinkProps {
  href: string
  className?: string
  title?: string
  children: ReactNode
}

// ExternalLink opens an external URL in the system browser: through the
// native bridge where the shell provides it, else as a new-tab link.
export function ExternalLink ({ href, className, title, children }: ExternalLinkProps) {
  return (
    <a
      className={className}
      href={href}
      title={title}
      target='_blank'
      rel='noopener noreferrer'
      onClick={e => { if (openExternal(href)) e.preventDefault() }}
    >
      {children}
    </a>
  )
}

// Slider geometry, in px: the thumb's diameter and its inset from the track.
const THUMB = 52
const INSET = 4
// Fraction of the thumb's travel past which a release commits.
const COMMIT_AT = 0.85
// Keyboard and assistive-technology adjustment step, in percent.
const KEY_STEP = 25

interface SlideToConfirmProps {
  label: string
  busyLabel: string
  onCommit: () => void
  disabled?: boolean
  // busy holds the thumb at the end with a spinner while the action runs.
  busy?: boolean
}

// SlideToConfirm is a deliberate-confirmation control for actions that move
// funds: drag the thumb across the track to commit. A release short of the
// threshold springs back, and a tap only nudges the thumb as a hint, so a
// stray touch can never commit. The thumb is an adjustable slider for
// assistive technology and keyboards: arrow keys step it, and reaching the end
// commits. It returns to the start whenever it is not busy and not held (for
// example after a failed attempt).
export function SlideToConfirm ({ label, busyLabel, onCommit, disabled, busy }: SlideToConfirmProps) {
  const trackRef = useRef<HTMLDivElement | null>(null)
  const [x, setX] = useState(0)
  const [dragging, setDragging] = useState(false)
  const xRef = useRef(0)
  const dragRef = useRef<{ startX: number, from: number, moved: boolean } | null>(null)
  const nudgeRef = useRef<ReturnType<typeof setTimeout> | undefined>(undefined)
  const inert = Boolean(disabled || busy)

  const move = (v: number) => { xRef.current = v; setX(v) }
  const travel = () => Math.max(0, (trackRef.current?.clientWidth ?? 0) - THUMB - 2 * INSET)

  // Back to the start when the action finishes or the control disables. A
  // drag in progress is left alone, so a state change mid-drag (a quote
  // refresh) does not yank the thumb; the release decides.
  useEffect(() => { if (!busy && !dragRef.current) move(0) }, [busy, disabled])
  useEffect(() => () => clearTimeout(nudgeRef.current), [])

  const commit = () => {
    move(travel())
    onCommit()
  }

  const onPointerDown = (e: ReactPointerEvent<HTMLSpanElement>) => {
    if (inert) return
    clearTimeout(nudgeRef.current)
    e.currentTarget.setPointerCapture(e.pointerId)
    dragRef.current = { startX: e.clientX, from: xRef.current, moved: false }
    setDragging(true)
  }

  const onPointerMove = (e: ReactPointerEvent<HTMLSpanElement>) => {
    const d = dragRef.current
    if (!d) return
    const dx = e.clientX - d.startX
    if (Math.abs(dx) > 4) d.moved = true
    move(Math.min(travel(), Math.max(0, d.from + dx)))
  }

  const onPointerUp = () => {
    const d = dragRef.current
    if (!d) return
    dragRef.current = null
    setDragging(false)
    const t = travel()
    if (!d.moved) {
      // A tap: nudge as a hint that the thumb slides.
      move(12)
      nudgeRef.current = setTimeout(() => move(0), 160)
    } else if (!disabled && !busy && t > 0 && xRef.current >= t * COMMIT_AT) {
      commit()
    } else {
      move(0)
    }
  }

  const onPointerCancel = () => {
    dragRef.current = null
    setDragging(false)
    move(0)
  }

  const onKeyDown = (e: ReactKeyboardEvent<HTMLSpanElement>) => {
    if (inert) return
    const t = travel()
    if (t <= 0) return
    const step = t * KEY_STEP / 100
    let next: number | null = null
    switch (e.key) {
      case 'ArrowRight':
      case 'ArrowUp':
        next = xRef.current + step
        break
      case 'ArrowLeft':
      case 'ArrowDown':
        next = xRef.current - step
        break
      case 'Home':
        next = 0
        break
      case 'End':
        next = t
        break
    }
    if (next === null) return
    e.preventDefault()
    next = Math.min(t, Math.max(0, next))
    if (next >= t - 0.5) commit()
    else move(next)
  }

  const t = travel()
  const pct = busy ? 100 : t > 0 ? Math.round(x / t * 100) : 0
  const cls = ['slide', disabled && !busy ? 'off' : '', busy ? 'busy' : '', dragging ? 'dragging' : ''].filter(Boolean).join(' ')

  return (
    <div ref={trackRef} className={cls}>
      <span className='slide-trail' style={busy ? undefined : { width: x + THUMB + 2 * INSET }} aria-hidden='true' />
      <span className='slide-label' aria-hidden='true'>{busy ? busyLabel : label}</span>
      <span
        role='slider'
        tabIndex={inert ? -1 : 0}
        aria-label={label}
        aria-valuemin={0}
        aria-valuemax={100}
        aria-valuenow={pct}
        aria-valuetext={busy ? busyLabel : undefined}
        aria-disabled={inert || undefined}
        className='slide-thumb'
        style={busy ? undefined : { transform: `translateX(${x}px)` }}
        onPointerDown={onPointerDown}
        onPointerMove={onPointerMove}
        onPointerUp={onPointerUp}
        onPointerCancel={onPointerCancel}
        onKeyDown={onKeyDown}
      >
        {busy ? <span className='spin on-fill' aria-hidden='true' /> : <Icon name='chevron-left' className='mirror' />}
      </span>
    </div>
  )
}
