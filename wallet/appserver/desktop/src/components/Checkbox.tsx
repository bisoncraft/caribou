import { ChangeEvent } from 'react'

interface CheckboxProps {
  checked: boolean
  onChange?: (checked: boolean) => void
  disabled?: boolean
  className?: string
  ariaLabel?: string
}

// Checkbox is a styled control that wraps a native <input type="checkbox">.
// The native input is visually hidden but receives focus and screen-reader
// semantics; the visible state lives on the sibling .checkbox-pip. Pair with
// the .checkbox-row class on a wrapping <label> to make an entire row act as
// the click target. Styling lives in components.scss under .checkbox-*.
export default function Checkbox ({ checked, onChange, disabled, className, ariaLabel }: CheckboxProps) {
  const handle = (e: ChangeEvent<HTMLInputElement>) => onChange?.(e.target.checked)
  return (
    <span className={`checkbox-wrap ${className ?? ''}`}>
      <input
        type="checkbox"
        className="checkbox-input"
        checked={checked}
        onChange={handle}
        disabled={disabled}
        aria-label={ariaLabel}
      />
      <span className="checkbox-pip" aria-hidden>
        <svg viewBox="0 0 24 24" className="checkbox-check">
          <polyline points="20 6 9 17 4 12" />
        </svg>
      </span>
    </span>
  )
}
