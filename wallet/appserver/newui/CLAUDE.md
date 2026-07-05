# newui — CSS & styling conventions

The desktop UI (`wallet/appserver/newui/`) is styled **utility-first**. These
rules govern SCSS under `src/scss/` and the `className`s written in TSX. Apply
them while writing new styles, not only as a later cleanup pass.

## Utility-first: custom classes hold only what utilities can't express

A custom (component) CSS class must contain **only** properties that no utility
class can express.

- If every property of a custom class is utility-expressible, delete the class
  and put the utilities on the element's `className`.
- If some properties are utility-expressible and some are not, move the
  expressible ones to the `className` and leave only the rest in the custom class.

### Utilities already exist for these — never put them in a custom class

- Display: `d-flex`, `d-inline-flex`, `d-grid`, `d-block`, `d-none`
- Flex: `flex-column`, `flex-row`, `align-items-*`, `justify-content-*`, `flex-grow-1`
- Text: `text-left` / `text-right` / `text-center`, `mono`, `demi`,
  `grey` / `errcolor` / `warncolor`
- Font size: the `.fsN` scale (see below)
- Icon / logo size: the `.icon-N` scale (see below)
- Spacing on the standard scale: `m* / p*` (`m/mx/my/mt/me/mb/ms`,
  `p/px/py/pt/pe/pb/ps`) with steps `1=.25 / 2=.5 / 3=1 / 4=1.5 / 5=3` rem, plus
  `-auto`
- Box: `w-25/50/75/100`, `w-auto`, `h-100`, `border`,
  `border-top/right/bottom/left`, `br5` (5px radius)
- Misc: `pointer`, `hoverbg`, `fill-abs`, `flex-center`,
  `flex-stretch-column`, `position-relative`

### No utility exists for these — a custom class is justified

`gap`, `flex-wrap`, `overflow`, `letter-spacing`, `transition`, `filter`,
`transform`, `grid-template-*`, `max-width`, backgrounds, non-5px
`border-radius`, off-scale sizes/spacing, and pseudo-selectors (`:hover`,
`:last-child`, `:focus`). `line-height` also has no utility, but usually isn't
needed — see below.

## Font sizes — the `.fsN` scale

Font sizes come from the generated `.fsN` utilities (the `$font-sizes` loop in
`typography.scss`). When a size is missing, add it to `$font-sizes` — only sizes
actually used in the codebase. Never inline `font-size` or wrap a one-off size in
a custom class.

## Icon / logo sizes — the `.icon-N` scale

Square icon/logo sizes come from the generated `.icon-N` utilities (the
`$icon-sizes` loop in `icons.scss`); each sets an equal `width`/`height`. Snap
arbitrary sizes to the scale — exact pixels (44 vs 40) are not sacred; prefer a
scale step. Add a new step to `$icon-sizes` only when the scale genuinely lacks a
needed size.

## Coin logos — never hide pixels

Coin/asset logos (`shared/img/coins/`, rendered via `<CoinLogo>`) are square
artwork designed to display exactly as drawn. Size them with an `.icon-N` class
and nothing else. Do NOT add `border-radius` (clips the corners), `object-fit`
(a no-op for square art in a square box), or any `overflow`-style clipping.

## Spacing — prefer margin utilities

Prefer margin utilities over bespoke spacing. Replace a flex-container `gap` with
a margin on a child (`me-3`, etc.) where practical, and use `mt-N` / `mb-N` for
block spacing, snapping to the standard scale. Reach for a small margin before
introducing a custom `line-height`.

## Colors

Promote a repeated color to `vars.scss` (`--accent-*`) before referencing it in a
component. A custom class may hold a color only when no color utility covers it.
