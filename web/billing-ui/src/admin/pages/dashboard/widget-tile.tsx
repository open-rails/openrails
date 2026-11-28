import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
// One dashboard tile: runs its saved query (POST /v1/metrics/query) under
// the page's view, renders the viz, and offers refresh / edit / delete. A kpi
// also runs its sparkline; a heading is a section title. Count widgets
// deep-link to the matching admin list.
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
import { errorText } from "#orck/admin/lib/error-text"
import {
  type MetricsResult,
  type QueryMetricsParams,
  type Widget,
} from "#orck/admin/lib/api/metrics"

import { cn } from "#orck/admin/lib/utils"

import { deepLinkFor, undatedPSPs } from "./lib"
import { ProgramsView } from "./programs"
import {
  rangeDates,
  sparkQuery,
  widgetQuery,
  type DashboardView,
} from "./range"
import type { Registry } from "./registry"
import { WidgetVizView } from "./widget-viz"

interface TileProps {
  widget: Widget
  view: DashboardView
  registry: Registry
  // empty: the merchant has no activity yet; tiles read "—" and run nothing.
  empty?: boolean
  // editable shows the drag handle and the edit and remove menu.
  editable?: boolean
  onEdit: () => void
  onDelete: () => void
}

export function WidgetTile(props: TileProps) {
  if (props.widget.viz === "heading") return <HeadingTile {...props} />
  if (props.widget.viz === "programs") return <ProgramsTile {...props} />
  return <QueryTile {...props} />
}

// ProgramsTile is the dispute program standings for the month the page's
// range ends in.
function ProgramsTile({
  widget,
  view,
  empty = false,
  editable = true,
  onEdit,
  onDelete,
}: TileProps) {
  return (
    <Card
      className="group/tile @container/tile relative h-full gap-2"
      data-widget={widget.id}
    >
      <Toolbar editable={editable} onEdit={onEdit} onDelete={onDelete} />
      <CardContent className="flex min-h-0 flex-1 flex-col gap-2">
        <span className="line-clamp-2 text-sm font-medium" title={widget.title}>
          {widget.title}
        </span>
        <div className="min-h-0 flex-1">
          {empty ? (
            <div className="text-2xl font-semibold tabular-nums">—</div>
          ) : (
            <ProgramsView at={`${rangeDates(view.range).to}T00:00:00Z`} />
          )}
        </div>
      </CardContent>
    </Card>
  )
}

// isTile: a widget drawn as one figure (a kpi or a stat).
export const isTile = (widget: Widget) =>
  widget.viz === "kpi" || widget.viz === "stat"

function HeadingTile({ widget, editable = true, onEdit, onDelete }: TileProps) {
  return (
    <div
      className="group/tile relative flex h-full items-end border-b pb-1"
      data-widget={widget.id}
    >
      <h2 className="text-base font-semibold tracking-tight">{widget.title}</h2>
      {editable ? (
        <Toolbar editable onEdit={onEdit} onDelete={onDelete} />
      ) : null}
    </div>
  )
}

function QueryTile({
  widget,
  view,
  registry,
  empty = false,
  editable = true,
  onEdit,
  onDelete,
}: TileProps) {
  const { adminQueries } = useAdminServices()
  const { text: adminText } = useAdminUI()

  const query = widgetQuery(widget, view, registry.takesCurrency)
  // A measure read from a rollup kept per UTC date says so on its title.
  const utcDays = widget.query.measures.some(
    (m) => registry.measure(m)?.utc_days
  )
  const title = utcDays
    ? `${widget.title} (${adminText("Days are UTC dates.")})`
    : widget.title
  const {
    data,
    isPending: loading,
    isFetching,
    error,
    refetch,
  } = useQuery({ ...adminQueries.widgetMetrics(query), enabled: !empty })
  const spark = useQuery({
    ...adminQueries.widgetMetrics(sparkQuery(query)),
    enabled: !empty && widget.viz === "kpi",
  })
  const link = deepLinkFor(widget.query, widget.viz)

  return (
    <Card
      className="group/tile @container/tile relative h-full gap-2"
      data-widget={widget.id}
    >
      <Toolbar
        editable={editable}
        link={link}
        fetching={isFetching}
        onRefresh={() => {
          void refetch()
          if (widget.viz === "kpi") void spark.refetch()
        }}
        onEdit={onEdit}
        onDelete={onDelete}
      />
      <CardContent className="flex min-h-0 flex-1 flex-col gap-2">
        <span
          className={cn(
            "line-clamp-2",
            isTile(widget)
              ? "text-xs font-medium tracking-wider text-muted-foreground uppercase"
              : "text-sm font-medium"
          )}
          title={title}
        >
          {widget.title}
        </span>
        <div className="min-h-0 flex-1">
          {empty ? (
            <div className="text-2xl font-semibold tabular-nums">—</div>
          ) : (
            <TileBody
              loading={loading && !data}
              error={error}
              widget={widget}
              data={data ?? null}
              spark={spark.data}
              registry={registry}
              query={query}
            />
          )}
        </div>
      </CardContent>
    </Card>
  )
}

function Toolbar({
  editable,
  link,
  fetching,
  onRefresh,
  onEdit,
  onDelete,
}: {
  editable: boolean
  link?: string | null
  fetching?: boolean
  onRefresh?: () => void
  onEdit: () => void
  onDelete: () => void
}) {
  const { text: adminText } = useAdminUI()
  return (
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
      {onRefresh ? (
        <Button
          variant="ghost"
          size="icon"
          className="size-6"
          onClick={onRefresh}
          title={adminText("Refresh")}
          aria-label={adminText("Refresh widget")}
        >
          <HugeiconsIcon
            icon={Refresh01Icon}
            className={cn("size-3.5", fetching && "animate-spin")}
          />
        </Button>
      ) : null}
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
  )
}

function TileBody({
  loading,
  error,
  widget,
  data,
  spark,
  registry,
  query,
}: {
  loading: boolean
  error: unknown
  widget: Widget
  data: MetricsResult | null
  spark?: MetricsResult
  registry: Registry
  query: QueryMetricsParams
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
      error instanceof ApiError && error.isPermissionDenied
        ? "Your role can't see metrics."
        : errorText(error).text
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
  const after =
    data.complete_through &&
    Date.parse(data.range.to) > Date.parse(data.complete_through)
      ? data.complete_through
      : null
  const undated = undatedPSPs(data)
  const view = (
    <WidgetVizView
      viz={widget.viz}
      result={data}
      query={query}
      spark={spark}
      registry={registry}
    />
  )
  if (!after && undated.length === 0) return view
  // A stale PSP's data ends inside the range, or has no known date: the
  // figure is what is known.
  return (
    <div className="flex h-full flex-col gap-1">
      <div className="min-h-0 flex-1">{view}</div>
      <IncompleteNote at={after} undated={undated} />
    </div>
  )
}

function IncompleteNote({
  at,
  undated,
}: {
  at: string | null
  undated: string[]
}) {
  return (
    <span
      className="text-xs text-muted-foreground"
      data-incomplete={at ?? "undated"}
    >
      {at ? (
        <>
          <AdminMessage id={"Incomplete after "} />
          {new Date(at).toLocaleDateString(undefined, {
            month: "short",
            day: "numeric",
            year: "numeric",
          })}
          {undated.length > 0 ? "; " : null}
        </>
      ) : (
        <AdminMessage id={"Incomplete: "} />
      )}
      {undated.length > 0 ? (
        <>
          <AdminMessage id={"undated data from "} />
          {undated.join(", ")}
        </>
      ) : null}
    </span>
  )
}
