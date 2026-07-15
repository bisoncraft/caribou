# UI re-skin plan — "Warm & Natural"

Re-skin the desktop wallet UI (`wallet/appserver/newui`) to the Caribou
"Warm & Natural" visual language. Color, typography, and elevation change;
layout and functional density do not.

## Sources of truth

- [docs/caribou-style-guide.md](caribou-style-guide.md) — authoritative design
  direction. §2 defines the palette, §3 the type system, §11 the
  wallet-specific application (use the dark / warm-charcoal column; the wallet
  ships dark by identity).
- `design/caribou-current-design.fig` — generated documentation of the
  pre-reskin UI. It is not hand-edited; it gets regenerated from the updated
  code in phase 5. (The file is a git-lfs pointer. Note for tooling: the Figma
  MCP page listing for the imported file shows only the Foundations page, but
  all 23 pages resolve when addressed by node ID.)

## Current state

- Design tokens: `wallet/appserver/newui/src/scss/vars.scss` — CSS custom
  properties on `body`, single dark theme, ~30 tokens, 82 `var(--…)`
  references in `components.scss`.
- Fonts: Inter and Inconsolata (variable, Latin-subset woff2) in
  `wallet/appserver/shared/font/`, declared in
  `newui/src/scss/typography.scss`. Both are reused as-is; Fraunces is the
  only addition.
- Font sizing uses fixed px utility classes (`.fs8`–`.fs50`); the scale is
  deliberately untouched by this re-skin.

## Phase 1 — token remap (`vars.scss`)

| Current | Value | New value (warm dark) | Notes |
| --- | --- | --- | --- |
| `--body-bg` | `#30343d` | `#17130d` | |
| `--modal-wrapper-bg` | `#464e5c` | `#1f1a13` | becomes `--surface`; unlock/init backdrop reads as a section band |
| `--form-input-bg` | `#3c424e` | ~`#2e2720` (derived) | one step above `--surface-raised`; contrast-validate |
| `--border-color` | `#383f4b` | `#3a3227` | |
| `--scrim-bg` | `rgba(0,0,0,.47)` | keep, possibly deepen to `.5` | |
| `--text-color` | `#dfe2e1` | `#f1e9da` | |
| `--text-grey` | `#999` | `#b3a890` | |
| `--text-error` / `--text-danger` / `--text-warning` | cool reds / `#f82e` | derived warm (terracotta / clay family) | guide: status colors sit warm, not neon; AA-validate |
| `--accent-blue` (fills: pills, highlighted rows, feature buttons) | `#1d50b5` | `#2c7d5c` | guide `--accent-strong` |
| `--accent-blue-text` | `#6fa3d8` | `#7bbf9f` | guide `--accent` (sage links/highlights) |
| `--accent-blue-border` / `-hover` / `-strong` | blues | derived pine/sage steps; near-white → warm off-white | usage-by-usage mapping pass |
| `--accent-syncing` | `#e6a817` | `#d9b25e` | folds into `--gold` |
| `--btn-feature-hover-bg` | `#176699` (lightens) | `color-mix(in srgb, var(--accent-strong) 86%, #000)` | guide: accent hover darkens and keeps white text |
| (new) | — | `--surface-raised: #262019`, `--gold: #d9b25e`, `--gold-soft: #e8ca86`, `--focus: #7bbf9f`, `--font-display` | |

Alongside the value changes, the `--accent-blue-*` family is renamed to
`--accent-*` — a green value in a variable named `accent-blue` is naming
debt. Mechanical sweep across `components.scss` and the few `.tsx`
references. One collision to resolve explicitly: the current
`--accent-blue-strong` (near-white emphasis text) is **not** the guide's
`--accent-strong` (primary button fill); the rename pass must not conflate
them.

## Phase 2 — typography

- Add Fraunces (variable, Latin subset, ~37 KB, from
  `@fontsource-variable/fraunces`, SIL OFL) to `wallet/appserver/shared/font/`
  with its license file; `@font-face` in `typography.scss`; new
  `--font-display` token.
- Apply as an opt-in class at specific sites only: the portfolio total
  numeral (the fs50 balance — the guide's "gold serif numeral" move), the
  TopBar total, and InitWizard/Unlock headings. `letter-spacing: -0.015em`,
  `text-wrap: balance` on those headings.
- Everything functional stays Inter (guide §3: only the largest headings go
  serif).

## Phase 3 — elevation, components, motion

- Warm-tinted shadows. In dark theme, lift comes primarily from
  `--surface-raised` plus a 1px `--border-color` border; the big shadow
  (`0 20px 55px rgb(0 0 0 / .5)`) is reserved for modals.
- `:focus-visible` ring using `--focus` on all interactives.
- Buttons: `:active { transform: translateY(1px) }`; transitions 150–200ms
  ease-out, `transform`/`opacity`/color only; honor
  `prefers-reduced-motion`.
- Gold hairline rules where the design uses dividers around totals.
- `.modals` scrim currently uses `backdrop-filter: blur(5px)`
  (`components.scss`). This is a static modal scrim, not the guide's
  forbidden sticky-scroll case, but the restraint principle (and the pending
  decision below) points at dropping it and letting a deepened scrim do the
  work.
- Coin logos are untouched: rendered as shipped, `object-fit: contain`,
  never clipped or radius-masked (already standing policy, reaffirmed by
  guide §4/§7).

## Phase 4 — validation

- Contrast is validated by script, not by eye (guide §10): a small committed
  script checks every text/background token pair for WCAG AA 4.5:1 (3:1 for
  large/graphical), runnable as an npm script.
- `npm run build` plus eslint on touched files.
- Live run-through with screenshots of unlock, init wizard, portfolio, and
  each modal, compared against the Figma originals for layout regressions
  (colors differ by design).

## Phase 5 — design file regeneration

- Regenerate the Figma design pages from the re-skinned code and replace
  `design/caribou-current-design.fig` (requires git-lfs installed locally to
  commit the pointer).
- Fix known stale artifacts from the previous generation pass while at it:
  the four duplicate "BTC" asset pills and the duplicated blank-logo Bitcoin
  row in MainFrame.

## Open decisions

1. **Token rename** — rename `--accent-blue-*` → `--accent-*`, or value-swap
   only. Recommendation: rename.
2. **Radii** — keep `--border-radius: 4px`, or soften to 6px controls plus a
   new `--radius-card` (12–16px) for modals/cards. Recommendation: 6px + 16px
   (guide §4 allows either; 6/16 is the warmer choice).
3. **Scrim blur** — drop the `.modals` `backdrop-filter`, or keep it since it
   is not the sticky-scroll case. Recommendation: drop.
4. **Light theme** — dark-only for now per guide §11, with `vars.scss`
   structured so the bone-paper light column can drop in later.
   Recommendation: dark-only.

## Out of scope

- Mobile UI (`wallet/mobile`) re-skin — follow-up pass.
- "Boss Wallet" → "Caribou" copy/branding rename — separate track; this
  re-skin does not touch product copy.
