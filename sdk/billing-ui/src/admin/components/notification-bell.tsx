import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import { HugeiconsIcon } from "@hugeicons/react"
import {
  Alert01Icon,
  AlertCircleIcon,
  Notification01Icon,
} from "@hugeicons/core-free-icons"
import * as React from "react"
import { useNavigate } from "#orck/admin/router"
import { useQuery } from "@tanstack/react-query"

import { Button } from "#orck/admin/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuTrigger,
} from "#orck/admin/components/ui/dropdown-menu"
import type { Finding } from "#orck/admin/lib/api/types"
import { timeAgo } from "#orck/admin/lib/format"
import { useMerchantConfig, useMetrics } from "#orck/admin/lib/capabilities"

// The bell is the findings queue: the open findings that need a person. A
// finding leaves it when it is resolved, by an operator or by itself. Its
// count is a metrics query, shown only to staff holding Metrics.
export function NotificationBell() {
  const { text: adminText } = useAdminUI()

  const { adminQueries } = useAdminServices()

  const navigate = useNavigate()
  const [open, setOpen] = React.useState(false)
  const metrics = useMetrics()
  const merchantConfig = useMerchantConfig()
  const { data: count = 0 } = useQuery({
    ...adminQueries.unreadNotifications(),
    enabled: merchantConfig && metrics,
  })
  const { data, isFetching: loading } = useQuery(
    adminQueries.notifications(merchantConfig && open)
  )
  const items = data?.data ?? []
  if (!merchantConfig) return null

  const onItemClick = (f: Finding) => {
    setOpen(false)
    navigate(`/ops?finding=${encodeURIComponent(f.id)}`)
  }

  return (
    <DropdownMenu open={open} onOpenChange={setOpen}>
      <DropdownMenuTrigger
        render={
          <Button
            variant="ghost"
            size="icon"
            aria-label={adminText("Notifications")}
            className="relative"
          >
            <HugeiconsIcon icon={Notification01Icon} className="size-4" />
            {count > 0 && (
              <span className="absolute -top-0.5 -right-0.5 flex h-4 min-w-4 items-center justify-center rounded-full bg-destructive px-1 text-[10px] font-medium text-white">
                {count > 9 ? "9+" : count}
              </span>
            )}
          </Button>
        }
      />
      <DropdownMenuContent align="end" className="w-80 p-0 sm:w-96">
        <div className="flex items-center justify-between border-b px-3 py-2">
          <span className="text-sm font-medium">
            <AdminMessage id={"Needs attention"} />
          </span>
          {count > items.length && (
            <Button
              variant="ghost"
              size="sm"
              className="h-6 px-2 text-xs"
              onClick={() => {
                setOpen(false)
                navigate("/ops")
              }}
            >
              <AdminMessage id={"View all "} />
              {count}
            </Button>
          )}
        </div>
        <div className="max-h-96 overflow-y-auto">
          {loading ? (
            <p className="px-3 py-6 text-center text-sm text-muted-foreground">
              <AdminMessage id={"Loading…"} />
            </p>
          ) : items.length === 0 ? (
            <div className="flex flex-col items-center gap-1 px-3 py-8 text-center">
              <HugeiconsIcon
                icon={Notification01Icon}
                className="size-5 text-muted-foreground"
              />
              <p className="text-sm text-muted-foreground">
                <AdminMessage id={"You&apos;re all caught up."} />
              </p>
            </div>
          ) : (
            items.map((f) => (
              <button
                key={f.id}
                type="button"
                onClick={() => onItemClick(f)}
                className="flex w-full items-start gap-2 border-b px-3 py-2 text-left last:border-b-0 hover:bg-muted/50"
              >
                <span className="mt-0.5 shrink-0">
                  {f.severity === "critical" || f.severity === "high" ? (
                    <HugeiconsIcon
                      icon={AlertCircleIcon}
                      className="text-failed size-4"
                    />
                  ) : (
                    <HugeiconsIcon
                      icon={Alert01Icon}
                      className="text-held size-4"
                    />
                  )}
                </span>
                <span className="min-w-0 flex-1">
                  <span className="flex items-center justify-between gap-2">
                    <span className="truncate text-sm font-medium">
                      {f.finding_type}
                    </span>
                    <span className="shrink-0 text-[11px] text-muted-foreground">
                      {timeAgo(f.created_at)}
                    </span>
                  </span>
                  {f.recommended_action && (
                    <span className="mt-0.5 line-clamp-2 block text-xs text-muted-foreground">
                      {f.recommended_action}
                    </span>
                  )}
                </span>
              </button>
            ))
          )}
        </div>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
