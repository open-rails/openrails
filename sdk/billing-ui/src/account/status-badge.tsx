import { cn } from "cn"

import { Badge } from "#orck/components/ui/badge"
import { useMessages } from "#orck/i18n/context"
import type { MessageKey } from "#orck/i18n/messages"
import { statusTone, type StatusTone } from "./format"

const TONE_CLASS: Record<StatusTone, string> = {
  success: "bg-[color:var(--orck-success)]/12 text-[color:var(--orck-success)]",
  warning: "bg-[color:var(--orck-warning)]/14 text-[color:var(--orck-warning)]",
  destructive: "bg-destructive/10 text-destructive",
  neutral: "bg-muted text-muted-foreground",
}

export interface BillingStatusBadgeProps {
  /** Subscription, payment or card-health status. */
  status: string
  /** Overrides the translated label. */
  label?: string
  className?: string
}

export function BillingStatusBadge({
  status,
  label,
  className,
}: BillingStatusBadgeProps) {
  const { t, messages } = useMessages()
  const key = status === "canceled" ? "cancelled" : status
  const text =
    label ??
    (key in messages.status
      ? t(`status.${key}` as MessageKey)
      : status.replace(/_/g, " "))
  return (
    <Badge
      data-status={status}
      className={cn(TONE_CLASS[statusTone(status)], "font-medium", className)}
    >
      {text}
    </Badge>
  )
}
