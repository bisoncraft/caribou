interface SendProps {
  // Optional preselected ticker (e.g. when launched from a portfolio row's
  // Send icon). Reserved for the eventual Send implementation; currently
  // ignored by the placeholder UI.
  ticker?: string
}

export default function Send (_props: SendProps) {
  return (
    <form className="modal">
      Send
    </form>
  )
}
