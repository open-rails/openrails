import * as React from "react"
import { useNavigate, useSearchParams } from "react-router-dom"
import type { ColumnDef } from "@tanstack/react-table"

import { DataTable } from "@/components/data-table"
import { StatusBadge } from "@/components/status-badge"
import { Tabs, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { useApiData } from "@/hooks/use-api-data"
import { listSubscriptions } from "@/lib/api/endpoints"
import type { AdminSubscription } from "@/lib/api/types"
import { formatDate, shortId } from "@/lib/format"
import { toastApiError } from "@/lib/toast"

const PAGE = 50

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
    header: "Subscription",
    cell: ({ row }) => (
      <span className="text-xs">{shortId(row.original.id, 13)}</span>
    ),
  },
  {
    header: "Status",
    cell: ({ row }) => <StatusBadge status={row.original.status} />,
  },
  { header: "Rail", cell: ({ row }) => row.original.rail },
  { header: "Email", cell: ({ row }) => row.original.user_email ?? "—" },
  { header: "Started", cell: ({ row }) => formatDate(row.original.started_at) },
  {
    header: "Period ends",
    cell: ({ row }) => formatDate(row.original.current_period_ends_at),
  },
  {
    header: "Retries",
    cell: ({ row }) =>
      row.original.status === "past_due"
        ? `${row.original.retry_attempts ?? 0} · next ${formatDate(row.original.next_retry_at)}`
        : "—",
  },
]

export function SubscriptionsPage() {
  const [params, setParams] = useSearchParams()
  const status = params.get("status") ?? ""
  const offset = Number(params.get("offset") ?? 0)
  const navigate = useNavigate()

  const { data, loading, error } = useApiData(
    () => listSubscriptions(status ? { status } : {}, PAGE, offset),
    [status, offset]
  )
  React.useEffect(() => {
    if (error) toastApiError(error, "Load subscriptions")
  }, [error])

  return (
    <div className="flex flex-col gap-4">
      <Tabs
        value={status}
        onValueChange={(v) => setParams(v ? { status: v } : {})}
      >
        <TabsList>
          {statusTabs.map((t) => (
            <TabsTrigger key={t.value} value={t.value}>
              {t.label}
            </TabsTrigger>
          ))}
        </TabsList>
      </Tabs>
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
      />
    </div>
  )
}
