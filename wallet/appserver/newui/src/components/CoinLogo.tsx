import { useState, useEffect, ImgHTMLAttributes } from 'react'

// Letters that exist as fallback files in /img/coins/.
const fallbackLetters = new Set('abcdefghijklmnopqrstuvwxyz'.split(''))

function pathFor (ticker: string): string {
  return `/img/coins/${ticker.toLowerCase()}.png`
}

function letterPath (ticker: string): string | null {
  const c = ticker.charAt(0).toLowerCase()
  return fallbackLetters.has(c) ? `/img/coins/${c}.png` : null
}

interface CoinLogoProps extends Omit<ImgHTMLAttributes<HTMLImageElement>, 'src'> {
  ticker: string
}

// CoinLogo renders a coin logo by ticker, with a single-letter fallback if the
// ticker-specific PNG is missing. Pass any standard <img> props (className,
// alt, title, ...) through.
export default function CoinLogo ({ ticker, alt, ...rest }: CoinLogoProps) {
  const [src, setSrc] = useState(pathFor(ticker))
  const [triedLetter, setTriedLetter] = useState(false)

  useEffect(() => {
    setSrc(pathFor(ticker))
    setTriedLetter(false)
  }, [ticker])

  const onError = () => {
    if (triedLetter) return
    const lp = letterPath(ticker)
    if (lp && lp !== src) setSrc(lp)
    setTriedLetter(true)
  }

  return <img src={src} alt={alt ?? ticker} onError={onError} {...rest} />
}
