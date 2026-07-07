// Pure logic for the #777 price-change wizard — kept isolated from React so
// it's trivially unit-testable (web/admin has no test runner today; this
// module is where that debt would be paid down first).
import { formatMicros } from "@/lib/format"

export type PriceDirection = "increase" | "decrease" | "unchanged"

export function detectDirection(newAmount: number, currentAmount: number): PriceDirection {
  if (newAmount > currentAmount) return "increase"
  if (newAmount < currentAmount) return "decrease"
  return "unchanged"
}

export type MigrationMode = "grandfather" | "migrate"

// #773 shipped NO merchant-configurable notice window and NO server-side
// enforcement of one — this is a console-only UX guardrail on the date
// picker for increase+migrate. See the #777 tracker note and #773's Progress
// section: the engine only emits the reprice_scheduled disclosure event: it
// never rejects an early effective_at.
export const NOTICE_WINDOW_DAYS = 30

// defaultMigrationMode is the direction-aware Step 2 default: increases
// default to grandfather (zero-risk — #774's existing behavior, no new
// action); decreases default to migrate-now (you never grandfather a
// decrease — #773's design ruling).
export function defaultMigrationMode(direction: PriceDirection): MigrationMode {
  return direction === "decrease" ? "migrate" : "grandfather"
}

// minEffectiveDate returns the earliest allowed migration date, or null when
// there is no minimum (decreases may move everyone at next renewal starting
// now — notice is optional goodwill, not a requirement).
export function minEffectiveDate(direction: PriceDirection, now: Date): Date | null {
  if (direction !== "increase") return null
  const min = new Date(now)
  min.setUTCDate(min.getUTCDate() + NOTICE_WINDOW_DAYS)
  return min
}

// defaultEffectiveDate is Step 2's pre-filled migration date: the notice
// window's floor for an increase, "now" for a decrease.
export function defaultEffectiveDate(direction: PriceDirection, now: Date): Date {
  return minEffectiveDate(direction, now) ?? now
}

// isEffectiveDateValid enforces the console-side notice-window gate: an
// increase+migrate plan may not pick a date inside the notice window.
export function isEffectiveDateValid(direction: PriceDirection, effectiveAt: Date, now: Date): boolean {
  const min = minEffectiveDate(direction, now)
  return !min || effectiveAt.getTime() >= min.getTime()
}

export interface MigrationPlan {
  mode: MigrationMode
  // ISO instant; only meaningful when mode === "migrate".
  effectiveAt: string
}

const dateLabel = (iso: string) =>
  new Date(iso).toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" })

// buildReviewText renders Step 3's "plan in words" exactly per the #777 spec
// phrasing: "New subscribers pay $12 immediately. 1,204 existing subscribers
// keep $10 until Sep 1, then move to $12 at their next renewal. Notices go
// out on confirm."
export function buildReviewText(params: {
  newAmount: number
  currentAmount: number
  currency: string
  affectedCount: number
  plan: MigrationPlan
  now: Date
}): string {
  const { newAmount, currentAmount, currency, affectedCount, plan, now } = params
  const newLabel = formatMicros(newAmount, currency)
  const oldLabel = formatMicros(currentAmount, currency)
  const lead = `New subscribers pay ${newLabel} immediately.`

  if (affectedCount === 0) {
    return `${lead} No existing subscribers are on a prior version of this price.`
  }
  const subj = `${affectedCount.toLocaleString()} existing subscriber${affectedCount === 1 ? "" : "s"}`

  if (plan.mode === "grandfather") {
    return `${lead} ${subj} keep ${oldLabel} forever (grandfathered).`
  }

  // The engine emits a reprice_scheduled notification per affected
  // subscriber at SCHEDULE time regardless of how far out effective_at is
  // (#773) — always say so, not just for the delayed-migration phrasing.
  const effective = new Date(plan.effectiveAt)
  const immediate = effective.getTime() <= now.getTime()
  if (immediate) {
    return `${lead} ${subj} move to ${newLabel} at their next renewal. Notices go out on confirm.`
  }
  return `${lead} ${subj} keep ${oldLabel} until ${dateLabel(plan.effectiveAt)}, then move to ${newLabel} at their next renewal. Notices go out on confirm.`
}
