import type { AutoTopupStatus } from "@/lib/api/types"

export function AutoTopupSafetySummary({
  status,
}: {
  status?: AutoTopupStatus
}) {
  if (!status) return null
  return (
    <div className="mt-3 space-y-1 text-xs text-muted-foreground">
      <p>Auto-topup: {status.enabled ? "Enabled" : "Disabled"}</p>
      {status.pending && (
        <p>Payment outcome pending verification. New top-ups are paused.</p>
      )}
      <p>
        Charge episodes: {status.daily}/{status.policy.max_daily} in 24h ·{" "}
        {status.weekly}/{status.policy.max_weekly} in 7d · {status.monthly}/
        {status.policy.max_monthly} in 30d
      </p>
      <p>
        Consecutive declines: {status.consecutive_declines}/
        {status.policy.declines_before_disable}
      </p>
    </div>
  )
}
