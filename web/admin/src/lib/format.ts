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

// timeAgo renders a compact relative time ("3m", "2h", "5d") for feeds like the
// notification bell; falls back to a short date past a week.
export function timeAgo(iso?: string | null): string {
  if (!iso) return ""
  const then = new Date(iso).getTime()
  if (Number.isNaN(then)) return ""
  const secs = Math.round((Date.now() - then) / 1000)
  if (secs < 45) return "just now"
  if (secs < 90) return "1m"
  const mins = Math.round(secs / 60)
  if (mins < 60) return `${mins}m`
  const hours = Math.round(mins / 60)
  if (hours < 24) return `${hours}h`
  const days = Math.round(hours / 24)
  if (days < 7) return `${days}d`
  return new Date(then).toLocaleDateString(undefined, { month: "short", day: "numeric" })
}
