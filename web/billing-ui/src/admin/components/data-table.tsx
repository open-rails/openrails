import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
// Server-paginated data table on @tanstack/react-table v8 + the shadcn Table
// primitives (shadcn Data Table pattern).
import { HugeiconsIcon } from "@hugeicons/react"
import { ArrowLeft01Icon, ArrowRight01Icon } from "@hugeicons/core-free-icons"
import {
  flexRender,
  getCoreRowModel,
  useReactTable,
  type ColumnDef,
} from "@tanstack/react-table"
import { Button } from "#orck/admin/components/ui/button"
import { Skeleton } from "#orck/admin/components/ui/skeleton"
import { isInteractiveTarget } from "#orck/admin/lib/dom"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "#orck/admin/components/ui/table"

interface DataTableProps<TData> {
  columns: ColumnDef<TData, unknown>[]
  data: TData[]
  loading?: boolean
  total?: number
  limit?: number
  offset?: number
  onPageChange?: (offset: number) => void
  onRowClick?: (row: TData) => void
  emptyMessage?: string
}

// CARD_ROWS turns the table into stacked cards when the page container is
// narrower than 40rem.
const CARD_ROWS = [
  "@max-[40rem]/page:block @max-[40rem]/page:[&_thead]:hidden @max-[40rem]/page:[&_tbody]:grid @max-[40rem]/page:[&_tbody]:gap-2 @min-[30rem]/page:@max-[40rem]/page:[&_tbody]:grid-cols-2",
  "@max-[40rem]/page:[&_tr]:grid @max-[40rem]/page:[&_tr]:gap-1.5 @max-[40rem]/page:[&_tr]:rounded-lg @max-[40rem]/page:[&_tr]:border-0 @max-[40rem]/page:[&_tr]:ring-1 @max-[40rem]/page:[&_tr]:ring-border @max-[40rem]/page:[&_tr]:ring-inset @max-[40rem]/page:[&_tr]:px-3 @max-[40rem]/page:[&_tr]:py-2.5",
  "@max-[40rem]/page:[&_td:not([colspan])]:flex @max-[40rem]/page:[&_td:not([colspan])]:min-w-0 @max-[40rem]/page:[&_td:not([colspan])]:items-baseline @max-[40rem]/page:[&_td:not([colspan])]:justify-between @max-[40rem]/page:[&_td:not([colspan])]:gap-3 @max-[40rem]/page:[&_td:not([colspan])]:p-0 @max-[40rem]/page:[&_td:not([colspan])]:whitespace-normal",
  "@max-[40rem]/page:[&_td[data-label]]:before:shrink-0 @max-[40rem]/page:[&_td[data-label]]:before:text-xs @max-[40rem]/page:[&_td[data-label]]:before:text-muted-foreground @max-[40rem]/page:[&_td[data-label]]:before:content-[attr(data-label)]",
  "@max-[40rem]/page:[&_td:first-child]:font-medium @max-[40rem]/page:[&_td:first-child]:before:hidden",
].join(" ")

export function DataTable<TData>({
  columns,
  data,
  loading,
  total,
  limit,
  offset,
  onPageChange,
  onRowClick,
  emptyMessage = "No results.",
}: DataTableProps<TData>) {
  const { text: adminText } = useAdminUI()

  // headerLabel is a column's name for its cells in the card layout; a
  // header rendered by a function has none.
  const headerLabel = (header: unknown) =>
    typeof header === "string" ? adminText(header) : undefined

  const table = useReactTable({
    data,
    columns,
    getCoreRowModel: getCoreRowModel(),
    manualPagination: true,
  })

  const paged =
    onPageChange &&
    limit !== undefined &&
    offset !== undefined &&
    total !== undefined

  return (
    <div className="flex flex-col gap-3">
      <div className="overflow-x-auto">
        {/* Below 40rem of page a row of six or more columns no longer fits,
            so each row becomes a card: the first cell is its title and every
            other cell is labelled with its column. From 30rem the cards sit
            two to a row. */}
        <Table className={CARD_ROWS}>
          <TableHeader>
            {table.getHeaderGroups().map((headerGroup) => (
              <TableRow key={headerGroup.id} className="hover:bg-transparent">
                {headerGroup.headers.map((header) => (
                  <TableHead key={header.id} className="text-muted-foreground">
                    {header.isPlaceholder
                      ? null
                      : typeof header.column.columnDef.header === "string"
                        ? adminText(header.column.columnDef.header)
                        : flexRender(
                            header.column.columnDef.header,
                            header.getContext()
                          )}
                  </TableHead>
                ))}
              </TableRow>
            ))}
          </TableHeader>
          <TableBody>
            {loading ? (
              Array.from({ length: 5 }).map((_, ri) => (
                <TableRow key={ri} className="hover:bg-transparent">
                  {columns.map((_, ci) => (
                    <TableCell key={ci} className="py-3.5">
                      <Skeleton className="h-4 w-full max-w-28" />
                    </TableCell>
                  ))}
                </TableRow>
              ))
            ) : table.getRowModel().rows.length ? (
              table.getRowModel().rows.map((row) => (
                <TableRow
                  key={row.id}
                  // A clickable row takes Enter as well as a click, and ignores
                  // both on interactive content inside its cells.
                  tabIndex={onRowClick ? 0 : undefined}
                  role={onRowClick ? "link" : undefined}
                  onClick={
                    onRowClick
                      ? (event) => {
                          if (isInteractiveTarget(event.target)) return
                          onRowClick(row.original)
                        }
                      : undefined
                  }
                  onKeyDown={
                    onRowClick
                      ? (event) => {
                          if (event.key !== "Enter") return
                          if (isInteractiveTarget(event.target)) return
                          onRowClick(row.original)
                        }
                      : undefined
                  }
                  className={
                    onRowClick
                      ? "cursor-pointer focus-visible:bg-muted/50 focus-visible:outline-2 focus-visible:-outline-offset-2 focus-visible:outline-ring"
                      : undefined
                  }
                >
                  {row.getVisibleCells().map((cell) => (
                    <TableCell
                      key={cell.id}
                      className="py-3.5"
                      data-label={headerLabel(cell.column.columnDef.header)}
                    >
                      {flexRender(
                        cell.column.columnDef.cell,
                        cell.getContext()
                      )}
                    </TableCell>
                  ))}
                </TableRow>
              ))
            ) : (
              <TableRow className="hover:bg-transparent">
                <TableCell
                  colSpan={columns.length}
                  className="h-40 text-center text-muted-foreground"
                >
                  {emptyMessage}
                </TableCell>
              </TableRow>
            )}
          </TableBody>
        </Table>
      </div>
      {paged && (
        <div className="flex items-center justify-between text-sm text-muted-foreground">
          <span className="tabular-nums">
            {total === 0
              ? "0"
              : `${offset + 1}–${Math.min(offset + limit, total)}`}{" "}
            <AdminMessage id={"of "} />
            {total}
          </span>
          <div className="flex gap-1">
            <Button
              variant="ghost"
              size="icon"
              aria-label={adminText("Previous page")}
              disabled={offset <= 0 || loading}
              onClick={() => onPageChange(Math.max(0, offset - limit))}
            >
              <HugeiconsIcon icon={ArrowLeft01Icon} className="size-4" />
            </Button>
            <Button
              variant="ghost"
              size="icon"
              aria-label={adminText("Next page")}
              disabled={offset + limit >= total || loading}
              onClick={() => onPageChange(offset + limit)}
            >
              <HugeiconsIcon icon={ArrowRight01Icon} className="size-4" />
            </Button>
          </div>
        </div>
      )}
    </div>
  )
}
