interface SpinnerProps {
  className?: string
  size?: number // px
}

// Spinner is a small inline CSS spinner suitable for buttons and inputs.
// Styling lives in main.scss under .spinner-circle.
export default function Spinner ({ className, size = 14 }: SpinnerProps) {
  const style: React.CSSProperties = { width: size, height: size, borderWidth: Math.max(2, Math.round(size / 7)) }
  return <span className={`spinner-circle ${className ?? ''}`} style={style} role="status" aria-label="loading" />
}
