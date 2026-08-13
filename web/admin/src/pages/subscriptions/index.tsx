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
import type { AdminSubscription } from "@/lib/api/types"
import { formatDate, shortId } from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { toast } from "sonner"

import { toastApiError } from "@/lib/toast"
import { adminQueries } from "@/lib/queries"

const PAGE = 50
const RAILS = ["nmi", "ccbill", "stripe", "solana"]

// past_due IS the dunning view (#664 doctrine: park, don't cancel).
const statusTabs = [
  { value: "", label: "All" },
  { value: "active", label: "Active" },
  { value: "past_due", label: "Dunning" },
  { value: "pending", label: "Pending" },
  { value: "cancelled", label: "Cancelled" },
  { value: "unknown", label: "Unknown" },
]

const columns: ColumnDef<AdminSubscription, unknown>[] = [
  {
    header: "Email",
    cell: ({ row }) =>
      row.original.user_email ? (
        <span className="font-medium">{row.original.user_email}</span>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    header: "Status",
    cell: ({ row }) => <StatusBadge status={row.original.status} />,
  },
  { header: "Rail", cell: ({ row }) => row.original.rail },
  {
    header: "Subscription",
    cell: ({ row }) => (
      <span className="text-xs text-muted-foreground">
        {shortId(row.original.id, 13)}
      </span>
    ),
  },
  {
    header: "Started",
    cell: ({ row }) => (
      <span className="text-muted-foreground tabular-nums">
        {formatDate(row.original.started_at)}
      </span>
    ),
  },
  {
    header: "Period ends",
    cell: ({ row }) => (
      <span className="text-muted-foreground tabular-nums">
        {formatDate(row.original.current_period_ends_at)}
      </span>
    ),
  },
  {
    header: "Retries",
    cell: ({ row }) =>
      row.original.status === "past_due" ? (
        `${row.original.retry_attempts ?? 0} · next ${formatDate(row.original.next_retry_at)}`
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
]

function csvEscape(v: unknown): string {
  const s = v == null ? "" : String(v)
  return /[",\n]/.test(s) ? '"' + s.replaceAll('"', '""') + '"' : s
}

export function SubscriptionsPage() {
  const [params, setParams] = useSearchParams()
  const status = params.get("status") ?? ""
  const rail = params.get("rail") ?? ""
  const userId = params.get("user_id") ?? ""
  const customerLabel = params.get("customer") ?? ""
  const offset = Number(params.get("offset") ?? 0)
  const [input, setInput] = React.useState("")
  const navigate = useNavigate()
  const customerLookup = useMutation(adminMutations.findCustomer())
  const exportSubscriptions = useMutation(adminMutations.exportSubscriptions())

  const filters = {
    ...(status ? { status } : {}),
    ...(rail ? { rail } : {}),
    ...(userId ? { user_id: userId } : {}),
  }
  const { data, isPending: loading } = useQuery(
    adminQueries.subscriptions(filters, PAGE, offset)
  )

  const setParam = (key: string, value: string) => {
    const p = new URLSearchParams(params)
    if (value) p.set(key, value)
    else p.delete(key)
    p.delete("offset")
    setParams(p)
  }

  // No free-text search on the subscriptions API — resolve the term to a
  // customer first, then filter by their user id.
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
    try {
      const rows = await exportSubscriptions.mutateAsync(filters)
      const csv = [
        [
          "id",
          "status",
          "rail",
          "email",
          "started_at",
          "current_period_ends_at",
          "cancelled_at",
        ].join(","),
        ...rows.map((r) =>
          [
            r.id,
            r.status,
            r.rail,
            r.user_email,
            r.started_at,
            r.current_period_ends_at,
            r.cancelled_at,
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
      a.download = `subscriptions-${new Date().toISOString().slice(0, 10)}.csv`
      a.click()
      URL.revokeObjectURL(url)
    } catch (err) {
      toastApiError(err, "Export subscriptions")
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold tracking-tight">Subscriptions</h1>
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
            disabled={
              exportSubscriptions.isPending ||
              loading ||
              (data?.total ?? 0) === 0
            }
          >
            <HugeiconsIcon icon={Download01Icon} className="size-4" />
            {exportSubscriptions.isPending ? "Exporting…" : "Export CSV"}
          </Button>
        </div>
      </div>

      <Tabs value={status} onValueChange={(v) => setParam("status", v ?? "")}>
        <TabsList
          variant="line"
          className="w-full justify-start gap-6 rounded-none p-0"
        >
          {statusTabs.map((t) => (
            <TabsTrigger
              key={t.value}
              value={t.value}
              className="flex-none px-0 after:bg-primary group-data-horizontal/tabs:after:bottom-[-1px]"
            >
              {t.label}
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
        total={data?.total}
        limit={data?.limit ?? PAGE}
        offset={data?.offset ?? offset}
        onPageChange={(next) => {
          const p = new URLSearchParams(params)
          p.set("offset", String(next))
          setParams(p)
        }}
        onRowClick={(row) => navigate(`/subscriptions/${row.id}`)}
        emptyMessage="No subscriptions match."
      />
    </div>
  )
}
