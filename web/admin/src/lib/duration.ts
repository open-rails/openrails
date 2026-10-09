// Durations travel as exact hours or seconds; people read and type them in
// words. A value is shown in the largest whole unit that divides it.
const UNITS: [seconds: number, name: string][] = [
  [604_800, "week"],
  [86_400, "day"],
  [3_600, "hour"],
  [60, "minute"],
  [1, "second"],
]

function counted(count: number, name: string): string {
  return `${count} ${name}${count === 1 ? "" : "s"}`
}

// formatSeconds renders "1 week", "30 days", "36 hours", "90 minutes".
export function formatSeconds(seconds: number): string {
  if (!Number.isSafeInteger(seconds) || seconds <= 0) return counted(seconds, "second")
  const [size, name] = UNITS.find(([size]) => seconds % size === 0) ?? UNITS[4]
  return counted(seconds / size, name)
}

export function formatHours(hours: number): string {
  return formatSeconds(hours * 3_600)
}

const HOURS_PER_UNIT: Record<string, number> = { h: 1, d: 24, w: 168 }

// parseHours reads what the catalog YAML accepts — "30 days", "1 week",
// "12 hours", or the short "30d" — as whole hours; anything else is null.
export function parseHours(text: string): number | null {
  const match = /^\s*(\d+)\s*(h|hours?|d|days?|w|weeks?)\s*$/i.exec(text)
  if (!match) return null
  const hours = Number(match[1]) * HOURS_PER_UNIT[match[2][0].toLowerCase()]
  return Number.isSafeInteger(hours) && hours > 0 ? hours : null
}

// An allowance cap is spelled "720h" or "30d" on the wire.
export function capToHours(cap: string): number | null {
  const match = /^(\d+)([hd])$/.exec(cap.trim().toLowerCase())
  return match ? Number(match[1]) * HOURS_PER_UNIT[match[2]] : null
}

export function hoursToCap(hours: number): string {
  return hours % 24 === 0 ? `${hours / 24}d` : `${hours}h`
}

export function formatCap(cap: string): string {
  const hours = capToHours(cap)
  return hours === null ? cap : formatHours(hours)
}
