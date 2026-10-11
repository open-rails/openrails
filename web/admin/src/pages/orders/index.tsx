import { useState } from "react"
import { Button } from "@/components/ui/button"
import { useQuery } from "@tanstack/react-query"
import { Link, useSearchParams } from "react-router-dom"
import type { ColumnDef } from "@tanstack/react-table"
import { CursorPager } from "@/components/cursor-pager"
import { DataTable } from "@/components/data-table"
import { StatusBadge } from "@/components/status-badge"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { orderQueries } from "@/lib/order-queries"
import type { Order } from "@/lib/api/generated/wire"
import type { OrderFilters } from "@/lib/api/order-endpoints"
import { useUrlCursor } from "@/hooks/use-cursor-paging"
import { formatDate, formatNativeAmount, shortId } from "@/lib/format"

const PAGE = 25
// dueAt is when an order is owed: a usage order's or a renewal's due date.
const dueAt = (order: Order) =>
  order.usage?.due_at ?? order.renewal?.due_at ?? null
const columns: ColumnDef<Order, unknown>[] = [
  {
    header: "Order",
    cell: ({ row }) => (
      <Link className="underline" to={`/orders/${row.original.id}`}>
        {row.original.number ?? shortId(row.original.id)}
      </Link>
    ),
  },
  {
    header: "Customer",
    cell: ({ row }) => (
      <Link to={`/customers/${row.original.customer_id}`}>
        {shortId(row.original.customer_id)}
      </Link>
    ),
  },
  { header: "Origin", cell: ({ row }) => row.original.origin },
  {
    header: "Status",
    cell: ({ row }) => (
      <span className="flex gap-1">
        <StatusBadge status={row.original.status} />
        {/* Past grace: the customer's new usage in this currency is refused. */}
        {row.original.usage?.delinquent && <StatusBadge status="delinquent" />}
      </span>
    ),
  },
  {
    header: "Total",
    cell: ({ row }) =>
      formatNativeAmount(row.original.total, row.original.currency),
  },
  {
    header: "Unpaid",
    cell: ({ row }) =>
      row.original.usage
        ? formatNativeAmount(
            row.original.usage.amount_due,
            row.original.currency
          )
        : "—",
  },
  { header: "Due", cell: ({ row }) => formatDate(dueAt(row.original)) },
  { header: "Created", cell: ({ row }) => formatDate(row.original.created_at) },
]
export function OrdersPage() {
  const [params, setParams] = useSearchParams()
  const filters: OrderFilters = Object.fromEntries(
    ["customer_id", "origin", "status", "overdue"].flatMap((key) =>
      params.get(key) ? [[key, params.get(key)!]] : []
    )
  )
  const paging = useUrlCursor()
  const { data, isPending, isFetching, error } = useQuery(
    orderQueries.list(filters, PAGE, paging.cursor)
  )

  return (
    <div className="flex flex-col gap-5">
      <div>
        <h1 className="text-xl font-semibold">Orders</h1>
        <p className="text-sm text-muted-foreground">
          Purchases, subscription renewals and usage billed in arrears: what
          each owes and how it was paid.
        </p>
      </div>
      <OrderFiltersForm
        key={JSON.stringify(filters)}
        filters={filters}
        onApply={(next) => {
          const p = new URLSearchParams()
          for (const [key, value] of Object.entries(next)) {
            if (value?.trim()) p.set(key, value.trim())
          }
          setParams(p)
        }}
      />
      {error ? (
        <p role="alert" className="text-destructive">
          {error.message}
        </p>
      ) : (
        <>
          <DataTable
            columns={columns}
            data={data?.data ?? []}
            loading={isPending}
            emptyMessage="No orders match these filters."
          />
          <CursorPager
            pages={paging}
            nextCursor={data?.next_cursor}
            busy={isFetching}
          />
        </>
      )}
    </div>
  )
}

function OrderFiltersForm({
  filters,
  onApply,
}: {
  filters: OrderFilters
  onApply: (filters: OrderFilters) => void
}) {
  const [draft, setDraft] = useState<OrderFilters>(filters)
  const setFilter = (key: string, value: string) =>
    setDraft((old) => ({ ...old, [key]: value }))
  return (
    <form
      className="grid gap-3 md:grid-cols-3"
      onSubmit={(event) => {
        event.preventDefault()
        onApply(draft)
      }}
    >
      <div>
        <Label htmlFor="order-customer">Customer ID</Label>
        <Input
          id="order-customer"
          value={draft.customer_id ?? ""}
          onChange={(e) => setFilter("customer_id", e.target.value.trim())}
        />
      </div>
      <div>
        <Label htmlFor="order-origin">Origin</Label>
        <select
          id="order-origin"
          className="h-9 w-full rounded-md border bg-background px-2"
          value={draft.origin ?? ""}
          onChange={(e) => setFilter("origin", e.target.value)}
        >
          <option value="">All origins</option>
          {["customer", "merchant", "subscription", "usage"].map((origin) => (
            <option key={origin} value={origin}>
              {origin}
            </option>
          ))}
        </select>
      </div>
      <div>
        <Label htmlFor="order-status">Status</Label>
        <select
          id="order-status"
          className="h-9 w-full rounded-md border bg-background px-2"
          value={draft.status ?? ""}
          onChange={(e) => setFilter("status", e.target.value)}
        >
          <option value="">All statuses</option>
          {[
            "open",
            "processing",
            "complete",
            "canceled",
            "expired",
            "void",
            "uncollectible",
          ].map((status) => (
            <option key={status} value={status}>
              {status}
            </option>
          ))}
        </select>
      </div>
      <label className="flex items-center gap-2 text-sm md:col-span-3">
        <input
          type="checkbox"
          checked={draft.overdue === "true"}
          onChange={(e) => setFilter("overdue", e.target.checked ? "true" : "")}
        />
        Overdue only: still owed past the due date
      </label>
      <div className="flex gap-2 md:col-span-3">
        <Button type="submit">Apply filters</Button>
        <Button
          type="button"
          variant="outline"
          onClick={() => {
            setDraft({})
            onApply({})
          }}
        >
          Clear filters
        </Button>
      </div>
    </form>
  )
}
