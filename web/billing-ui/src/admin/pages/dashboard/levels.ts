// A measure's threshold levels as the dashboard draws them: the levels in
// force at an instant (a network's program changes its levels on a date),
// and the one a value reached, by the measure's direction.
import type { MetricsThreshold } from "#orck/admin/lib/api/metrics"
import type { Direction } from "./registry"

// inForce are the levels in force at the instant at (milliseconds), lowest
// first: from it on, before until. A level's bounds are instants, so a
// bucket in any timezone compares them with its own instants.
export function inForce(
  levels: MetricsThreshold[],
  at: number
): MetricsThreshold[] {
  return levels
    .filter(
      (l) =>
        (!l.from || Date.parse(l.from) <= at) &&
        (!l.until || at < Date.parse(l.until))
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
