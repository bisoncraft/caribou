# Caribou — "Warm & Natural" style guide

A portable description of the caribou.you visual language, written so it can be
implemented in another UI (e.g. the wallet). Values are given as CSS custom
properties; **adapt them to your stack** (SCSS variables, CSS-in-JS, etc.) — the
names and relationships matter more than the syntax.

The wallet ships dark by identity, so if you're re-skinning it, use the **dark
(warm charcoal)** column throughout and see [§11 Applying to the wallet](#11-applying-to-the-wallet).

---

## 1. The idea in one paragraph

Warm, natural, and quietly premium — the opposite of the cold, neon, "crypto
dashboard" look. Bone-paper and warm charcoal instead of pure white / slate
black; **pine green** as the brand/interactive color; **gold** as a sparing
accent; a **serif** (Fraunces) for headings to read human and editorial, with
Inter for everything functional. Restraint over effects: the polish comes from
color, type, spacing, and soft elevation — not gradients, glows, or animation.

**Do:** warm tones, serif display headings, generous whitespace, softly-lifted
cards, one accent (pine) + one sparing highlight (gold).
**Don't:** cold slate/black, pure `#fff`, neon, heavy gradients, `backdrop-filter:
blur()` on sticky/fixed elements (see §9), clipped/border-radiused coin logos,
a tiny generic line-icon on every card.

---

## 2. Color tokens

Light is the site default; both themes are first-class. Every text/background
pair below passes **WCAG AA (4.5:1)**; large/graphical elements pass 3:1.
Gold is intentionally **different per theme** — a deep ochre in light (so it has
contrast on paper) and a brighter gold in dark.

| Token | Light (default) | Dark (warm charcoal) | Use |
| --- | --- | --- | --- |
| `--bg` | `#f7f2e8` | `#17130d` | Page background (warm bone / warm near-black) |
| `--surface` | `#efe7d6` | `#1f1a13` | Alternating section bands |
| `--surface-raised` | `#fffdf7` | `#262019` | Cards, raised panels, code blocks |
| `--border` | `#e4dac4` | `#3a3227` | Hairlines, card borders |
| `--text` | `#2c2721` | `#f1e9da` | Body text (warm espresso / warm off-white) |
| `--text-secondary` | `#6a6153` | `#b3a890` | Captions, metadata, muted copy |
| `--accent` | `#1f5a44` | `#7bbf9f` | Links, highlights (pine / sage) |
| `--accent-strong` | `#1f5a44` | `#2c7d5c` | Primary button background |
| `--accent-contrast` | `#ffffff` | `#ffffff` | Text/icon on the accent |
| `--gold` | `#8f6410` | `#d9b25e` | Sparing accent: numerals, small rules |
| `--gold-soft` | `#ddbb6a` | `#e8ca86` | Decorative fills only (never small text) |
| `--focus` | `#1f5a44` | `#7bbf9f` | Focus ring |
| `--shadow` | `0 18px 44px rgb(60 45 20 / .12)` | `0 20px 55px rgb(0 0 0 / .5)` | Big elevation (framed art, modals) |

Rules:

- **One accent family (pine) + one sparing highlight (gold).** No status
  rainbow on marketing surfaces; add success/warning/danger only where the app
  genuinely needs them, tuned warm.
- **Gold is for accents, not text.** In light it's dark enough for large/bold
  use (headline numerals, 2px rules); don't set small body text in gold.
- Shadows are **warm-tinted** (brown in light), never neutral gray.

### Paste-able token block

```css
:root {                     /* warm light — default */
  --bg:#f7f2e8; --surface:#efe7d6; --surface-raised:#fffdf7; --border:#e4dac4;
  --text:#2c2721; --text-secondary:#6a6153;
  --accent:#1f5a44; --accent-strong:#1f5a44; --accent-contrast:#ffffff;
  --gold:#8f6410; --gold-soft:#ddbb6a; --focus:#1f5a44;
  --shadow:0 18px 44px rgb(60 45 20 / .12);
}
@media (prefers-color-scheme: dark) {
  :root {                   /* warm charcoal — dark */
    --bg:#17130d; --surface:#1f1a13; --surface-raised:#262019; --border:#3a3227;
    --text:#f1e9da; --text-secondary:#b3a890;
    --accent:#7bbf9f; --accent-strong:#2c7d5c; --accent-contrast:#ffffff;
    --gold:#d9b25e; --gold-soft:#e8ca86; --focus:#7bbf9f;
    --shadow:0 20px 55px rgb(0 0 0 / .5);
  }
}
```

---

## 3. Typography

Three self-hosted, Latin-subset, **variable** woff2 fonts, all SIL OFL:

| Role | Face | Where used | Notes |
| --- | --- | --- | --- |
| Display | **Fraunces** | `h1`, `h2`, big numerals | Warm old-style serif; weight 600; the "human" signal |
| UI / body | **Inter** | everything functional, `h3`+ | 400–600 |
| Mono | **Inconsolata** | code, checksums, hashes | also good for tabular figures |

```css
--font-display: 'Fraunces', Georgia, 'Times New Roman', 'Times', serif;
--font-sans:    'Inter', system-ui, -apple-system, 'Segoe UI', Roboto, Helvetica, Arial, sans-serif;
--font-mono:    'Inconsolata', ui-monospace, 'Cascadia Code', 'SF Mono', Menlo, Consolas, monospace;
```

**Fluid type scale** (clamp keeps it responsive without breakpoints):

```css
--step-display: clamp(2.5rem, 6vw, 4.25rem);  /* hero h1 */
--step-h2:      clamp(1.75rem, 3.5vw, 2.5rem); /* section titles */
--step-h3:      1.25rem;                        /* card titles */
--step-body:    1.0625rem;                      /* body */
--step-small:   0.875rem;                       /* captions */
```

Details that matter:

- Body `line-height: 1.65`; headings `1.15`, tighter for display (`h1` 1.05,
  `h2` 1.1). Display headings: `letter-spacing: -0.015em`.
- `h1, h2` use `--font-display` (serif); `h3` and smaller stay Inter.
- `text-wrap: balance` on headings (kills orphan lines); `text-wrap: pretty` on
  paragraphs.
- Long-form prose capped at `--prose: 72ch`. Intro/lead paragraphs `1.1875rem`
  in `--text-secondary`.
- `font-synthesis: none; -webkit-font-smoothing: antialiased;`

---

## 4. Spacing, layout, shape

```css
/* 4px spacing scale */
--s-1:.25rem; --s-2:.5rem; --s-3:.75rem; --s-4:1rem;
--s-6:1.5rem; --s-8:2rem;  --s-12:3rem;  --s-16:4rem;
--section-pad: clamp(4rem, 10vw, 7rem);  /* vertical breathing room per section */

--maxw:  1120px;   /* content max width */
--prose: 72ch;     /* long-form measure */

--radius-control: 6px;   /* buttons, inputs, chips */
--radius-card:    16px;  /* cards, panels, framed media */
```

- **Whitespace is the primary "premium" signal** — be generous with
  `--section-pad` and card padding (`--s-8`).
- Softer radii (6/16px) read friendlier than sharp corners. (The wallet's
  historic 4px is fine to keep; 6/16 is the warmer choice.)
- **Coin / asset logos render as shipped:** `object-fit: contain`, never
  clipped, masked, or `border-radius`'d.

---

## 5. Elevation

Two levels, both warm-tinted:

```css
/* cards: barely-there lift */
box-shadow: 0 1px 2px rgb(60 45 20 / .04), 0 12px 28px rgb(60 45 20 / .05);
/* framed media / modals: the big one */
box-shadow: var(--shadow);
```

Cards sit on `--surface-raised` with a `1px solid --border`. In dark theme the
lift comes from the raised surface color more than the shadow.

---

## 6. Components

### Buttons

```css
.btn { display:inline-flex; align-items:center; gap:.5rem; font-weight:600;
  line-height:1; padding:.75rem 1.25rem; border:1px solid transparent;
  border-radius:var(--radius-control); cursor:pointer;
  transition: background 150ms ease-out, border-color 150ms ease-out, transform 150ms ease-out; }
.btn:active { transform: translateY(1px); }

.btn--accent { background:var(--accent-strong); color:var(--accent-contrast); }
/* hover DARKENS and keeps white text — readable in both themes (don't just
   swap to --accent, which fails contrast) */
.btn--accent:hover { background: color-mix(in srgb, var(--accent-strong) 86%, #000); }

.btn--ghost  { background:transparent; color:var(--text); border-color:var(--border); }
.btn--ghost:hover { border-color:var(--accent); color:var(--accent); }

.btn--sm { padding:.5rem .85rem; font-size:var(--step-small); }
```

### Cards

```css
.card { background:var(--surface-raised); border:1px solid var(--border);
  border-radius:var(--radius-card); padding:var(--s-8);
  box-shadow:0 1px 2px rgb(60 45 20 / .04), 0 12px 28px rgb(60 45 20 / .05); }
.card h3 { margin-bottom:var(--s-3); }
.card p  { color:var(--text-secondary); }
```

### Editorial block (use instead of cards for emphasis)

When a section is the emotional core, drop the box and go editorial: a **gold
serif numeral**, a hairline, a serif heading. This is the single biggest move
away from "template" feel — don't box everything.

```css
.promise { border-top:2px solid var(--gold); padding-top:var(--s-6); }
.promise__num { font-family:var(--font-display); font-size:1.5rem; font-weight:600; color:var(--gold); }
.promise h3 { font-family:var(--font-display); font-size:clamp(1.4rem,2.4vw,1.75rem); margin:var(--s-3) 0 var(--s-4); }
.promise p { color:var(--text-secondary); }
```

### Section head + banding

```css
.section { padding-block: var(--section-pad); }
.section--alt { background: var(--surface); }   /* alternate every other section */
.section__head { max-width:60ch; margin-bottom:var(--s-8); }
.section__head p { color:var(--text-secondary); margin-top:var(--s-3); }
```

Alternate `--bg` and `--surface` section-to-section for a gentle rhythm; never
put two same-background sections adjacent.

### Badge (status pill), Chip (logo + label)

```css
.badge { display:inline-block; font-size:.75rem; font-weight:600; line-height:1;
  padding:.3em .6em; border-radius:999px; color:var(--accent); border:1px solid var(--accent); }
.chip { display:inline-flex; align-items:center; gap:.5rem; font-size:var(--step-small);
  padding:.35rem .7rem; border:1px solid var(--border); border-radius:999px; color:var(--text-secondary); }
.chip img { width:18px; height:18px; object-fit:contain; }
```

### Disclosure (native `<details>`)

No JS. Hide the default marker; use a `+ / −` or chevron in `::after`; hairline
separators for lists.

```css
summary { cursor:pointer; list-style:none; font-weight:600; }
summary::-webkit-details-marker { display:none; }
summary::after { content:"+"; color:var(--accent); }
details[open] summary::after { content:"\2212"; } /* minus */
```

### Links & focus

```css
a { color:var(--accent); text-underline-offset:2px; }
a:hover { text-decoration:underline; }
:focus-visible { outline:2px solid var(--focus); outline-offset:2px; border-radius:2px; }
```

---

## 7. Iconography & illustration

- Small inline **stroke SVG** icons, `stroke-width` ~1.8, `currentColor`. Keep
  them sparse — not one on every element.
- Brand/hero art is **flat, layered SVG shapes** (e.g. a golden-hour scene:
  warm sky gradient, a `--gold` sun disc, overlapping pine hills from light
  sage to deep pine, a few dots/birds). No stock renders, no fake UI.
- Coin logos: real, as shipped, never clipped (see §4).

---

## 8. Motion

- Transitions **150–200ms ease-out**, `transform` / `opacity` only.
- At most one entrance pattern; static is acceptable.
- Honor `prefers-reduced-motion: reduce` (disable non-essential motion, set
  `scroll-behavior:auto`).
- No parallax, no autoplay, no scroll-jacking.

---

## 9. Performance & the one hard "never"

- **Never** put `backdrop-filter: blur()` on a `position: sticky`/`fixed`
  element. It re-rasterizes the backdrop every scroll frame and pegs the CPU.
  A sticky header uses a **solid** `background: var(--bg)`.
- Self-host fonts (no third-party requests). Preload the display + body faces;
  `font-display: swap`.
- Prefer a single compiled stylesheet; keep it lean.

---

## 10. Accessibility (non-negotiable)

- Every text/background token pair passes **AA 4.5:1** in both themes (validate
  with a script, not by eye); large/graphical ≥ 3:1.
- Visible `:focus-visible` ring on all interactives; hit targets ≥ 24×24px.
- Semantic landmarks, ordered headings, one `h1` per page, skip link.
- Real `alt` text for informative images; `aria-hidden` decorative SVG.

---

## 11. Applying to the wallet

The wallet already ships Inter + Inconsolata; **add Fraunces** (variable, Latin
subset, OFL — from Fontsource `@fontsource-variable/fraunces`, the
`*-wght-normal` file, ~37 KB) for display headings. Fonts to reuse: Inter ~48 KB,
Inconsolata ~34 KB.

Map the app's existing SCSS variables to the **dark (warm charcoal)** palette:

| App variable (current) | Set to (warm dark) |
| --- | --- |
| `--body-bg: #30343d` | `#17130d` (page) — panels/cards `#1f1a13` / `#262019` |
| `--border-color: #383f4b` | `#3a3227` |
| `--text-color: #dfe2e1` | `#f1e9da`; secondary `#b3a890` |
| accent blues `#1d50b5 / #4271ce / #6fa3d8` | buttons `#2c7d5c`, links/highlights `#7bbf9f` |
| (new) gold accent | `#d9b25e` — sparing: numerals, small rules |
| control radius `4px` | keep `4px`, or soften to `6px` for the warmer feel |

Then adopt the type rules (§3): Fraunces for the largest headings, Inter for UI,
`text-wrap: balance` on headings. Keep the app's functional density — this is a
**re-skin of color, type, and elevation**, not a layout change. Re-validate
contrast after (the app has more UI states than the marketing site; tune any
success/warning/danger colors to sit warm rather than neon).

---

## 12. Reference

- Live implementation: the caribou.you site (`index.html` + `css/style.css`).
- Fonts + license: `font/` and `font/OFL.txt`.
- This palette and every component above are already built and contrast-checked
  in that stylesheet — lift values from there if anything here is ambiguous.
