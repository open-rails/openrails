// One dashboard tile: runs its saved query through POST /metrics/query on
// mount, renders the viz, and offers refresh / edit / delete. Count widgets
// deep-link to the matching admin list page (#733 contract).
import { HugeiconsIcon } from "@hugeicons/react"
import {
  ArrowUpRight01Icon,
  Delete02Icon,
  DragDropVerticalIcon,
  MoreVerticalIcon,
  PencilIcon,
  Refresh01Icon,
} from "@hugeicons/core-free-icons"
import { Link } from "react-router-dom"
import { useQuery } from "@tanstack/react-query"
import { Button } from "@/components/ui/button"
import { Card, CardContent } from "@/components/ui/card"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Skeleton } from "@/components/ui/skeleton"
import { ApiError } from "@/lib/api/client"
import {
  type MetricsRange,
  type MetricsResult,
  type Widget,
} from "@/lib/api/metrics"
import { adminQueries } from "@/lib/queries"
import { cn } from "@/lib/utils"

import { deepLinkFor } from "./lib"
import { WidgetVizView } from "./widget-viz"

export function WidgetTile({
  widget,
  range,
  onEdit,
  onDelete,
}: {
  widget: Widget
  range: MetricsRange
  onEdit: () => void
  onDelete: () => void
}) {
  const query = { ...widget.query, range }
  const {
    data,
    isPending: loading,
    isFetching,
    error,
    refetch,
  } = useQuery(adminQueries.widgetMetrics(query))
  const link = widget.viz === "stat" ? deepLinkFor(widget.query) : null

  const isStat = widget.viz === "stat"

  return (
    <Card className="group/tile relative h-full gap-2">
      <div className="absolute top-2 right-2 z-10 flex shrink-0 items-center gap-0.5 rounded-md border border-border bg-card opacity-0 shadow-sm transition-opacity group-hover/tile:opacity-100 focus-within:opacity-100">
        <HugeiconsIcon
          icon={DragDropVerticalIcon}
          aria-hidden
          className="widget-drag-handle size-6 shrink-0 cursor-grab p-1.5 text-muted-foreground active:cursor-grabbing"
        />
        {link ? (
          <Link
            to={link}
            className="flex size-6 items-center justify-center text-muted-foreground hover:text-foreground"
            title="Open matching list"
            aria-label="Open matching list"
          >
            <HugeiconsIcon icon={ArrowUpRight01Icon} className="size-3.5" />
          </Link>
        ) : null}
        <Button
          variant="ghost"
          size="icon"
          className="size-6"
          onClick={() => void refetch()}
          title="Refresh"
          aria-label="Refresh widget"
        >
          <HugeiconsIcon
            icon={Refresh01Icon}
            className={cn("size-3.5", isFetching && "animate-spin")}
          />
        </Button>
        <DropdownMenu>
          <DropdownMenuTrigger
            render={
              <Button
                variant="ghost"
                size="icon"
                className="size-6"
                aria-label="Widget menu"
              >
                <HugeiconsIcon icon={MoreVerticalIcon} className="size-3.5" />
              </Button>
            }
          />
          <DropdownMenuContent align="end">
            <DropdownMenuItem onClick={onEdit}>
              <HugeiconsIcon icon={PencilIcon} className="size-3.5" /> Edit
              widget
            </DropdownMenuItem>
            <DropdownMenuItem onClick={onDelete} variant="destructive">
              <HugeiconsIcon icon={Delete02Icon} className="size-3.5" /> Remove
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
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
      <div className="flex h-full items-center justify-center text-center text-xs text-destructive">
        {msg}
      </div>
    )
  }
  if (!data) return null
  return <WidgetVizView viz={widget.viz} result={data} />
}
