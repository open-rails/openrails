import { Badge } from "@/components/ui/badge"

const tones: Record<string, string> = {
  active: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400",
  succeeded: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400",
  completed: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400",
  linked: "bg-emerald-500/15 text-emerald-600 dark:text-emerald-400",
  past_due: "bg-amber-500/15 text-amber-600 dark:text-amber-400",
  pending: "bg-amber-500/15 text-amber-600 dark:text-amber-400",
  unknown: "bg-amber-500/15 text-amber-600 dark:text-amber-400",
  cancelled: "bg-muted text-muted-foreground",
  failed: "bg-red-500/15 text-red-600 dark:text-red-400",
  refunded: "bg-sky-500/15 text-sky-600 dark:text-sky-400",
  partially_refunded: "bg-sky-500/15 text-sky-600 dark:text-sky-400",
}

export function StatusBadge({ status }: { status?: string }) {
  if (!status) return <span className="text-muted-foreground">—</span>
  return (
    <Badge variant="secondary" className={tones[status] ?? ""}>
      {status}
    </Badge>
  )
}
