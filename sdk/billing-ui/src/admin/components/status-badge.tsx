import { Badge } from "#orck/admin/components/ui/badge"
import { useAdminUI } from "#orck/admin/ui-context"

const settled = "bg-settled-surface text-settled"
const held = "bg-held-surface text-held"
const failed = "bg-failed-surface text-failed"
const refunded = "bg-refunded-surface text-refunded"
const inactive = "bg-inactive-surface text-inactive"

const tones: Record<string, string> = {
  active: settled,
  succeeded: settled,
  completed: settled,
  complete: settled,
  linked: settled,
  approved: settled,
  landed: settled,
  collected: settled,
  current: settled,
  held,
  past_due: held,
  grace: held,
  pending: held,
  unknown: held,
  open: held,
  requires_review: held,
  delinquent: failed,
  failed,
  declined: failed,
  lost: failed,
  refunded,
  partially_refunded: refunded,
  dismissed: inactive,
  superseded: inactive,
  already_resolved: inactive,
  canceled: inactive,
}

// statusLabel is a wire status in plain words: partially_refunded reads
// "Partially refunded".
export function statusLabel(status: string): string {
  const words = status.replaceAll("_", " ")
  return words.charAt(0).toUpperCase() + words.slice(1)
}

export function StatusBadge({ status }: { status?: string }) {
  const { text } = useAdminUI()
  if (!status) return <span className="text-muted-foreground">—</span>
  return (
    <Badge variant="secondary" className={tones[status] ?? inactive}>
      {text(statusLabel(status))}
    </Badge>
  )
}
