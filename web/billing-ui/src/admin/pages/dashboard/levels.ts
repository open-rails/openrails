// A measure's threshold levels as the dashboard draws them: the levels in
// force in a month (a network's program changes its levels on a date), and
// the one a value reached, by the measure's direction.
import type { MetricsThreshold } from "#orck/admin/lib/api/metrics"
import type { Direction } from "./registry"

// monthOf is the calendar month (YYYY-MM) of an instant in timeZone.
export function monthOf(iso: string, timeZone = "UTC"): string {
  const parts = new Intl.DateTimeFormat("en-CA", {
    timeZone,
    year: "numeric",
    month: "2-digit",
  }).formatToParts(new Date(iso))
  const part = (type: string) => parts.find((p) => p.type === type)?.value
  return `${part("year")}-${part("month")}`
}

// inForce are the levels in force in month (YYYY-MM), lowest first.
export function inForce(
  levels: MetricsThreshold[],
  month: string
): MetricsThreshold[] {
  return levels
    .filter(
      (l) =>
        (!l.from || l.from.slice(0, 7) <= month) &&
        (!l.until || month < l.until.slice(0, 7))
    )
    .sort((a, b) => a.value - b.value)
}

// worse is whether a value past level is worse: above it where down is
// good, below it where up is.
export function worse(
  value: number,
  level: number,
  direction: Direction
): boolean {
  return direction === "up_is_good" ? value <= level : value >= level
}

// reached is the severest of levels (lowest first) value has reached.
export function reached(
  levels: MetricsThreshold[],
  value: number,
  direction: Direction
): MetricsThreshold | undefined {
  if (!Number.isFinite(value)) return undefined
  const passed = levels.filter((l) => worse(value, l.value, direction))
  return direction === "up_is_good" ? passed[0] : passed.at(-1)
}

// severest is the level past which nothing is worse.
export function severest(
  levels: MetricsThreshold[],
  direction: Direction
): MetricsThreshold | undefined {
  return direction === "up_is_good" ? levels[0] : levels.at(-1)
}

// nearest is the first level a value meets on its way to worse.
export function nearest(
  levels: MetricsThreshold[],
  direction: Direction
): MetricsThreshold | undefined {
  return direction === "up_is_good" ? levels.at(-1) : levels[0]
}

// band is the span a level shades towards worse: up to the next level or
// the axis top where down is good, down to the next or zero where up is.
export function band(
  levels: MetricsThreshold[],
  i: number,
  direction: Direction,
  top: number
): [number, number] {
  return direction === "up_is_good"
    ? [levels[i - 1]?.value ?? 0, levels[i].value]
    : [levels[i].value, levels[i + 1]?.value ?? top]
}
