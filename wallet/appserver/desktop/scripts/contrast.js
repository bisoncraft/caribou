// Validates WCAG AA contrast for the theme palettes in src/scss/vars.scss.
//
// Every text token must clear 4.5:1 against every surface token in its
// theme, and --accent-contrast must clear 4.5:1 on the --accent-strong fill.
// Decorative tokens (border-color, gold-soft, scrim-bg, shadow) are exempt.
//
// The parser expects the literal layout of the $-themes map: one
// `"theme-name": (` block per theme, one `"token": #hex,` entry per line.
// Non-hex values (rgba(), quoted shadow strings) are ignored.
//
// Usage: npm run contrast   (exits non-zero on any failing pair)

const fs = require('fs')
const path = require('path')

const SURFACE_TOKENS = ['body-bg', 'surface', 'surface-raised', 'form-input-bg']
const TEXT_TOKENS = [
  'text-color', 'text-grey', 'text-error', 'text-danger', 'text-warning',
  'accent', 'accent-hover', 'accent-green', 'gold',
]
const AA = 4.5

const src = fs.readFileSync(
  path.join(__dirname, '..', 'src', 'scss', 'vars.scss'), 'utf8')

const themes = {}
let current = null
for (const line of src.split('\n')) {
  const themeOpen = line.match(/^\s*"([a-z]+)": \(/)
  if (themeOpen) {
    current = {}
    themes[themeOpen[1]] = current
    continue
  }
  const token = line.match(/^\s*"([a-z-]+)": (#[0-9a-fA-F]{6}),/)
  if (token && current) current[token[1]] = token[2]
}
if (Object.keys(themes).length === 0) {
  console.error('no theme palettes parsed from vars.scss — layout changed?')
  process.exit(1)
}

function luminance(hex) {
  const [r, g, b] = [1, 3, 5].map((i) => {
    const c = parseInt(hex.slice(i, i + 2), 16) / 255
    return c <= 0.04045 ? c / 12.92 : ((c + 0.055) / 1.055) ** 2.4
  })
  return 0.2126 * r + 0.7152 * g + 0.0722 * b
}

function ratio(a, b) {
  const [hi, lo] = [luminance(a), luminance(b)].sort((x, y) => y - x)
  return (hi + 0.05) / (lo + 0.05)
}

let failures = 0
function check(theme, fgName, fg, bgName, bg) {
  const r = ratio(fg, bg)
  const ok = r >= AA
  if (!ok) failures++
  console.log(`  ${ok ? ' ok ' : 'FAIL'} ${theme}: ${fgName} on ${bgName} = ${r.toFixed(2)}`)
}

for (const [name, palette] of Object.entries(themes)) {
  console.log(`${name}:`)
  for (const tokens of [SURFACE_TOKENS, TEXT_TOKENS, ['accent-strong', 'accent-contrast']]) {
    for (const t of tokens) {
      if (!palette[t]) {
        console.error(`  FAIL ${name}: token ${t} missing or not a 6-digit hex literal`)
        failures++
      }
    }
  }
  for (const text of TEXT_TOKENS) {
    for (const surface of SURFACE_TOKENS) {
      if (palette[text] && palette[surface]) {
        check(name, text, palette[text], surface, palette[surface])
      }
    }
  }
  if (palette['accent-contrast'] && palette['accent-strong']) {
    check(name, 'accent-contrast', palette['accent-contrast'],
      'accent-strong', palette['accent-strong'])
  }
}

console.log(failures === 0
  ? 'all theme contrast pairs pass WCAG AA'
  : `${failures} contrast pair(s) below ${AA}:1`)
process.exit(failures === 0 ? 0 : 1)
