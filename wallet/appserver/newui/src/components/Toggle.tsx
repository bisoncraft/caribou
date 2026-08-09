import { ChangeEvent } from 'react'

interface ToggleProps {
  checked: boolean
  onChange?: (checked: boolean) => void
  disabled?: boolean
  ariaLabel?: string
}

// Toggle is an on/off switch wrapping a native <input type="checkbox"> with
// switch semantics. Like Checkbox, the native input is visually hidden but
// receives focus and screen-reader semantics; the visible state lives on the
// sibling .toggle-track. Styling lives in components.scss under .toggle-*.
export default function Toggle ({ checked, onChange, disabled, ariaLabel }: ToggleProps) {
  const handle = (e: ChangeEvent<HTMLInputElement>) => onChange?.(e.target.checked)
  return (
    <span className="toggle-wrap">
      <input
        type="checkbox"
        role="switch"
        className="toggle-input"
        checked={checked}
        onChange={handle}
        disabled={disabled}
        aria-label={ariaLabel}
      />
      <span className="toggle-track" aria-hidden>
        <span className="toggle-knob" />
      </span>
    </span>
  )
}
