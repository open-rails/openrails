import { HugeiconsIcon } from "@hugeicons/react"
import {
  Cancel01Icon,
  Download01Icon,
  Search01Icon,
} from "@hugeicons/core-free-icons"
import * as React from "react"
import { useNavigate, useSearchParams } from "react-router-dom"
import { useQuery } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"

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
import { listCustomers, listPayments } from "@/lib/api/endpoints"
import type { PaymentObject } from "@/lib/api/types"
import { formatMicros, formatUnix, shortId } from "@/lib/format"
import { toastApiError } from "@/lib/toast"
import { adminQueries } from "@/lib/queries"

const PAGE = 50
const EXPORT_PAGE = 200
const RAILS = ["nmi", "ccbill", "stripe", "solana"]

const columns: ColumnDef<PaymentObject, unknown>[] = [
  {
    header: "Amount",
    cell: ({ row }) => (
      <span className="font-medium tabular-nums">
        {formatMicros(row.original.amount, row.original.currency)}
      </span>
    ),
  },
  {
    header: "Status",
    cell: ({ row }) => <StatusBadge status={row.original.status} />,
  },
  { header: "Type", cell: ({ row }) => row.original.object },
  { header: "Rail", cell: ({ row }) => row.original.rail },
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
      row.original.amount_refunded ? (
        <span className="text-muted-foreground tabular-nums">
          {formatMicros(row.original.amount_refunded, row.original.currency)}
        </span>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    header: "Created",
    cell: ({ row }) => (
      <span className="text-muted-foreground">
        {formatUnix(row.original.created)}
      </span>
    ),
  },
]

function csvEscape(v: unknown): string {
  const s = v == null ? "" : String(v)
  return /[",\n]/.test(s) ? '"' + s.replaceAll('"', '""') + '"' : s
}

export function PaymentsPage() {
  const [params, setParams] = useSearchParams()
  const rail = params.get("rail") ?? ""
  const view = params.get("view") ?? ""
  const userId = params.get("user_id") ?? ""
  const customerLabel = params.get("customer") ?? ""
  const offset = Number(params.get("offset") ?? 0)
  const [input, setInput] = React.useState("")
  const [exporting, setExporting] = React.useState(false)
  const navigate = useNavigate()

  const filters = {
    rail: rail || undefined,
    refunds_only: view === "refunds" ? true : undefined,
    user_id: userId || undefined,
  }
  const { data, isPending: loading } = useQuery(
    adminQueries.payments(filters, PAGE, offset)
  )

  const setParam = (key: string, value: string) => {
    const p = new URLSearchParams(params)
    if (value) p.set(key, value)
    else p.delete(key)
    p.delete("offset")
    setParams(p)
  }

  // No free-text search on the payments API — resolve the term to a customer
  // first, then filter by their user id.
  const searchCustomer = async () => {
    const term = input.trim()
    if (!term) return
    try {
      const res = await listCustomers(term, 1, 0)
      const c = res.data[0]
      if (!c) {
        toast.info("No customer matches")
        return
      }
      const p = new URLSearchParams(params)
      p.set("user_id", c.id)
      p.set("customer", c.email || c.subject || shortId(c.id, 13))
      p.delete("offset")
      setParams(p)
      setInput("")
    } catch (err) {
      toastApiError(err, "Search customers")
    }
  }

  const clearCustomer = () => {
    const p = new URLSearchParams(params)
    p.delete("user_id")
    p.delete("customer")
    p.delete("offset")
    setParams(p)
  }

  const exportCsv = async () => {
    setExporting(true)
    try {
      const rows: PaymentObject[] = []
      let cursor = 0
      for (;;) {
        const page = await listPayments(filters, EXPORT_PAGE, cursor)
        rows.push(...page.data)
        if (rows.length >= page.total || page.data.length === 0) break
        cursor += EXPORT_PAGE
      }
      const csv = [
        [
          "id",
          "type",
          "status",
          "amount_micros",
          "amount_refunded_micros",
          "currency",
          "rail",
          "user",
          "transaction_id",
          "created",
        ].join(","),
        ...rows.map((r) =>
          [
            r.id,
            r.object,
            r.status,
            r.amount,
            r.amount_refunded,
            r.currency,
            r.rail,
            r.user,
            r.transaction_id,
            new Date(r.created * 1000).toISOString(),
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
    } finally {
      setExporting(false)
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold tracking-tight">Payments</h1>
        <div className="flex flex-wrap items-center gap-3">
          <form
            className="relative"
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
            disabled={exporting || loading || (data?.total ?? 0) === 0}
          >
            <HugeiconsIcon icon={Download01Icon} className="size-4" />
            {exporting ? "Exporting…" : "Export CSV"}
          </Button>
        </div>
      </div>

      <Tabs value={view} onValueChange={(v) => setParam("view", v ?? "")}>
        <TabsList
          variant="line"
          className="w-full justify-start gap-6 rounded-none p-0"
        >
          <TabsTrigger
            value=""
            className="flex-none px-0 after:bg-primary group-data-horizontal/tabs:after:bottom-[-1px]"
          >
            All
          </TabsTrigger>
          <TabsTrigger
            value="refunds"
            className="flex-none px-0 after:bg-primary group-data-horizontal/tabs:after:bottom-[-1px]"
          >
            Refunds
          </TabsTrigger>
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
        total={data?.total}
        limit={data?.limit ?? PAGE}
        offset={data?.offset ?? offset}
        onPageChange={(next) => {
          const p = new URLSearchParams(params)
          p.set("offset", String(next))
          setParams(p)
        }}
        onRowClick={(row) => navigate(`/payments/${row.id}`)}
        emptyMessage="No payments match."
      />
    </div>
  )
}
