import * as React from "react"
import { cn } from "cn"

import type { CheckoutAppearance } from "#orck/appearance"
import type { BillingError } from "#orck/client/errors"
import { Button } from "#orck/components/ui/button"
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "#orck/components/ui/card"
import { Skeleton } from "#orck/components/ui/skeleton"
import { useMessages } from "#orck/i18n/context"
import { useScopeProps } from "#orck/scope-context"
import { RESET } from "./format"

export interface SectionProps {
  title: string
  description?: string
  action?: React.ReactNode
  appearance?: CheckoutAppearance
  className?: string
  children?: React.ReactNode
  "data-testid"?: string
}

// Styling root plus card. Each panel is usable alone, so each is a root.
export function Section({
  title,
  description,
  action,
  appearance,
  className,
  children,
  ...rest
}: SectionProps) {
  const scope = useScopeProps(appearance)
  const id = React.useId()
  return (
    <section
      aria-labelledby={id}
      className={cn(scope.className, RESET, "block", className)}
      data-orck-theme={scope["data-orck-theme"]}
      style={scope.style}
      data-testid={rest["data-testid"]}
    >
      <Card className="gap-4 bg-card text-card-foreground">
        <CardHeader>
          <CardTitle id={id} role="heading" aria-level={2}>
            {title}
          </CardTitle>
          {description ? (
            <CardDescription>{description}</CardDescription>
          ) : null}
          {action ? <CardAction>{action}</CardAction> : null}
        </CardHeader>
        <CardContent className="gap-0">{children}</CardContent>
      </Card>
    </section>
  )
}

export function ListSkeleton({ rows = 2 }: { rows?: number }) {
  return (
    <div className="grid gap-4 py-2" aria-hidden>
      {Array.from({ length: rows }, (_, i) => (
        <div key={i} className="flex items-center gap-3">
          <Skeleton className="h-7 w-11 rounded-md" />
          <div className="grid flex-1 gap-2">
            <Skeleton className="h-3.5 w-40" />
            <Skeleton className="h-3 w-28" />
          </div>
          <Skeleton className="h-3.5 w-14" />
        </div>
      ))}
    </div>
  )
}

export function EmptyState({
  title,
  description,
  action,
}: {
  title: string
  description?: string
  action?: React.ReactNode
}) {
  return (
    <div className="grid justify-items-center gap-1 rounded-lg border border-dashed px-4 py-8 text-center">
      <p className="text-sm font-medium">{title}</p>
      {description ? (
        <p className="text-sm text-muted-foreground">{description}</p>
      ) : null}
      {action ? <div className="mt-3">{action}</div> : null}
    </div>
  )
}

export function ErrorState({
  error,
  onRetry,
}: {
  error: BillingError
  onRetry: () => void
}) {
  const m = useMessages()
  return (
    <div
      role="alert"
      className="flex flex-wrap items-center justify-between gap-3 rounded-lg bg-destructive/5 px-4 py-3"
    >
      <p className="text-sm text-destructive">{m.error(error)}</p>
      <Button variant="outline" size="sm" onClick={onRetry}>
        {m.t("common.retry")}
      </Button>
    </div>
  )
}
