interface SpinnerProps {
  className?: string
  size?: number // px — must have a matching .spinner-N class (see animations.scss)
}

// Spinner is a small CSS spinner suitable for buttons and inputs. Size maps to a
// .spinner-N modifier class; styling lives in animations.scss under
// .spinner-circle.
export default function Spinner ({ className, size = 14 }: SpinnerProps) {
  return <span className={`spinner-circle spinner-${size} ${className ?? ''}`} role="status" aria-label="loading" />
}
