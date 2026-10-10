import { HugeiconsIcon } from "@hugeicons/react"
import {
  Cancel01Icon,
  Download01Icon,
  Search01Icon,
} from "@hugeicons/core-free-icons"
import * as React from "react"
import { useNavigate, useSearchParams } from "react-router-dom"
import { useMutation, useQuery } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"

import { CursorPager } from "@/components/cursor-pager"
import { DataTable } from "@/components/data-table"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import type { PaymentFilters } from "@/lib/api/endpoints"
import type { Payment } from "@/lib/api/generated/wire"
import { useUrlCursor } from "@/hooks/use-cursor-paging"
import {
  currencyScale,
  formatNativeAmount,
  formatDate,
  customerName,
  shortId,
  unitsToDecimal,
} from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { toastApiError } from "@/lib/toast"
import { adminQueries } from "@/lib/queries"

const PAGE = 50
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

function csvEscape(v: unknown): string {
  const s = v == null ? "" : String(v)
  return /[",\n]/.test(s) ? '"' + s.replaceAll('"', '""') + '"' : s
}

export function PaymentsPage() {
  const [params, setParams] = useSearchParams()
  const rail = params.get("rail") ?? ""
  const kind = (params.get("kind") ?? "") as Payment["kind"] | ""
  const userId = params.get("customer_id") ?? ""
  const customerLabel = params.get("customer") ?? ""
  const paging = useUrlCursor()
  const [input, setInput] = React.useState("")
  const navigate = useNavigate()
  const customerLookup = useMutation(adminMutations.findCustomer())
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
  } = useQuery(adminQueries.payments(filters, PAGE, paging.cursor))

  const setParam = (key: string, value: string) => {
    const p = new URLSearchParams(params)
    if (value) p.set(key, value)
    else p.delete(key)
    p.delete("cursor")
    setParams(p)
  }

  // The payments API has no free-text search: resolve the term to a customer,
  // then filter by its id.
  const searchCustomer = async () => {
    const term = input.trim()
    if (!term) return
    try {
      const c = await customerLookup.mutateAsync(term)
      if (!c) {
        toast.info("No customer matches")
        return
      }
      const p = new URLSearchParams(params)
      p.set("customer_id", c.id)
      p.set("customer", customerName(c))
      p.delete("cursor")
      setParams(p)
      setInput("")
    } catch (err) {
      toastApiError(err, "Search customers")
    }
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
      const csv = [
        [
          "id",
          "kind",
          "status",
          "channel",
          "amount",
          "amount_refunded",
          "currency",
          "rail",
          "customer_id",
          "transaction_id",
          "created_at",
        ].join(","),
        ...rows.map((r) =>
          [
            r.id,
            r.kind,
            r.status,
            r.channel,
            csvAmount(r.amount, r.currency),
            csvAmount(r.amount_refunded, r.currency),
            r.currency,
            r.rail,
            r.customer_id,
            r.transaction_id,
            r.created_at,
          ]
            .map(csvEscape)
            .join(",")
        ),
      ].join("\n")
      const url = URL.createObjectURL(
        new Blob([csv], { type: "text/csv;charset=utf-8" })
      )
      const a = document.createElement("a")
      a.href = url
      a.download = `payments-${new Date().toISOString().slice(0, 10)}.csv`
      a.click()
      URL.revokeObjectURL(url)
    } catch (err) {
      toastApiError(err, "Export payments")
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold tracking-tight">Payments</h1>
        <div className="flex flex-wrap items-center gap-3">
          <form
            className="relative"
            aria-busy={customerLookup.isPending}
            onSubmit={(e) => {
              e.preventDefault()
              void searchCustomer()
            }}
          >
            <HugeiconsIcon
              icon={Search01Icon}
              className="absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground"
            />
            <Input
              className="w-64 pl-8"
              placeholder="Search customer email, ref, or id…"
              value={input}
              disabled={customerLookup.isPending}
              onChange={(e) => setInput(e.target.value)}
            />
          </form>
          <Select
            items={[
              { value: "all", label: "All rails" },
              ...RAILS.map((r) => ({ value: r, label: r })),
            ]}
            value={rail || "all"}
            onValueChange={(v) => setParam("rail", !v || v === "all" ? "" : v)}
          >
            <SelectTrigger className="w-36">
              <SelectValue placeholder="Rail" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem value="all">All rails</SelectItem>
              {RAILS.map((r) => (
                <SelectItem key={r} value={r}>
                  {r}
                </SelectItem>
              ))}
            </SelectContent>
          </Select>
          <Button
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
          className="w-full justify-start gap-6 rounded-none p-0"
        >
          {KINDS.map(([value, label]) => (
            <TabsTrigger
              key={value}
              value={value}
              className="flex-none px-0 after:bg-primary group-data-horizontal/tabs:after:bottom-[-1px]"
            >
              {label}
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>

      {userId ? (
        <div className="flex items-center gap-2 text-sm">
          <span className="text-muted-foreground">Customer</span>
          <span className="inline-flex items-center gap-1 rounded-md bg-muted py-0.5 pr-1 pl-2 font-medium">
            {customerLabel || shortId(userId, 13)}
            <Button
              variant="ghost"
              size="icon"
              className="size-5"
              aria-label="Clear customer filter"
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
        nextCursor={data?.next_cursor}
        busy={isFetching}
      />
    </div>
  )
}
