import { useLayoutEffect, useRef, useState } from 'react'
import t from '@caribou/shared/intl'
import app from '@caribou/shared/application'
import { applySidebarCollapsed, setSidebarCollapsed, storedSidebarCollapsed } from '../layout'

const navButtons = [
  { page: 'portfolio', icon: 'ico-portfolio', label: 'Portfolio' },
  { page: 'activity', icon: 'ico-bell', label: 'Activity' },
  { page: 'networks', icon: 'ico-network', label: 'Networks' }
]

// Minimum expanded width. The content measurement takes over whenever the
// labels need more room than this.
const minExpandedWidth = 160

function sidebarButtonClass (page: string, button: string) {
  return `sidebar-btn p-2 pe-3 d-flex align-items-center mt-2 text-left fs18 ${page === button ? 'selected' : ''}`
}

export default function Sidebar () {
  const page = app.pageData.pageRoot
  const btns = useRef<Record<string, HTMLButtonElement | null>>({})
  const sidebarDiv = useRef<HTMLDivElement | null>(null)
  const [cursor, setCursor] = useState<{ top: number, height: number } | null>(null)
  const [collapsed, setCollapsed] = useState(storedSidebarCollapsed())

  // The cursor bar tracks the active nav button. Measured from the DOM so it
  // follows font/locale-driven size differences; when no nav button matches
  // the page (settings, asset detail), the bar unmounts. Re-measured once
  // webfonts land, since a late font swap shifts the button metrics.
  useLayoutEffect(() => {
    const measure = () => {
      const btn = btns.current[page]
      if (!btn) {
        setCursor(null)
        return
      }
      setCursor({ top: btn.offsetTop, height: btn.offsetHeight })
    }
    measure()
    let stale = false
    if (document.fonts) document.fonts.ready.then(() => { if (!stale) measure() })
    return () => { stale = true }
  }, [page, collapsed])

  // The expanded width is content-driven, measured from a hidden max-content
  // clone rather than the live element: the live buttons stretch to the
  // current width, so scroll-based metrics feed back the width being set
  // instead of the intrinsic content width. The clone counts every padding
  // exactly once, returns a stable value on re-measure, and leaves any
  // in-flight width transition undisturbed. Labels must stay in layout
  // (opacity/clipping, never display: none) so the clone sees them.
  const measuredWidth = () => {
    const div = sidebarDiv.current
    if (!div) return minExpandedWidth
    const clone = div.cloneNode(true) as HTMLDivElement
    clone.style.width = 'max-content'
    clone.style.position = 'absolute'
    clone.style.visibility = 'hidden'
    document.body.appendChild(clone)
    const w = clone.getBoundingClientRect().width
    clone.remove()
    return Math.max(minExpandedWidth, Math.ceil(w))
  }

  // Measurement runs after render but before paint, so the collapsed class
  // (which narrows button padding) is already off the body by the time an
  // expand is measured. Only a collapse-state change animates: measurement
  // corrections (mount, font load, locale change) apply with the transition
  // suppressed, because the clone measurement forces a layout that would
  // otherwise give the browser a "before" width to animate from — a visible
  // settle on every page load.
  const lastCollapsed = useRef<boolean | null>(null)
  useLayoutEffect(() => {
    const toggled = lastCollapsed.current !== null && lastCollapsed.current !== collapsed
    lastCollapsed.current = collapsed
    if (collapsed) return
    let stale = false
    const apply = (animate: boolean) => {
      if (stale) return
      const div = sidebarDiv.current
      if (animate || !div) {
        applySidebarCollapsed(false, measuredWidth())
        return
      }
      div.style.transition = 'none'
      applySidebarCollapsed(false, measuredWidth())
      div.getBoundingClientRect() // commit the width before the transition returns
      div.style.transition = ''
    }
    apply(toggled)
    // Re-measure once webfonts land; a late font swap changes label widths.
    // The staleness guard keeps a slow font load from re-expanding a sidebar
    // the user has since collapsed.
    if (document.fonts) document.fonts.ready.then(() => apply(false))
    return () => { stale = true }
  }, [collapsed, app.lang])

  const toggle = () => {
    const next = !collapsed
    setCollapsed(next)
    // On expand, the measurement effect supplies the width after the
    // collapsed styles are gone.
    setSidebarCollapsed(next)
  }

  return (
    <div ref={sidebarDiv} className="sidebar flex-stretch-column p-3 position-relative">
      {cursor && <span className="sidebar-cursor" style={{ top: cursor.top, height: cursor.height }} />}
      <div className={`d-flex ${collapsed ? 'justify-content-center' : 'justify-content-end'}`}>
        <button className="sidebar-toggle p-1"
          aria-expanded={!collapsed}
          aria-label={collapsed ? t('Expand') : t('Collapse')}
          title={collapsed ? t('Expand') : t('Collapse')}
          onClick={toggle}>
          <span className={`ico-chevron-left fs14${collapsed ? ' mirror-x' : ''}`} />
        </button>
      </div>
      {navButtons.map(({ page: btnPage, icon, label }) => (
        <button key={btnPage} className={sidebarButtonClass(page, btnPage)}
          ref={el => { btns.current[btnPage] = el }}
          title={collapsed ? t(label) : undefined}
          onClick={() => app.loadPage(btnPage)}>
          <span className={`${icon} me-3`} />
          <span className="sidebar-label">{t(label)}</span>
        </button>
      ))}
      <button className={sidebarButtonClass(page, '')} title={collapsed ? t('New') : undefined}>
        <span className="ico-add me-3" />
        <span className="sidebar-label">{t('New')}</span>
      </button>
    </div>
  )
}
