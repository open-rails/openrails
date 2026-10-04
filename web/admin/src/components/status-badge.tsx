import { Badge } from "@/components/ui/badge"

const tones: Record<string, string> = {
  active: "bg-settled-surface text-settled",
  succeeded: "bg-settled-surface text-settled",
  completed: "bg-settled-surface text-settled",
  linked: "bg-settled-surface text-settled",
  approved: "bg-settled-surface text-settled",
  collected: "bg-settled-surface text-settled",
  past_due: "bg-held-surface text-held",
  pending: "bg-held-surface text-held",
  unknown: "bg-held-surface text-held",
  open: "bg-held-surface text-held",
  canceled: "bg-muted text-muted-foreground",
  failed: "bg-failed-surface text-failed",
  declined: "bg-failed-surface text-failed",
  lost: "bg-failed-surface text-failed",
  refunded: "bg-refunded-surface text-refunded",
  partially_refunded: "bg-refunded-surface text-refunded",
}

export function StatusBadge({ status }: { status?: string }) {
  if (!status) return <span className="text-muted-foreground">—</span>
  return (
    <Badge variant="secondary" className={tones[status] ?? ""}>
      {status}
    </Badge>
  )
}
