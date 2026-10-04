/** The https URL a redirect rail may navigate to, or null. */
export function safeRedirectURL(raw: string): string | null {
  try {
    const url = new URL(raw)
    if (url.protocol !== "https:" || url.username || url.password) return null
    return url.href
  } catch {
    return null
  }
}
