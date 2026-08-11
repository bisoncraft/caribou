import State from '@caribou/shared/state'

/*
 * storedSidebarCollapsed returns the persisted sidebar collapse preference.
 */
export function storedSidebarCollapsed (): boolean {
  return Boolean(State.fetchLocal(State.sidebarCollapsedLK))
}

/*
 * applySidebarCollapsed sets the sidebar's collapse state by tagging body
 * with the sidebar-collapsed class; vars.scss maps the class to the rail
 * width. The expanded width is content-measured by the Sidebar component and
 * applied here as an inline --sidebar-width override, which is cleared when
 * collapsing so the class rule can take effect. The choice is not persisted,
 * so this is safe to call at boot.
 */
export function applySidebarCollapsed (collapsed: boolean, expandedWidth?: number) {
  document.body.classList.toggle('sidebar-collapsed', collapsed)
  if (collapsed) document.body.style.removeProperty('--sidebar-width')
  else if (expandedWidth) document.body.style.setProperty('--sidebar-width', `${Math.ceil(expandedWidth)}px`)
}

/* setSidebarCollapsed applies the collapse state and persists the choice. */
export function setSidebarCollapsed (collapsed: boolean, expandedWidth?: number) {
  applySidebarCollapsed(collapsed, expandedWidth)
  State.storeLocal(State.sidebarCollapsedLK, collapsed)
}
