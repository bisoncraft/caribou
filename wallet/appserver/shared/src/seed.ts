// Seed helpers shared by the desktop and mobile init wizards.

// normalizeSeed collapses runs of whitespace/newlines to single spaces and
// trims, so stray formatting (pasted line breaks, double spaces) does not break
// seed validation.
export function normalizeSeed (raw: string): string {
  return raw.trim().replace(/\s+/g, ' ')
}

// seedWordCount counts the whitespace-delimited words in a (possibly
// unnormalized) seed string.
export function seedWordCount (raw: string): number {
  const s = raw.trim()
  return s === '' ? 0 : s.split(/\s+/).length
}
