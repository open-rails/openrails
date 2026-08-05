import { HugeiconsIcon } from "@hugeicons/react"
import {
  Alert01Icon,
  AlertCircleIcon,
  Notification01Icon,
} from "@hugeicons/core-free-icons"
import * as React from "react"
import { useNavigate } from "react-router-dom"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import type { MerchantNotification } from "@/lib/api/types"
import { timeAgo } from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { adminQueries } from "@/lib/queries"
import { cn } from "@/lib/utils"

// normalizeLink maps an alert's stored link to an in-app router path (basename
// /admin) or an external URL. Returns { path } for router navigation or { href }
// to open in a new tab.
function normalizeLink(link?: string | null): { path?: string; href?: string } {
  if (!link) return {}
  if (/^https?:\/\//i.test(link)) return { href: link }
  let path = link
  if (path.startsWith("/admin/")) path = path.slice("/admin".length)
  else if (path === "/admin") path = "/"
  if (!path.startsWith("/")) path = `/${path}`
  return { path }
}

export function NotificationBell() {
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const readNotification = useMutation(
    adminMutations.markNotificationRead(queryClient)
  )
  const readNotifications = useMutation(
    adminMutations.markNotificationsRead(queryClient)
  )
  const [open, setOpen] = React.useState(false)
  const unreadOptions = adminQueries.unreadNotifications()
  const notificationsOptions = adminQueries.notifications(open)
  const { data: unreadData } = useQuery(unreadOptions)
  const { data: notificationData, isFetching: loading } =
    useQuery(notificationsOptions)
  const count = unreadData?.unread ?? 0
  const items = notificationData?.data ?? []

  const handleOpen = (next: boolean) => {
    setOpen(next)
  }

  const isUnread = (n: MerchantNotification) => !n.read_at

  const onItemClick = async (n: MerchantNotification) => {
    setOpen(false)
    if (isUnread(n)) {
      try {
        await readNotification.mutateAsync(n.id)
      } catch {
        /* best effort */
      }
    }
    const { path, href } = normalizeLink(n.link)
    if (href) window.open(href, "_blank", "noopener,noreferrer")
    else if (path) navigate(path)
  }

  // No bulk endpoint in the contract — mark each currently-listed unread item.
  const markAll = () => {
    const unread = items.filter(isUnread)
    if (unread.length === 0) return
    readNotifications.mutate(unread.map((notification) => notification.id))
  }

  return (
    <DropdownMenu open={open} onOpenChange={handleOpen}>
      <DropdownMenuTrigger
        render={
          <Button
            variant="ghost"
            size="icon"
            aria-label="Notifications"
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
          <span className="text-sm font-medium">Notifications</span>
          {items.some(isUnread) && (
            <Button
              variant="ghost"
              size="sm"
              className="h-6 px-2 text-xs"
              onClick={markAll}
              disabled={readNotifications.isPending}
            >
              {readNotifications.isPending ? "Marking…" : "Mark all read"}
            </Button>
          )}
        </div>
        <div className="max-h-96 overflow-y-auto">
          {loading ? (
            <p className="px-3 py-6 text-center text-sm text-muted-foreground">
              Loading…
            </p>
          ) : items.length === 0 ? (
            <div className="flex flex-col items-center gap-1 px-3 py-8 text-center">
              <HugeiconsIcon
                icon={Notification01Icon}
                className="size-5 text-muted-foreground"
              />
              <p className="text-sm text-muted-foreground">
                You&apos;re all caught up.
              </p>
              <p className="max-w-[16rem] text-xs text-muted-foreground">
                Threshold alerts land here. Configure rules in Settings →
                Alerts.
              </p>
            </div>
          ) : (
            items.map((n) => (
              <button
                key={n.id}
                type="button"
                onClick={() => onItemClick(n)}
                className={cn(
                  "flex w-full items-start gap-2 border-b px-3 py-2 text-left last:border-b-0 hover:bg-muted/50",
                  isUnread(n) && "bg-primary/[0.04]"
                )}
              >
                <span className="mt-0.5 shrink-0">
                  {n.severity === "critical" ? (
                    <HugeiconsIcon
                      icon={AlertCircleIcon}
                      className="size-4 text-red-500"
                    />
                  ) : (
                    <HugeiconsIcon
                      icon={Alert01Icon}
                      className="size-4 text-amber-500"
                    />
                  )}
                </span>
                <span className="min-w-0 flex-1">
                  <span className="flex items-center justify-between gap-2">
                    <span
                      className={cn(
                        "truncate text-sm",
                        isUnread(n) && "font-medium"
                      )}
                    >
                      {n.title}
                    </span>
                    <span className="shrink-0 text-[11px] text-muted-foreground">
                      {timeAgo(n.created_at)}
                    </span>
                  </span>
                  {n.body && (
                    <span className="mt-0.5 line-clamp-2 block text-xs text-muted-foreground">
                      {n.body}
                    </span>
                  )}
                </span>
                {isUnread(n) && (
                  <span
                    className="mt-1.5 size-2 shrink-0 rounded-full bg-primary"
                    aria-label="unread"
                  />
                )}
              </button>
            ))
          )}
        </div>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
