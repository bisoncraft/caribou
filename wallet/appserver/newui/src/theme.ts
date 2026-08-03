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
 * data-theme. It does not persist the choice, so it is safe to call at boot
 * without pinning first-time users to the current default.
 */
export function applyTheme (theme: Theme) {
  document.body.dataset.theme = theme
}

/* setTheme applies the theme and persists the choice to localStorage. */
export function setTheme (theme: Theme) {
  applyTheme(theme)
  State.storeLocal(State.themeLK, theme)
}
