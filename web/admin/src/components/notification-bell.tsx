import * as React from "react"
import { BellIcon, CircleAlertIcon, TriangleAlertIcon } from "lucide-react"
import { useNavigate } from "react-router-dom"

import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import {
  getUnreadCount,
  listNotifications,
  markNotificationRead,
} from "@/lib/api/endpoints"
import type { MerchantNotification } from "@/lib/api/types"
import { timeAgo } from "@/lib/format"
import { cn } from "@/lib/utils"

const POLL_MS = 30_000

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
  const [open, setOpen] = React.useState(false)
  const [count, setCount] = React.useState(0)
  const [items, setItems] = React.useState<MerchantNotification[]>([])
  const [loading, setLoading] = React.useState(false)

  // Poll the unread badge on a modest interval. Errors are swallowed — the bell
  // must never spam toasts (and the endpoint may 404 on an un-migrated server).
  React.useEffect(() => {
    let cancelled = false
    const tick = async () => {
      try {
        const res = await getUnreadCount()
        if (!cancelled) setCount(res.count ?? 0)
      } catch {
        /* ignore poll errors */
      }
    }
    tick()
    const h = setInterval(tick, POLL_MS)
    return () => {
      cancelled = true
      clearInterval(h)
    }
  }, [])

  const loadList = React.useCallback(async () => {
    setLoading(true)
    try {
      const res = await listNotifications()
      setItems(res.data ?? [])
    } catch {
      setItems([])
    } finally {
      setLoading(false)
    }
  }, [])

  const handleOpen = (next: boolean) => {
    setOpen(next)
    if (next) loadList()
  }

  const isUnread = (n: MerchantNotification) => !n.read_at

  const onItemClick = async (n: MerchantNotification) => {
    setOpen(false)
    if (isUnread(n)) {
      try {
        await markNotificationRead(n.id)
        setItems((prev) => prev.map((x) => (x.id === n.id ? { ...x, read_at: new Date().toISOString() } : x)))
        setCount((c) => Math.max(0, c - 1))
      } catch {
        /* best effort */
      }
    }
    const { path, href } = normalizeLink(n.link)
    if (href) window.open(href, "_blank", "noopener,noreferrer")
    else if (path) navigate(path)
  }

  // No bulk endpoint in the contract — mark each currently-listed unread item.
  const markAll = async () => {
    const unread = items.filter(isUnread)
    if (unread.length === 0) return
    await Promise.allSettled(unread.map((n) => markNotificationRead(n.id)))
    const now = new Date().toISOString()
    setItems((prev) => prev.map((x) => (x.read_at ? x : { ...x, read_at: now })))
    setCount(0)
  }

  return (
    <DropdownMenu open={open} onOpenChange={handleOpen}>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" size="icon" aria-label="Notifications" className="relative">
          <BellIcon className="size-4" />
          {count > 0 && (
            <span className="absolute -top-0.5 -right-0.5 flex h-4 min-w-4 items-center justify-center rounded-full bg-destructive px-1 text-[10px] font-medium text-white">
              {count > 9 ? "9+" : count}
            </span>
          )}
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="w-80 p-0 sm:w-96">
        <div className="flex items-center justify-between border-b px-3 py-2">
          <span className="text-sm font-medium">Notifications</span>
          {items.some(isUnread) && (
            <Button variant="ghost" size="sm" className="h-6 px-2 text-xs" onClick={markAll}>
              Mark all read
            </Button>
          )}
        </div>
        <div className="max-h-96 overflow-y-auto">
          {loading ? (
            <p className="px-3 py-6 text-center text-sm text-muted-foreground">Loading…</p>
          ) : items.length === 0 ? (
            <div className="flex flex-col items-center gap-1 px-3 py-8 text-center">
              <BellIcon className="size-5 text-muted-foreground" />
              <p className="text-sm text-muted-foreground">You&apos;re all caught up.</p>
              <p className="max-w-[16rem] text-xs text-muted-foreground">
                Threshold alerts land here. Configure rules in Settings → Alerts.
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
                  isUnread(n) && "bg-primary/[0.04]",
                )}
              >
                <span className="mt-0.5 shrink-0">
                  {n.severity === "critical" ? (
                    <CircleAlertIcon className="size-4 text-red-500" />
                  ) : (
                    <TriangleAlertIcon className="size-4 text-amber-500" />
                  )}
                </span>
                <span className="min-w-0 flex-1">
                  <span className="flex items-center justify-between gap-2">
                    <span className={cn("truncate text-sm", isUnread(n) && "font-medium")}>
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
                  <span className="mt-1.5 size-2 shrink-0 rounded-full bg-primary" aria-label="unread" />
                )}
              </button>
            ))
          )}
        </div>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}
