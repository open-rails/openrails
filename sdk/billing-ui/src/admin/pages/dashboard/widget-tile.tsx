import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
// One dashboard tile: runs its saved query (POST /v1/metrics/query),
// renders the viz, and offers refresh / edit / delete. Count widgets deep-link
// to the matching admin list.
import { HugeiconsIcon } from "@hugeicons/react"
import {
  ArrowUpRight01Icon,
  Delete02Icon,
  DragDropVerticalIcon,
  MoreVerticalIcon,
  PencilIcon,
  Refresh01Icon,
} from "@hugeicons/core-free-icons"
import { Link } from "#orck/admin/router"
import { useQuery } from "@tanstack/react-query"
import { Button } from "#orck/admin/components/ui/button"
import { Card, CardContent } from "#orck/admin/components/ui/card"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "#orck/admin/components/ui/dropdown-menu"
import { Skeleton } from "#orck/admin/components/ui/skeleton"
import { ApiError } from "#orck/admin/lib/api/client"
import { type MetricsResult, type Widget } from "#orck/admin/lib/api/metrics"

import { cn } from "#orck/admin/lib/utils"

import { deepLinkFor } from "./lib"
import { widgetQuery, type DashboardRange } from "./range"
import { WidgetVizView } from "./widget-viz"

export function WidgetTile({
  widget,
  range,
  editable = true,
  onEdit,
  onDelete,
}: {
  widget: Widget
  range: DashboardRange
  // editable shows the drag handle and the edit and remove menu.
  editable?: boolean
  onEdit: () => void
  onDelete: () => void
}) {
  const { text: adminText } = useAdminUI()

  const { adminQueries } = useAdminServices()

  const query = widgetQuery(widget, range)
  const {
    data,
    isPending: loading,
    isFetching,
    error,
    refetch,
  } = useQuery(adminQueries.widgetMetrics(query))
  const link = deepLinkFor(widget.query, widget.viz)

  const isStat = widget.viz === "stat"

  return (
    <Card className="group/tile relative h-full gap-2">
      <div className="absolute top-2 right-2 z-10 flex shrink-0 items-center gap-0.5 rounded-md border border-border bg-card opacity-0 shadow-sm transition-opacity group-hover/tile:opacity-100 focus-within:opacity-100">
        {editable && (
          <HugeiconsIcon
            icon={DragDropVerticalIcon}
            aria-hidden
            className="widget-drag-handle size-6 shrink-0 cursor-grab p-1.5 text-muted-foreground active:cursor-grabbing"
          />
        )}
        {link ? (
          <Link
            to={link}
            className="flex size-6 items-center justify-center text-muted-foreground hover:text-foreground"
            title={adminText("Open matching list")}
            aria-label={adminText("Open matching list")}
          >
            <HugeiconsIcon icon={ArrowUpRight01Icon} className="size-3.5" />
          </Link>
        ) : null}
        <Button
          variant="ghost"
          size="icon"
          className="size-6"
          onClick={() => void refetch()}
          title={adminText("Refresh")}
          aria-label={adminText("Refresh widget")}
        >
          <HugeiconsIcon
            icon={Refresh01Icon}
            className={cn("size-3.5", isFetching && "animate-spin")}
          />
        </Button>
        {editable && (
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button
                  variant="ghost"
                  size="icon"
                  className="size-6"
                  aria-label={adminText("Widget menu")}
                >
                  <HugeiconsIcon icon={MoreVerticalIcon} className="size-3.5" />
                </Button>
              }
            />
            <DropdownMenuContent align="end">
              <DropdownMenuItem onClick={onEdit}>
                <HugeiconsIcon icon={PencilIcon} className="size-3.5" />
                <AdminMessage id={" Edit widget"} />
              </DropdownMenuItem>
              <DropdownMenuItem onClick={onDelete} variant="destructive">
                <HugeiconsIcon icon={Delete02Icon} className="size-3.5" />{" "}
                <AdminMessage id={"Remove"} />
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        )}
      </div>
      <CardContent className="flex min-h-0 flex-1 flex-col gap-2">
        <span
          className={cn(
            "truncate",
            isStat
              ? "text-xs font-medium tracking-wider text-muted-foreground uppercase"
              : "text-sm font-medium"
          )}
          title={widget.title}
        >
          {widget.title}
        </span>
        <div className="min-h-0 flex-1">
          <TileBody
            loading={loading && !data}
            error={error}
            widget={widget}
            data={data ?? null}
          />
        </div>
      </CardContent>
    </Card>
  )
}

function TileBody({
  loading,
  error,
  widget,
  data,
}: {
  loading: boolean
  error: unknown
  widget: Widget
  data: MetricsResult | null
}) {
  if (loading) {
    return (
      <div className="flex h-full flex-col justify-center gap-2">
        <Skeleton className="h-6 w-2/3" />
        <Skeleton className="h-3 w-1/3" />
      </div>
    )
  }
  if (error) {
    const msg =
      error instanceof ApiError
        ? error.isPermissionDenied
          ? "your role lacks metrics access"
          : error.message
        : "query failed"
    return (
      <div
        className="flex h-full items-center justify-center text-center text-xs text-destructive"
        role="alert"
      >
        {msg}
      </div>
    )
  }
  if (!data) return null
  return <WidgetVizView viz={widget.viz} result={data} query={widget.query} />
}
