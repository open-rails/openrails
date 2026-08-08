// Money display. All engine amounts are micros (millionths of a currency
// unit); rendering is the only place they become decimals.
export function formatMoney(micros: number, currency: string): string {
  const value = micros / 1_000_000
  try {
    return new Intl.NumberFormat(undefined, {
      style: "currency",
      currency: currency.toUpperCase(),
    }).format(value)
  } catch {
    return `${value} ${currency.toUpperCase()}`
  }
}

export function formatPeriod(hours?: number | null): string {
  if (!hours) return ""
  const days = Math.round(hours / 24)
  if (days >= 28 && days <= 31) return "/ month"
  if (days >= 360 && days <= 366) return "/ year"
  if (days >= 6 && days <= 8) return "/ week"
  return `/ ${days} days`
}

export function periodNoun(hours?: number | null): string {
  if (!hours) return ""
  const days = Math.round(hours / 24)
  if (days >= 28 && days <= 31) return "monthly"
  if (days >= 360 && days <= 366) return "yearly"
  if (days >= 6 && days <= 8) return "weekly"
  return `every ${days} days`
}
