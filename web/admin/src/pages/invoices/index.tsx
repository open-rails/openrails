import { useState } from "react"
import { Button } from "@/components/ui/button"
import { useQuery } from "@tanstack/react-query"
import { Link, useSearchParams } from "react-router-dom"
import type { ColumnDef } from "@tanstack/react-table"
import { DataTable } from "@/components/data-table"
import { StatusBadge } from "@/components/status-badge"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { invoiceQueries } from "@/lib/invoice-queries"
import type { InvoiceFilters, MerchantInvoice } from "@/lib/api/invoice-types"
import { formatDate, formatUnits, shortId } from "@/lib/format"

const PAGE = 25
const columns: ColumnDef<MerchantInvoice, unknown>[] = [
  {
    header: "Invoice",
    cell: ({ row }) => (
      <Link className="underline" to={`/invoices/${row.original.id}`}>
        {row.original.invoice_number ?? shortId(row.original.id)}
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
  {
    header: "Status",
    cell: ({ row }) => <StatusBadge status={row.original.status} />,
  },
  {
    header: "Period",
    cell: ({ row }) => (
      <span>
        {formatDate(row.original.period_from)} –{" "}
        {formatDate(row.original.period_to)}
      </span>
    ),
  },
  {
    header: "Total",
    cell: ({ row }) =>
      formatUnits(
        row.original.total_amount,
        row.original.currency,
        row.original.unit_decimals
      ),
  },
  {
    header: "Unpaid",
    cell: ({ row }) =>
      formatUnits(
        row.original.amount_due,
        row.original.currency,
        row.original.unit_decimals
      ),
  },
  { header: "Due", cell: ({ row }) => formatDate(row.original.due_at) },
]
export function InvoicesPage() {
  const [params, setParams] = useSearchParams()
  const filters: InvoiceFilters = Object.fromEntries(
    ["customer_id", "currency", "status", "period_from", "period_to"].flatMap(
      (key) => (params.get(key) ? [[key, params.get(key)!]] : [])
    )
  )
  const parsedOffset = Number(params.get("offset") ?? 0),
    offset =
      Number.isSafeInteger(parsedOffset) && parsedOffset >= 0 ? parsedOffset : 0
  const { data, isPending, error } = useQuery(
    invoiceQueries.list(filters, PAGE, offset)
  )

  return (
    <div className="flex flex-col gap-5">
      <div>
        <h1 className="text-xl font-semibold">Invoices</h1>
        <p className="text-sm text-muted-foreground">
          Issued statements, unpaid balances, and payment history.
        </p>
      </div>
      <InvoiceFiltersForm
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
        <DataTable
          columns={columns}
          data={data?.items ?? []}
          loading={isPending}
          total={data?.total ?? 0}
          limit={PAGE}
          offset={offset}
          onPageChange={(next) => {
            const p = new URLSearchParams(params)
            p.set("offset", String(next))
            setParams(p)
          }}
          emptyMessage="No invoices match these filters."
        />
      )}
    </div>
  )
}

function InvoiceFiltersForm({
  filters,
  onApply,
}: {
  filters: InvoiceFilters
  onApply: (filters: InvoiceFilters) => void
}) {
  const [draft, setDraft] = useState<InvoiceFilters>(filters)
  const setFilter = (key: string, value: string) =>
    setDraft((old) => ({ ...old, [key]: value }))
  return (
    <form
      className="grid gap-3 md:grid-cols-5"
      onSubmit={(event) => {
        event.preventDefault()
        onApply(draft)
      }}
    >
      <div>
        <Label htmlFor="invoice-customer">Customer ID</Label>
        <Input
          id="invoice-customer"
          value={draft.customer_id ?? ""}
          onChange={(e) => setFilter("customer_id", e.target.value.trim())}
        />
      </div>
      <div>
        <Label htmlFor="invoice-currency">Currency</Label>
        <Input
          id="invoice-currency"
          maxLength={3}
          placeholder="All currencies"
          value={draft.currency ?? ""}
          onChange={(e) => setFilter("currency", e.target.value.toUpperCase())}
        />
      </div>
      <div>
        <Label htmlFor="invoice-status">Status</Label>
        <select
          id="invoice-status"
          className="h-9 w-full rounded-md border bg-background px-2"
          value={draft.status ?? ""}
          onChange={(e) => setFilter("status", e.target.value)}
        >
          <option value="">All statuses</option>
          {["draft", "open", "past_due", "paid", "voided", "uncollectible"].map(
            (status) => (
              <option key={status} value={status}>
                {status.replaceAll("_", " ")}
              </option>
            )
          )}
        </select>
      </div>
      <div>
        <Label htmlFor="invoice-from">Period starts from</Label>
        <Input
          id="invoice-from"
          type="date"
          value={draft.period_from ?? ""}
          onChange={(e) => setFilter("period_from", e.target.value)}
        />
      </div>
      <div>
        <Label htmlFor="invoice-to">Period starts before</Label>
        <Input
          id="invoice-to"
          type="date"
          value={draft.period_to ?? ""}
          onChange={(e) => setFilter("period_to", e.target.value)}
        />
      </div>
      <div className="flex gap-2 md:col-span-5">
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
