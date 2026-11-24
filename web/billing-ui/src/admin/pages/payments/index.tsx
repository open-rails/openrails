import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import { HugeiconsIcon } from "@hugeicons/react"
import { Cancel01Icon, Download01Icon } from "@hugeicons/core-free-icons"
import { useNavigate, useSearchParams } from "#orck/admin/router"
import { useMutation, useQuery } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import type { Customer } from "#orck/client/generated/wire"

import { CursorPager } from "#orck/admin/components/cursor-pager"
import { CustomerSearch } from "#orck/admin/components/customer-search"
import { DataTable } from "#orck/admin/components/data-table"
import { StatusBadge } from "#orck/admin/components/status-badge"
import { Button } from "#orck/admin/components/ui/button"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "#orck/admin/components/ui/select"
import { Tabs, TabsList, TabsTrigger } from "#orck/admin/components/ui/tabs"
import type { PaymentFilters } from "#orck/admin/lib/api/endpoints"
import type { Payment } from "#orck/client/generated/wire"
import { downloadCsv } from "#orck/admin/lib/csv"
import { useUrlCursor } from "#orck/admin/lib/cursor-pages"
import {
  currencyScale,
  formatCard,
  formatNativeAmount,
  formatDate,
  customerName,
  shortId,
  unitsToDecimal,
} from "#orck/admin/lib/format"

const RAILS = ["nmi", "ccbill", "stripe", "solana"]
const KINDS = [
  ["", "All"],
  ["charge", "Charges"],
  ["refund", "Refunds"],
  ["chargeback", "Chargebacks"],
] as const

const columns: ColumnDef<Payment, unknown>[] = [
  {
    header: "Amount",
    cell: ({ row }) => (
      <span className="font-medium tabular-nums">
        {formatNativeAmount(row.original.amount, row.original.currency)}
      </span>
    ),
  },
  {
    header: "Status",
    cell: ({ row }) => <StatusBadge status={row.original.status} />,
  },
  { header: "Kind", cell: ({ row }) => row.original.kind },
  // Off-rail payments name their channel instead.
  {
    header: "Rail",
    cell: ({ row }) => row.original.rail ?? row.original.channel,
  },
  {
    header: "Payment",
    cell: ({ row }) => (
      <span className="text-xs text-muted-foreground">
        {shortId(row.original.id, 16)}
      </span>
    ),
  },
  {
    header: "Refunded",
    cell: ({ row }) =>
      row.original.amount_refunded !== "0" ? (
        <span className="text-muted-foreground tabular-nums">
          {formatNativeAmount(
            row.original.amount_refunded,
            row.original.currency
          )}
        </span>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    header: "Created",
    cell: ({ row }) => (
      <span className="text-muted-foreground tabular-nums">
        {formatDate(row.original.created_at)}
      </span>
    ),
  },
]

// csvAmount exports an exact major-unit decimal; an amount that cannot be
// represented exactly aborts the export rather than writing a wrong figure.
function csvAmount(amount: string, currency: string): string {
  const scale = currencyScale(currency)
  const decimal = scale === undefined ? null : unitsToDecimal(amount, scale)
  if (decimal === null)
    throw new Error(`${currency} amount ${amount} cannot be exported exactly`)
  return decimal
}

export function PaymentsPage() {
  const { toastApiError, adminMutations, adminQueries } = useAdminServices()

  const { text: adminText } = useAdminUI()

  const [params, setParams] = useSearchParams()
  const rail = params.get("rail") ?? ""
  const kind = (params.get("kind") ?? "") as Payment["kind"] | ""
  const userId = params.get("customer_id") ?? ""
  const customerLabel = params.get("customer") ?? ""
  const paging = useUrlCursor()
  const navigate = useNavigate()
  const exportPayments = useMutation(adminMutations.exportPayments())

  const filters: PaymentFilters = {
    rail: rail || undefined,
    kind: kind || undefined,
    customer_id: userId || undefined,
  }
  const {
    data,
    isPending: loading,
    isFetching,
  } = useQuery(adminQueries.payments(filters, paging.size, paging.cursor))

  const setParam = (key: string, value: string) => {
    const p = new URLSearchParams(params)
    if (value) p.set(key, value)
    else p.delete(key)
    p.delete("cursor")
    setParams(p)
  }

  // The payments API has no free-text search: CustomerSearch resolves the
  // term to a customer, then the list filters by its id.
  const pickCustomer = (c: Customer) => {
    const p = new URLSearchParams(params)
    p.set("customer_id", c.id)
    p.set("customer", customerName(c))
    p.delete("cursor")
    setParams(p)
  }

  const clearCustomer = () => {
    const p = new URLSearchParams(params)
    p.delete("customer_id")
    p.delete("customer")
    p.delete("cursor")
    setParams(p)
  }

  const exportCsv = async () => {
    try {
      const rows = await exportPayments.mutateAsync(filters)
      downloadCsv(
        `payments-${new Date().toISOString().slice(0, 10)}.csv`,
        [
          "id",
          "kind",
          "status",
          "channel",
          "amount",
          "amount_refunded",
          "currency",
          "rail",
          "card",
          "customer_id",
          "transaction_id",
          "created_at",
        ],
        rows.map((r) => [
          r.id,
          r.kind,
          r.status,
          r.channel,
          csvAmount(r.amount, r.currency),
          csvAmount(r.amount_refunded, r.currency),
          r.currency,
          r.rail,
          r.card?.last4 ? formatCard(r.card) : "",
          r.customer_id,
          r.transaction_id,
          r.created_at,
        ])
      )
    } catch (err) {
      toastApiError(err, "Export payments")
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold tracking-tight">
          <AdminMessage id={"Payments"} />
        </h1>
        <div className="flex w-full flex-wrap items-center gap-3 sm:w-auto">
          <CustomerSearch onPick={pickCustomer} />
          <Select
            items={[
              { value: "all", label: "All rails" },
              ...RAILS.map((r) => ({ value: r, label: r })),
            ]}
            value={rail || "all"}
            onValueChange={(v) => setParam("rail", !v || v === "all" ? "" : v)}
          >
            <SelectTrigger className="w-36">
              <SelectValue placeholder={adminText("Rail")} />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">
                <AdminMessage id={"All rails"} />
              </SelectItem>
              {RAILS.map((r) => (
                <SelectItem key={r} value={r}>
                  {r}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Button
            variant="outline"
            size="sm"
            onClick={exportCsv}
            disabled={exportPayments.isPending || loading || !data?.data.length}
          >
            <HugeiconsIcon icon={Download01Icon} className="size-4" />
            {exportPayments.isPending ? "Exporting…" : "Export CSV"}
          </Button>
        </div>
      </div>

      <Tabs value={kind} onValueChange={(v) => setParam("kind", v ?? "")}>
        <TabsList
          variant="line"
          className="w-full justify-start gap-6 overflow-x-auto rounded-none p-0"
        >
          {KINDS.map(([value, label]) => (
            <TabsTrigger
              key={value}
              value={value}
              className="h-8 flex-none px-0 after:inset-x-0 after:bottom-0 after:h-0.5 after:bg-primary pointer-coarse:h-10"
            >
              {label}
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>

      {userId ? (
        <div className="flex items-center gap-2 text-sm">
          <span className="text-muted-foreground">
            <AdminMessage id={"Customer"} />
          </span>
          <span className="inline-flex items-center gap-1 rounded-md bg-muted py-0.5 pr-1 pl-2 font-medium">
            {customerLabel || shortId(userId, 13)}
            <Button
              variant="ghost"
              size="icon"
              className="size-5"
              aria-label={adminText("Clear customer filter")}
              onClick={clearCustomer}
            >
              <HugeiconsIcon icon={Cancel01Icon} className="size-3" />
            </Button>
          </span>
        </div>
      ) : null}

      <DataTable
        columns={columns}
        data={data?.data ?? []}
        loading={loading}
        onRowClick={(row) => navigate(`/payments/${row.id}`)}
        emptyMessage="No payments match."
      />
      <CursorPager
        pages={paging}
        rows={data?.data?.length ?? 0}
        nextCursor={data?.next_cursor}
        busy={isFetching}
      />
    </div>
  )
}
