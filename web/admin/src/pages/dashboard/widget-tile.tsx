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
import { Button } from "@/components/ui/button"
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from "@/components/ui/dropdown-menu"
import { Skeleton } from "@/components/ui/skeleton"
import { useApiData } from "@/hooks/use-api-data"
import { ApiError } from "@/lib/api/client"
import {
  metricsQuery,
  type MetricsResult,
  type Widget,
} from "@/lib/api/metrics"
import { cn } from "@/lib/utils"

import { deepLinkFor } from "./lib"
import { WidgetVizView } from "./widget-viz"

export function WidgetTile({
  widget,
  onEdit,
  onDelete,
}: {
  widget: Widget
  onEdit: () => void
  onDelete: () => void
}) {
  const queryKey = JSON.stringify(widget.query)
  const { data, loading, error, reload } = useApiData(
    () => metricsQuery(widget.query),
    [queryKey]
  )
  const link = widget.viz === "stat" ? deepLinkFor(widget.query) : null

  return (
    <div className="flex h-full flex-col rounded-xl border bg-card text-card-foreground shadow-sm">
      <div className="flex items-center gap-1 border-b px-3 py-1.5">
        <HugeiconsIcon
          icon={DragDropVerticalIcon}
          className="widget-drag-handle size-3.5 shrink-0 cursor-grab text-muted-foreground/60 active:cursor-grabbing"
        />
        <span className="truncate text-sm font-medium" title={widget.title}>
          {widget.title}
        </span>
        {link ? (
          <Link
            to={link}
            className="shrink-0 text-muted-foreground hover:text-foreground"
            title="Open matching list"
          >
            <HugeiconsIcon icon={ArrowUpRight01Icon} className="size-3.5" />
          </Link>
        ) : null}
        <div className="ml-auto flex shrink-0 items-center">
          <Button
            variant="ghost"
            size="icon"
            className="size-6"
            onClick={reload}
            title="Refresh"
          >
            <HugeiconsIcon
              icon={Refresh01Icon}
              className={cn("size-3.5", loading && "animate-spin")}
            />
          </Button>
          <DropdownMenu>
            <DropdownMenuTrigger
              render={
                <Button variant="ghost" size="icon" className="size-6">
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
                <HugeiconsIcon icon={Delete02Icon} className="size-3.5" />{" "}
                Remove
              </DropdownMenuItem>
            </DropdownMenuContent>
          </DropdownMenu>
        </div>
      </div>
      <div className="min-h-0 flex-1 p-3">
        <TileBody
          loading={loading && !data}
          error={error}
          widget={widget}
          data={data}
        />
      </div>
    </div>
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
