import { HugeiconsIcon } from "@hugeicons/react"
import { Search01Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { useNavigate, useSearchParams } from "react-router-dom"
import type { ColumnDef } from "@tanstack/react-table"

import { DataTable } from "@/components/data-table"
import { Input } from "@/components/ui/input"
import { useApiData } from "@/hooks/use-api-data"
import { listCustomers } from "@/lib/api/endpoints"
import type { CustomerSummary } from "@/lib/api/types"
import { formatDate, shortId } from "@/lib/format"
import { toastApiError } from "@/lib/toast"

const PAGE = 50

const columns: ColumnDef<CustomerSummary, unknown>[] = [
  {
    header: "Customer",
    cell: ({ row }) => (
      <span className="text-xs">{shortId(row.original.id, 13)}</span>
    ),
  },
  {
    header: "Subject / external ref",
    cell: ({ row }) => row.original.subject ?? "—",
  },
  { header: "Email", cell: ({ row }) => row.original.email ?? "—" },
  {
    header: "First seen",
    cell: ({ row }) => formatDate(row.original.created_at),
  },
  {
    header: "Last seen",
    cell: ({ row }) => formatDate(row.original.last_seen_at),
  },
]

export function CustomersPage() {
  const [params, setParams] = useSearchParams()
  const q = params.get("q") ?? ""
  const offset = Number(params.get("offset") ?? 0)
  const [input, setInput] = React.useState(q)
  const navigate = useNavigate()

  const { data, loading, error } = useApiData(
    () => listCustomers(q, PAGE, offset),
    [q, offset]
  )
  React.useEffect(() => {
    if (error) toastApiError(error, "Load customers")
  }, [error])

  return (
    <div className="flex flex-col gap-4">
      <form
        className="relative max-w-md"
        onSubmit={(e) => {
          e.preventDefault()
          setParams(input ? { q: input } : {})
        }}
      >
        <HugeiconsIcon
          icon={Search01Icon}
          className="absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground"
        />
        <Input
          className="pl-8"
          placeholder="Search by email, external ref, or id prefix…"
          value={input}
          onChange={(e) => setInput(e.target.value)}
        />
      </form>
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
        onRowClick={(row) => navigate(`/customers/${row.id}`)}
        emptyMessage={q ? "No customers match." : "No customers yet."}
      />
    </div>
  )
}
