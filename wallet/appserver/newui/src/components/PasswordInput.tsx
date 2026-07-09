import { useState, ChangeEvent } from 'react'
import t from '@caribou/shared/intl'

interface PasswordInputParams {
  onChange: (e: ChangeEvent<HTMLInputElement>) => void
  value?: string
  placeholder?: string
  disabled?: boolean
  autoFocus?: boolean
}

export default function PasswordInput ({ onChange, placeholder, value, disabled, autoFocus }: PasswordInputParams) {
  const [showPW, setShowPW] = useState(false)

  const showPWClicked = () => {
    if (disabled) return
    setShowPW(!showPW)
  }

  return (
    <div className={`d-flex align-items-center form-input-bg br5 ${disabled ? 'is-disabled' : ''}`}>
      <span className="ico-key fs16 p-2"></span>
      <input
        type={showPW ? 'text' : 'password'}
        onChange={onChange}
        placeholder={placeholder || t('password')}
        value={value}
        disabled={disabled}
        autoFocus={autoFocus}
      ></input>
      <div
        className={`p-2 ${disabled ? '' : 'hoverbg pointer'} ${showPW ? 'ico-eyeopen' : 'ico-eyeclosed'}`}
        onClick={showPWClicked}
      ></div>
    </div>
  )
}
