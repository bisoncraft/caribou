import State from '@caribou/shared/state'

// The color themes defined in scss/vars.scss. Each palette is keyed off the
// data-theme attribute on body; warm is also the no-attribute default.
export const themes = ['warm', 'cool', 'light'] as const
export type Theme = typeof themes[number]

/*
 * storedTheme returns the persisted theme choice, falling back to the default
 * warm theme when nothing (or an unknown value) is stored.
 */
export function storedTheme (): Theme {
  const stored = State.fetchLocal(State.themeLK) as string | null
  return themes.find(th => th === stored) ?? 'warm'
}

/*
 * applyTheme switches the app to the given theme by tagging body with
 * data-theme, and matches the favicon to the theme's antler-mark color. It
 * does not persist the choice, so it is safe to call at boot without pinning
 * first-time users to the current default.
 */
export function applyTheme (theme: Theme) {
  document.body.dataset.theme = theme
  // Same mapping as the in-app logo: brown antlers for warm, blue for cool
  // and light. The cache-busting query from the server-rendered href is
  // carried over.
  const fav = document.querySelector<HTMLLinkElement>('link[rel="icon"]')
  if (!fav) return
  const color = theme === 'warm' ? 'brown' : 'blue'
  const q = fav.href.split('?')[1]
  fav.href = `/img/favicon-${color}.png${q ? `?${q}` : ''}`
}

/* setTheme applies the theme and persists the choice to localStorage. */
export function setTheme (theme: Theme) {
  applyTheme(theme)
  State.storeLocal(State.themeLK, theme)
}
