import { Fragment, ReactNode } from 'react'

// renderInline turns **bold** and `code` spans into elements. The legal text
// uses nothing else inline, so URLs are left as plain text.
function renderInline (text: string): ReactNode[] {
  return text.split(/(\*\*[^*]+\*\*|`[^`]+`)/).map((part, i) => {
    if (part.startsWith('**') && part.endsWith('**')) return <strong key={i}>{part.slice(2, -2)}</strong>
    if (part.startsWith('`') && part.endsWith('`')) return <code key={i}>{part.slice(1, -1)}</code>
    return <Fragment key={i}>{part}</Fragment>
  })
}

interface LegalTextProps { markdown: string }

// LegalText renders the small markdown subset used by the embedded Terms of
// Use and Privacy Policy: #/## headings, - bullets, and paragraphs. The
// markdown cannot carry utility classes per element, so each bundle styles the
// elements under .legal-text.
export default function LegalText ({ markdown }: LegalTextProps) {
  const blocks: ReactNode[] = []
  let bullets: string[] = []
  const flushBullets = () => {
    if (bullets.length === 0) return
    const items = bullets
    bullets = []
    blocks.push(<ul key={blocks.length}>{items.map((b, i) => <li key={i}>{renderInline(b)}</li>)}</ul>)
  }
  for (const line of markdown.split('\n')) {
    if (line.startsWith('- ')) {
      bullets.push(line.slice(2))
      continue
    }
    flushBullets()
    if (line.trim() === '') continue
    if (line.startsWith('## ')) blocks.push(<h4 key={blocks.length}>{line.slice(3)}</h4>)
    else if (line.startsWith('# ')) blocks.push(<h3 key={blocks.length}>{line.slice(2)}</h3>)
    else blocks.push(<p key={blocks.length}>{renderInline(line)}</p>)
  }
  flushBullets()
  return <div className="legal-text">{blocks}</div>
}
