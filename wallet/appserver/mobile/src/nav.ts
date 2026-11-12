import { createContext, useContext } from 'react'

// Tab is one of the bottom tab bar's root screens.
export type Tab = 'portfolio' | 'activity' | 'networks' | 'settings'

// Layer is a screen opened over a tab. Pushed screens (asset, network) keep
// the tab bar; flows (receive, send, swap, swapProgress) take the whole frame.
export type Layer =
  | { kind: 'asset', ticker: string }
  | { kind: 'network', assetID: number }
  | { kind: 'receive', ticker?: string, assetID?: number }
  | { kind: 'send', ticker?: string, assetID?: number }
  | { kind: 'swap', ticker?: string }
  | { kind: 'swapProgress', depositAddress: string }

const flowKinds = new Set<Layer['kind']>(['receive', 'send', 'swap', 'swapProgress'])

// isFlow reports whether a layer takes the whole frame (no tab bar).
export function isFlow (l: Layer): boolean {
  return flowKinds.has(l.kind)
}

export interface Nav {
  tab: Tab
  // setTab switches tabs, closing any open layers.
  setTab: (tab: Tab) => void
  // push opens a layer over the current screen.
  push: (layer: Layer) => void
  // replace swaps the top layer for another (e.g. swap form -> progress).
  replace: (layer: Layer) => void
  // back closes the top layer.
  back: () => void
}

export const NavContext = createContext<Nav>({
  tab: 'portfolio',
  setTab: () => {},
  push: () => {},
  replace: () => {},
  back: () => {}
})

export function useNav (): Nav {
  return useContext(NavContext)
}
