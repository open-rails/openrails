import { cn } from "cn"

import type { BillingAppearance } from "#orck/appearance"
import { buttonVariants } from "#orck/components/ui/button"
import { useMessages } from "#orck/i18n/context"
import { useSubscriptions } from "#orck/react/hooks"
import { useScopeProps, useUiSettings } from "#orck/scope-context"
import { isLive, RESET } from "./format"

export interface RenewalPromptProps {
  /** The host's plans page (uses `navigate` if set). */
  plansHref: string
  appearance?: BillingAppearance
  className?: string
}

/**
 * Invites a customer whose subscriptions have all ended back to the plans
 * page; renders nothing otherwise. A host that grants access some other way
 * leaves it out for customers who have that access.
 */
export function RenewalPrompt({
  plansHref,
  appearance,
  className,
}: RenewalPromptProps) {
  const { t } = useMessages()
  const { navigate } = useUiSettings()
  const scope = useScopeProps(appearance)
  const { subscriptions } = useSubscriptions()
  if (!subscriptions?.length || subscriptions.some(isLive)) return null
  return (
    <div
      data-testid="renewal-prompt"
      data-orck-theme={scope["data-orck-theme"]}
      style={scope.style}
      className={cn(
        scope.className,
        RESET,
        "flex flex-wrap items-center justify-between gap-3 rounded-xl border border-border bg-card p-4 text-card-foreground",
        className
      )}
    >
      <div className="grid min-w-0 gap-1">
        <p className="text-sm font-semibold">{t("renewal.title")}</p>
        <p className="text-sm text-muted-foreground">
          {t("renewal.description")}
        </p>
      </div>
      <a
        href={plansHref}
        className={buttonVariants({ size: "sm" })}
        onClick={(event) => {
          if (!navigate) return
          event.preventDefault()
          navigate(plansHref)
        }}
      >
        {t("renewal.action")}
      </a>
    </div>
  )
}
