import { HugeiconsIcon } from "@hugeicons/react"
import { Download01Icon, Search01Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { useNavigate, useSearchParams } from "react-router-dom"
import { useMutation, useQuery } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"

import { CursorPager } from "@/components/cursor-pager"
import { useCursorPages } from "@/lib/cursor-pages"
import { DataTable } from "@/components/data-table"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import type { Customer } from "@/lib/api/generated/wire"
import { formatDate, shortId } from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { toastApiError } from "@/lib/toast"
import { adminQueries } from "@/lib/queries"

const PAGE = 50

const columns: ColumnDef<Customer, unknown>[] = [
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
      <span className="text-muted-foreground tabular-nums">
        {formatDate(row.original.created_at)}
      </span>
    ),
  },
  {
    header: "Last active",
    cell: ({ row }) => (
      <span className="text-muted-foreground tabular-nums">
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
  const pages = useCursorPages(q)
  const [input, setInput] = React.useState(q)
  const navigate = useNavigate()
  const exportCustomers = useMutation(adminMutations.exportCustomers())

  const {
    data,
    isPending: loading,
    isFetching,
  } = useQuery(adminQueries.customers(q, PAGE, pages.cursor))

  // Export walks every page of the current filter, not just the visible one.
  const exportCsv = async () => {
    try {
      const rows = await exportCustomers.mutateAsync(q)
      const csv = [
        ["id", "email", "created_at", "last_seen_at"].join(","),
        ...rows.map((r) =>
          [r.id, r.email, r.created_at, r.last_seen_at].map(csvEscape).join(",")
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
              placeholder="Search email or id…"
              value={input}
              onChange={(e) => setInput(e.target.value)}
            />
          </form>
          <Button
            size="sm"
            onClick={exportCsv}
            disabled={
              exportCustomers.isPending || loading || !data?.data.length
            }
          >
            <HugeiconsIcon icon={Download01Icon} className="size-4" />
            {exportCustomers.isPending ? "Exporting…" : "Export CSV"}
          </Button>
        </div>
      </div>
      <DataTable
        columns={columns}
        data={data?.data ?? []}
        loading={loading}
        onRowClick={(row) => navigate(`/customers/${row.id}`)}
        emptyMessage={q ? "No customers match." : "No customers yet."}
      />
      <CursorPager
        pages={pages}
        nextCursor={data?.next_cursor}
        busy={isFetching}
      />
    </div>
  )
}
