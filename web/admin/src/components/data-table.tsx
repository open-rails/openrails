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
import { Button } from "@/components/ui/button"
import { Skeleton } from "@/components/ui/skeleton"
import { isInteractiveTarget } from "@/lib/dom"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"

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
        <Table>
          <TableHeader>
            {table.getHeaderGroups().map((headerGroup) => (
              <TableRow key={headerGroup.id} className="hover:bg-transparent">
                {headerGroup.headers.map((header) => (
                  <TableHead key={header.id} className="text-muted-foreground">
                    {header.isPlaceholder
                      ? null
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
                  // A clickable row is a shortcut, not a control: it takes
                  // Enter as well as a click, and it keeps its hands off
                  // whatever the cells put inside it.
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
                      ? "cursor-pointer focus-visible:bg-muted/50 focus-visible:outline-none"
                      : undefined
                  }
                >
                  {row.getVisibleCells().map((cell) => (
                    <TableCell key={cell.id} className="py-3.5">
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
            of {total}
          </span>
          <div className="flex gap-1">
            <Button
              variant="ghost"
              size="icon"
              aria-label="Previous page"
              disabled={offset <= 0 || loading}
              onClick={() => onPageChange(Math.max(0, offset - limit))}
            >
              <HugeiconsIcon icon={ArrowLeft01Icon} className="size-4" />
            </Button>
            <Button
              variant="ghost"
              size="icon"
              aria-label="Next page"
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
