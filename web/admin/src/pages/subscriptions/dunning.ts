import type { SubscriptionDunning } from "@/lib/api/generated/wire"
import { formatDate } from "@/lib/format"

// dunningSummary is one line for a dunning case: the declines so far and
// what comes next.
export function dunningSummary(d: SubscriptionDunning): string {
  const declined = `${d.attempts} declined`
  if (d.waiting_for_new_card)
    return `${declined} · needs a new card by ${formatDate(d.final_retry_at)}`
  const left =
    d.retries_left === null ? "provider retries" : `${d.retries_left} left`
  const next = d.next_retry_at ? ` · next ${formatDate(d.next_retry_at)}` : ""
  return `${declined} · ${left}${next}`
}
