// Payments → Attempts and Rebill cycles (#1117): the #1116 lists and details.
// A list's filters are its URL's query parameters, as the API names them, so
// the health page links straight into a filtered list.
import { HugeiconsIcon } from "@hugeicons/react"
import { ArrowLeft01Icon, Cancel01Icon } from "@hugeicons/core-free-icons"
import { Link, useNavigate, useParams, useSearchParams } from "react-router-dom"
import { useQuery } from "@tanstack/react-query"
import type { ColumnDef } from "@tanstack/react-table"

import { CursorPager } from "@/components/cursor-pager"
import { DataTable } from "@/components/data-table"
import { Fact } from "@/components/fact-card"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  ATTEMPT_FILTERS,
  CYCLE_FILTERS,
  filtersFrom,
  type AttemptFilters,
  type CycleFilters,
} from "@/lib/api/endpoints"
import type { PaymentAttempt, RebillCycle } from "@/lib/api/generated/wire"
import { useUrlCursor } from "@/hooks/use-cursor-paging"
import {
  formatCard,
  formatDate,
  formatNativeAmount,
  shortId,
} from "@/lib/format"
import { adminQueries } from "@/lib/queries"

const PAGE = 50

export function Outcome({ category }: { category: string }) {
  return (
    <StatusBadge status={category === "approved" ? "approved" : "declined"} />
  )
}

const attemptColumns: ColumnDef<PaymentAttempt, unknown>[] = [
  {
    header: "Attempted",
    cell: ({ row }) => (
      <span className="text-muted-foreground tabular-nums">
        {formatDate(row.original.attempted_at)}
      </span>
    ),
  },
  { header: "Kind", cell: ({ row }) => row.original.kind },
  {
    header: "Outcome",
    cell: ({ row }) => <Outcome category={row.original.category} />,
  },
  {
    header: "Reason",
    cell: ({ row }) =>
      row.original.category === "approved" ? (
        <span className="text-muted-foreground">—</span>
      ) : (
        <span>
          {row.original.reason || row.original.category}
          {row.original.response_code ? (
            <span className="ml-1 font-mono text-xs text-muted-foreground">
              {row.original.response_code}
            </span>
          ) : null}
        </span>
      ),
  },
  { header: "Owner", cell: ({ row }) => row.original.owner },
  { header: "Card", cell: ({ row }) => formatCard(row.original.card) },
  {
    header: "Amount",
    cell: ({ row }) =>
      row.original.currency ? (
        <span className="tabular-nums">
          {formatNativeAmount(row.original.amount, row.original.currency)}
        </span>
      ) : (
        <span className="text-muted-foreground">—</span>
      ),
  },
  { header: "Rail", cell: ({ row }) => row.original.rail },
]

function FilterChips({ keys }: { keys: readonly string[] }) {
  const [params, setParams] = useSearchParams()
  const active = keys.filter((k) => params.get(k))
  if (active.length === 0) return null
  const drop = (key?: string) => {
    const p = new URLSearchParams(params)
    for (const k of key ? [key] : active) p.delete(k)
    p.delete("cursor")
    setParams(p)
  }
  return (
    <div className="flex flex-wrap items-center gap-2 text-sm">
      {active.map((k) => (
        <span
          key={k}
          className="inline-flex items-center gap-1 rounded-md bg-muted py-0.5 pr-1 pl-2"
        >
          <span className="text-muted-foreground">{k}</span>
          <span className="font-medium">{params.get(k)}</span>
          <Button
            variant="ghost"
            size="icon"
            className="size-5"
            aria-label={`Clear ${k}`}
            onClick={() => drop(k)}
          >
            <HugeiconsIcon icon={Cancel01Icon} className="size-3" />
          </Button>
        </span>
      ))}
      <Button variant="ghost" size="sm" onClick={() => drop()}>
        Clear all
      </Button>
    </div>
  )
}

export function AttemptsPage() {
  const [params] = useSearchParams()
  const paging = useUrlCursor()
  const navigate = useNavigate()
  const filters: AttemptFilters = filtersFrom(params, ATTEMPT_FILTERS)
  const { data, isPending, isFetching } = useQuery(
    adminQueries.attempts(filters, PAGE, paging.cursor)
  )
  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-2xl font-semibold tracking-tight">
        Payment attempts
      </h1>
      <FilterChips keys={ATTEMPT_FILTERS} />
      <DataTable
        columns={attemptColumns}
        data={data?.data ?? []}
        loading={isPending}
        onRowClick={(row) => navigate(`/payments/attempts/${row.id}`)}
        emptyMessage="No attempts match."
      />
      <CursorPager
        pages={paging}
        nextCursor={data?.next_cursor}
        busy={isFetching}
      />
    </div>
  )
}

function Back() {
  const navigate = useNavigate()
  return (
    <Button
      variant="ghost"
      size="icon"
      onClick={() => navigate(-1)}
      aria-label="Back"
    >
      <HugeiconsIcon icon={ArrowLeft01Icon} className="size-4" />
    </Button>
  )
}

function To({ to, children }: { to: string; children: string }) {
  return (
    <Link className="text-xs underline-offset-2 hover:underline" to={to}>
      {children}
    </Link>
  )
}

export function AttemptDetailPage() {
  const { id = "" } = useParams()
  const { data: a, isPending } = useQuery(adminQueries.attempt(id))
  if (isPending)
    return <p className="text-sm text-muted-foreground">Loading…</p>
  if (!a)
    return <p className="text-sm text-muted-foreground">Attempt not found.</p>
  const answer = [
    ["Response", [a.response_code, a.response_text].filter(Boolean).join(" ")],
    ["Issuer", [a.issuer_code, a.issuer_text].filter(Boolean).join(" ")],
    ["AVS", a.avs_result],
    ["CVV", a.cvv_result],
    ["Action", a.action],
    ["Token", a.token_type],
    ["BIN", a.card_bin],
    ["Transaction", a.transaction_id],
    ["Observed via", `${a.source} · ${a.observed_via}`],
    ["Enriched", a.enriched_at ? formatDate(a.enriched_at) : ""],
  ] as const
  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-3">
        <Back />
        <div>
          <h2 className="flex items-center gap-2 text-sm">
            {a.id} <Outcome category={a.category} />
          </h2>
          <p className="text-xs text-muted-foreground">
            {a.kind} · {a.owner} · {a.rail} · {formatDate(a.attempted_at)}
          </p>
        </div>
      </div>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Fact label="Outcome">
          {a.category === "approved"
            ? "approved"
            : `${a.category}${a.reason ? ` · ${a.reason}` : ""}`}
        </Fact>
        <Fact label="Amount">
          {a.currency ? formatNativeAmount(a.amount, a.currency) : "—"}
        </Fact>
        <Fact label="Card">
          {formatCard(a.card)} ({a.card_entry})
        </Fact>
        <Fact label="Customer">
          <To to={`/customers/${a.customer_id}`}>
            {shortId(a.customer_id, 16)}
          </To>
        </Fact>
        <Fact label="Subscription">
          {a.subscription_id ? (
            <To to={`/subscriptions/${a.subscription_id}`}>
              {shortId(a.subscription_id, 16)}
            </To>
          ) : (
            "—"
          )}
        </Fact>
        <Fact label="Rebill cycle">
          {a.cycle_id ? (
            <To to={`/payments/cycles/${a.cycle_id}`}>
              {shortId(a.cycle_id, 16)}
            </To>
          ) : (
            "—"
          )}
        </Fact>
        <Fact label="Payment">
          {a.payment_id ? (
            <To to={`/payments/${a.payment_id}`}>
              {shortId(a.payment_id, 16)}
            </To>
          ) : (
            "—"
          )}
        </Fact>
        <Fact label="Checkout">
          {a.checkout_id ? (
            <To to={`/payments/attempts?checkout_id=${a.checkout_id}`}>
              {`${a.checkout_target || "checkout"} attempts`}
            </To>
          ) : (
            "—"
          )}
        </Fact>
      </div>
      <Card>
        <CardHeader>
          <CardTitle className="text-sm">PSP answer</CardTitle>
        </CardHeader>
        <CardContent>
          <dl className="grid gap-x-6 gap-y-2 text-sm sm:grid-cols-2">
            {answer.map(([label, value]) => (
              <div key={label} className="flex gap-2">
                <dt className="w-28 shrink-0 text-muted-foreground">{label}</dt>
                <dd className="font-mono text-xs break-all">{value || "—"}</dd>
              </div>
            ))}
          </dl>
        </CardContent>
      </Card>
    </div>
  )
}

// --- rebill cycles ----------------------------------------------------------------------

const cycleColumns: ColumnDef<RebillCycle, unknown>[] = [
  {
    header: "Due",
    cell: ({ row }) => (
      <span className="text-muted-foreground tabular-nums">
        {formatDate(row.original.due_at)}
      </span>
    ),
  },
  { header: "Owner", cell: ({ row }) => row.original.owner },
  {
    header: "First outcome",
    cell: ({ row }) =>
      row.original.miss_reason
        ? `missed · ${row.original.miss_reason}`
        : row.original.first_outcome,
  },
  {
    header: "Outcome",
    cell: ({ row }) => <StatusBadge status={row.original.outcome} />,
  },
  {
    header: "Recovered by",
    cell: ({ row }) => row.original.recovered_by || "—",
  },
  {
    header: "Amount",
    cell: ({ row }) => (
      <span className="tabular-nums">
        {formatNativeAmount(row.original.amount, row.original.currency)}
      </span>
    ),
  },
  { header: "Rail", cell: ({ row }) => row.original.rail },
]

export function CyclesPage() {
  const [params] = useSearchParams()
  const paging = useUrlCursor()
  const navigate = useNavigate()
  const filters: CycleFilters = filtersFrom(params, CYCLE_FILTERS)
  const { data, isPending, isFetching } = useQuery(
    adminQueries.cycles(filters, PAGE, paging.cursor)
  )
  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-2xl font-semibold tracking-tight">Rebill cycles</h1>
      <FilterChips keys={CYCLE_FILTERS} />
      <DataTable
        columns={cycleColumns}
        data={data?.data ?? []}
        loading={isPending}
        onRowClick={(row) => navigate(`/payments/cycles/${row.id}`)}
        emptyMessage="No rebill cycles match."
      />
      <CursorPager
        pages={paging}
        nextCursor={data?.next_cursor}
        busy={isFetching}
      />
    </div>
  )
}

export function CycleDetailPage() {
  const { id = "" } = useParams()
  const navigate = useNavigate()
  const { data: c, isPending } = useQuery(adminQueries.cycle(id))
  if (isPending)
    return <p className="text-sm text-muted-foreground">Loading…</p>
  if (!c)
    return (
      <p className="text-sm text-muted-foreground">Rebill cycle not found.</p>
    )
  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-3">
        <Back />
        <div>
          <h2 className="flex items-center gap-2 text-sm">
            {c.id} <StatusBadge status={c.outcome} />
          </h2>
          <p className="text-xs text-muted-foreground">
            {c.owner} · {c.rail} · due {formatDate(c.due_at)}
          </p>
        </div>
      </div>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Fact label="Amount">{formatNativeAmount(c.amount, c.currency)}</Fact>
        <Fact label="First outcome">
          {c.miss_reason ? `missed · ${c.miss_reason}` : c.first_outcome}
        </Fact>
        <Fact label="Collected">
          {c.collected_at
            ? `${formatDate(c.collected_at)}${c.recovered_by ? ` · ${c.recovered_by}` : ""}`
            : "—"}
        </Fact>
        <Fact label={c.outcome === "open" ? "Closes" : "Closed"}>
          {formatDate(c.closes_at)}
        </Fact>
        <Fact label="Subscription">
          <To to={`/subscriptions/${c.subscription_id}`}>
            {shortId(c.subscription_id, 16)}
          </To>
        </Fact>
        <Fact label="Customer">
          <To to={`/customers/${c.customer_id}`}>
            {shortId(c.customer_id, 16)}
          </To>
        </Fact>
      </div>
      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Attempts</CardTitle>
        </CardHeader>
        <CardContent>
          <DataTable
            columns={attemptColumns}
            data={c.attempts ?? []}
            onRowClick={(row) => navigate(`/payments/attempts/${row.id}`)}
            emptyMessage={
              c.miss_reason ? "Never attempted." : "No attempts yet."
            }
          />
        </CardContent>
      </Card>
    </div>
  )
}
