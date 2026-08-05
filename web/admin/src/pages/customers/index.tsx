import { HugeiconsIcon } from "@hugeicons/react"
import { Download01Icon, Search01Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { useNavigate, useSearchParams } from "react-router-dom"
import { useQuery } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"

import { DataTable } from "@/components/data-table"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { listCustomers } from "@/lib/api/endpoints"
import type { CustomerSummary } from "@/lib/api/types"
import { formatDate, shortId } from "@/lib/format"
import { toastApiError } from "@/lib/toast"
import { adminQueries } from "@/lib/queries"

const PAGE = 50
const EXPORT_PAGE = 200

const columns: ColumnDef<CustomerSummary, unknown>[] = [
  {
    header: "Email",
    cell: ({ row }) =>
      row.original.email ? (
        <span className="font-medium">{row.original.email}</span>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    header: "External ref",
    cell: ({ row }) =>
      row.original.subject ? (
        <span className="block max-w-64 truncate" title={row.original.subject}>
          {row.original.subject}
        </span>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  {
    header: "Customer",
    cell: ({ row }) => (
      <span className="text-xs text-muted-foreground">
        {shortId(row.original.id, 13)}
      </span>
    ),
  },
  {
    header: "Created",
    cell: ({ row }) => (
      <span className="text-muted-foreground">
        {formatDate(row.original.created_at)}
      </span>
    ),
  },
  {
    header: "Last active",
    cell: ({ row }) => (
      <span className="text-muted-foreground">
        {formatDate(row.original.last_seen_at)}
      </span>
    ),
  },
]

function csvEscape(v: unknown): string {
  const s = v == null ? "" : String(v)
  return /[",\n]/.test(s) ? '"' + s.replaceAll('"', '""') + '"' : s
}

export function CustomersPage() {
  const [params, setParams] = useSearchParams()
  const q = params.get("q") ?? ""
  const offset = Number(params.get("offset") ?? 0)
  const [input, setInput] = React.useState(q)
  const [exporting, setExporting] = React.useState(false)
  const navigate = useNavigate()

  const { data, isPending: loading } = useQuery(
    adminQueries.customers(q, PAGE, offset)
  )

  // Export walks every page of the current filter, not just the visible one.
  const exportCsv = async () => {
    setExporting(true)
    try {
      const rows: CustomerSummary[] = []
      let cursor = 0
      for (;;) {
        const page = await listCustomers(q, EXPORT_PAGE, cursor)
        rows.push(...page.data)
        if (rows.length >= page.total || page.data.length === 0) break
        cursor += EXPORT_PAGE
      }
      const csv = [
        ["id", "external_ref", "email", "created_at", "last_seen_at"].join(","),
        ...rows.map((r) =>
          [r.id, r.subject, r.email, r.created_at, r.last_seen_at]
            .map(csvEscape)
            .join(",")
        ),
      ].join("\n")
      const url = URL.createObjectURL(
        new Blob([csv], { type: "text/csv;charset=utf-8" })
      )
      const a = document.createElement("a")
      a.href = url
      a.download = `customers-${new Date().toISOString().slice(0, 10)}.csv`
      a.click()
      URL.revokeObjectURL(url)
    } catch (err) {
      toastApiError(err, "Export customers")
    } finally {
      setExporting(false)
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold tracking-tight">Customers</h1>
        <div className="flex items-center gap-3">
          <form
            className="relative"
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
              className="w-64 pl-8"
              placeholder="Search email, ref, or id…"
              value={input}
              onChange={(e) => setInput(e.target.value)}
            />
          </form>
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
