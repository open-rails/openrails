import * as React from "react"
import { useNavigate, useSearchParams } from "react-router-dom"
import type { ColumnDef } from "@tanstack/react-table"

import { DataTable } from "@/components/data-table"
import { StatusBadge } from "@/components/status-badge"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useApiData } from "@/hooks/use-api-data"
import { listPayments } from "@/lib/api/endpoints"
import type { PaymentObject } from "@/lib/api/types"
import { formatMicros, formatUnix, shortId } from "@/lib/format"
import { toastApiError } from "@/lib/toast"

const PAGE = 50
const RAILS = ["nmi", "ccbill", "stripe", "solana"]

const columns: ColumnDef<PaymentObject, unknown>[] = [
  { header: "Payment", cell: ({ row }) => <span className="font-mono text-xs">{shortId(row.original.id, 16)}</span> },
  { header: "Type", cell: ({ row }) => row.original.object },
  { header: "Status", cell: ({ row }) => <StatusBadge status={row.original.status} /> },
  { header: "Amount", cell: ({ row }) => formatMicros(row.original.amount, row.original.currency) },
  {
    header: "Refunded",
    cell: ({ row }) =>
      row.original.amount_refunded
        ? formatMicros(row.original.amount_refunded, row.original.currency)
        : "—",
  },
  { header: "Rail", cell: ({ row }) => row.original.rail },
  { header: "Created", cell: ({ row }) => formatUnix(row.original.created) },
]

export function PaymentsPage() {
  const [params, setParams] = useSearchParams()
  const rail = params.get("rail") ?? ""
  const view = params.get("view") ?? ""
  const offset = Number(params.get("offset") ?? 0)
  const navigate = useNavigate()

  const { data, loading, error } = useApiData(
    () =>
      listPayments(
        { rail: rail || undefined, refunds_only: view === "refunds" ? true : undefined },
        PAGE,
        offset,
      ),
    [rail, view, offset],
  )
  React.useEffect(() => {
    if (error) toastApiError(error, "Load payments")
  }, [error])

  const setParam = (key: string, value: string) => {
    const p = new URLSearchParams(params)
    if (value) p.set(key, value)
    else p.delete(key)
    p.delete("offset")
    setParams(p)
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-3">
        <Tabs value={view} onValueChange={(v) => setParam("view", v)}>
          <TabsList>
            <TabsTrigger value="">All</TabsTrigger>
            <TabsTrigger value="refunds">Refunds</TabsTrigger>
          </TabsList>
        </Tabs>
        <Select value={rail || "all"} onValueChange={(v) => setParam("rail", v === "all" ? "" : v)}>
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
      </div>
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
      />
    </div>
  )
}
