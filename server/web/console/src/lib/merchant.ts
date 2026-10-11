// Consume the merchant deep link before sign-in redirects can remove it.
export function takeMerchantFromHash(
  location: Location = window.location,
  history: History = window.history
): string | undefined {
  const params = new URLSearchParams(location.hash.slice(1))
  const merchant = params.get("merchant")?.trim()
  if (!params.has("merchant")) return undefined
  params.delete("merchant")
  const rest = params.toString()
  history.replaceState(
    history.state,
    "",
    `${location.pathname}${location.search}${rest ? `#${rest}` : ""}`
  )
  return merchant || undefined
}
