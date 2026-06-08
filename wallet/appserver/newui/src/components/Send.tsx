interface SendProps {
  // Optional preselected ticker (e.g. when launched from a portfolio row's
  // Send icon). Reserved for the eventual Send implementation; currently
  // the placeholder just echoes it so the wiring is visible during dev.
  ticker?: string
}

export default function Send (props: SendProps) {
  return (
    <form className="modal">
      {props.ticker ? `Send ${props.ticker}` : 'Send'}
    </form>
  )
}
