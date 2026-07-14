import { UnitInfo } from './registry'

const languages = navigator.languages.filter((locale: string) => locale !== 'c')

const RateEncodingFactor = 1e8 // same as value defined in ./orderutil

const log10RateEncodingFactor = Math.round(Math.log10(RateEncodingFactor))

const intFormatter = new Intl.NumberFormat(languages, { maximumFractionDigits: 0 })

const fourSigFigs = new Intl.NumberFormat(languages, {
  minimumSignificantDigits: 4,
  maximumSignificantDigits: 4
})

const usdFormatter = new Intl.NumberFormat(languages, {
  style: 'currency',
  currency: 'USD',
  minimumFractionDigits: 2,
  maximumFractionDigits: 2
})

/* A cache for formatters used for Doc.formatCoinValue. */
const decimalFormatters: Record<number, Intl.NumberFormat> = {}

/*
 * decimalFormatter gets the formatCoinValue formatter for the specified decimal
 * precision.
 */
function decimalFormatter (prec: number) {
  return formatter(decimalFormatters, 2, prec)
}

/* A cache for formatters used for Doc.formatFullPrecision. */
const fullPrecisionFormatters: Record<number, Intl.NumberFormat> = {}

/*
 * fullPrecisionFormatter gets the formatFullPrecision formatter for the
 * specified decimal precision.
 */
function fullPrecisionFormatter (prec: number, locales?: string | string[]) {
  return formatter(fullPrecisionFormatters, prec, prec, locales)
}

/*
 * formatter gets the formatter from the supplied cache if it already exists,
 * else creates it.
 */
function formatter (formatters: Record<string, Intl.NumberFormat>, min: number, max: number, locales?: string | string[]): Intl.NumberFormat {
  const k = `${min}-${max}`
  let fmt = formatters[k]
  if (!fmt) {
    fmt = new Intl.NumberFormat(locales ?? languages, {
      minimumFractionDigits: min,
      maximumFractionDigits: max
    })
    formatters[k] = fmt
  }
  return fmt
}

/*
 * convertToConventional converts the value in atomic units to conventional
 * units.
 */
function convertToConventional (v: number, unitInfo: UnitInfo) {
  const f = unitInfo.conventional.conversionFactor
  v /= f
  const prec = Math.round(Math.log10(f))
  return [v, prec]
}

export default class Doc {
  /* bind binds the function to the event for the element. */
  static bind (el: EventTarget, ev: string | string[], f: EventListenerOrEventListenerObject, opts?: any /* EventListenerOptions */): void {
    for (const e of (Array.isArray(ev) ? ev : [ev])) el.addEventListener(e, f, opts)
  }

  /* unbind removes the handler for the event from the element. */
  static unbind (el: EventTarget, ev: string, f: (e: Event) => void): void {
    el.removeEventListener(ev, f)
  }

  /*
   * formatCoinValue formats the value in atomic units into a string
   * representation in conventional units. If the value happens to be an
   * integer, no decimals are displayed. Trailing zeros may be truncated.
   */
  static formatCoinValue (vAtomic: number, unitInfo?: UnitInfo): string {
    const [v, prec] = convertToConventional(vAtomic, unitInfo)
    if (Number.isInteger(v)) return intFormatter.format(v)
    return decimalFormatter(prec).format(v)
  }

  static conventionalCoinValue (vAtomic: number, unitInfo?: UnitInfo): number {
    const [v] = convertToConventional(vAtomic, unitInfo)
    return v
  }

  /*
   * formatRateFullPrecision formats rate to represent it exactly at rate step
   * precision, trimming non-effectual zeros if there are any.
   */
  static formatRateFullPrecision (encRate: number, bui: UnitInfo, qui: UnitInfo, rateStepEnc: number) {
    const r = bui.conventional.conversionFactor / qui.conventional.conversionFactor
    const convRate = encRate * r / RateEncodingFactor
    const rateStepDigits = log10RateEncodingFactor - Math.floor(Math.log10(rateStepEnc)) -
      Math.floor(Math.log10(bui.conventional.conversionFactor) - Math.log10(qui.conventional.conversionFactor))
    if (rateStepDigits <= 0) return intFormatter.format(convRate)
    return fullPrecisionFormatter(rateStepDigits).format(convRate)
  }

  static formatFourSigFigs (n: number, maxDecimals?: number): string {
    return formatSigFigsWithFormatters(intFormatter, fourSigFigs, n, maxDecimals)
  }

  /* formatUSD formats a dollar amount as a USD currency string. */
  static formatUSD (usd: number): string {
    return usdFormatter.format(usd)
  }

  /*
   * usdString converts an atomic amount to a USD currency string via the
   * asset's conventional conversion factor and fiat rate, or '' when the rate
   * is unknown.
   */
  static usdString (atomic: number, cFactor: number, rate: number): string {
    if (!rate) return ''
    return usdFormatter.format((atomic / cFactor) * rate)
  }

  /*
   * timeSince returns a coarse, human-readable duration since the given
   * timestamp in milliseconds, e.g. "3 h 20 m" or "5 d 2 h".
   */
  static timeSince (ms: number): string {
    return Doc.formatDuration(new Date().getTime() - ms)
  }

  /*
   * formatDuration returns a string representation of a duration in ms. A
   * non-positive duration (e.g. a block timestamp slightly ahead of the local
   * clock) renders as "0 s"; without the clamp, Math.floor on a negative
   * quotient would cascade a huge positive remainder into the smaller units.
   */
  static formatDuration (dur: number): string {
    let seconds = Math.floor(Math.max(dur, 0))
    let result = ''
    let count = 0
    const add = (n: number, s: string) => {
      if (n > 0 || count > 0) count++
      if (n > 0) result += `${n} ${s} `
      return count >= 2
    }
    let y, mo, d, h, m, s
    [y, seconds] = timeMod(seconds, aYear)
    if (add(y, 'y')) { return result.trim() }
    [mo, seconds] = timeMod(seconds, aMonth)
    if (add(mo, 'mo')) { return result.trim() }
    [d, seconds] = timeMod(seconds, aDay)
    if (add(d, 'd')) { return result.trim() }
    [h, seconds] = timeMod(seconds, anHour)
    if (add(h, 'h')) { return result.trim() }
    [m, seconds] = timeMod(seconds, aMinute)
    if (add(m, 'm')) { return result.trim() }
    [s, seconds] = timeMod(seconds, 1000)
    add(s, 's')
    return result.trim() || '0 s'
  }
}

const aYear = 31536000000
const aMonth = 2592000000
const aDay = 86400000
const anHour = 3600000
const aMinute = 60000

/* timeMod returns the quotient and remainder of t / dur. */
function timeMod (t: number, dur: number): [number, number] {
  const n = Math.floor(t / dur)
  return [n, t - n * dur]
}

function formatSigFigsWithFormatters (intFormatter: Intl.NumberFormat, sigFigFormatter: Intl.NumberFormat, n: number, maxDecimals?: number, locales?: string | string[]): string {
  if (n >= 1000) return intFormatter.format(n)
  const s = sigFigFormatter.format(n)
  if (typeof maxDecimals !== 'number') return s
  const fractional = sigFigFormatter.formatToParts(n).filter((part: Intl.NumberFormatPart) => part.type === 'fraction')[0]?.value ?? ''
  if (fractional.length <= maxDecimals) return s
  return fullPrecisionFormatter(maxDecimals, locales).format(n)
}
