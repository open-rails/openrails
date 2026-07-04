// All API money amounts are micros (millionths of a currency unit).
const MICROS = 1_000_000

const isoCurrency = /^[a-zA-Z]{3}$/

export function formatMicros(amount: number, currency: string): string {
  const value = amount / MICROS
  if (isoCurrency.test(currency)) {
    try {
      return new Intl.NumberFormat(undefined, {
        style: "currency",
        currency: currency.toUpperCase(),
        maximumFractionDigits: 6,
      }).format(value)
    } catch {
      // non-ISO code fell through Intl — fall back below
    }
  }
  // Custom credit currencies: plain number + code.
  return `${value.toLocaleString(undefined, { maximumFractionDigits: 6 })} ${currency}`
}

export function microsFromInput(major: string): number | null {
  const n = Number(major)
  if (!Number.isFinite(n)) return null
  return Math.round(n * MICROS)
}

export function formatDate(iso?: string | null): string {
  if (!iso) return "—"
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return "—"
  return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" })
}

export function formatUnix(seconds?: number): string {
  if (!seconds) return "—"
  return new Date(seconds * 1000).toLocaleString(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  })
}

export function shortId(id: string, n = 8): string {
  return id.length > n ? `${id.slice(0, n)}…` : id
}
