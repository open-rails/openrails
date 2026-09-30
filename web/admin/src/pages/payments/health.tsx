// Payments → Health (#1117): decline and rebill-failure health from the
// metrics API. Every tile and cell opens the matching attempt or cycle list.
import * as React from "react"
import { Link, useSearchParams } from "react-router-dom"
import { useQuery } from "@tanstack/react-query"

import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import type { MetricsResult } from "@/lib/api/metrics"
import { adminQueries } from "@/lib/queries"
import { cn } from "@/lib/utils"
import { formatMeasure } from "@/pages/dashboard/lib"
import { WidgetVizView } from "@/pages/dashboard/widget-viz"

import {
  checkQueries,
  checkRows,
  coverageQuery,
  coverageRows,
  FAILED_CATEGORIES,
  FIRST_FAILED,
  formatChange,
  listURL,
  missedQuery,
  missedRows,
  NEW_CARD,
  OWNERS,
  RANGES,
  reasonGroups,
  reasonsQuery,
  RECOVERY_ATTEMPTS,
  RECOVERY_DAYS,
  recoveryCurves,
  recoveryQueries,
  TILES,
  tileValue,
  trendQueries,
  type RecoveryCurve,
  type Scope,
  type Tile,
} from "./health-model"

const pct = (v: number | null) => (v === null ? "—" : formatMeasure(v, "ratio"))
const ownerLabel = (owner: string) =>
  OWNERS.find((o) => o.value === owner)?.label ?? owner

const metrics = adminQueries.widgetMetrics

export function PaymentHealthPage() {
  const [params, setParams] = useSearchParams()
  const providers = useQuery(adminQueries.paymentProviders()).data?.data
  const pspId = params.get("psp_id") ?? ""
  const provider = providers?.find((p) => p.id === pspId)
  const scope: Scope = {
    owner: params.get("owner") ?? "",
    psp: provider
      ? { id: provider.id, account: provider.account_id }
      : undefined,
    last: params.get("last") ?? "30d",
  }
  const set = (key: string, value: string) => {
    const p = new URLSearchParams(params)
    if (value) p.set(key, value)
    else p.delete(key)
    setParams(p)
  }
  const pspOfAccount = (account: string) =>
    providers?.find((p) => p.account_id === account)?.id

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold tracking-tight">
          Payment health
        </h1>
        <div className="flex flex-wrap items-center gap-3">
          <Picker
            label="Owner"
            value={scope.owner}
            options={OWNERS.map((o) => ({ value: o.value, label: o.label }))}
            onChange={(v) => set("owner", v)}
          />
          <Picker
            label="PSP"
            value={pspId}
            options={[
              { value: "", label: "All PSPs" },
              ...(providers ?? []).map((p) => ({
                value: p.id,
                label: `${p.rail} · ${p.account_id}`,
              })),
            ]}
            onChange={(v) => set("psp_id", v)}
          />
          <Picker
            label="Range"
            value={scope.last}
            options={RANGES.map((r) => ({ value: r.value, label: r.label }))}
            onChange={(v) => set("last", v === "30d" ? "" : v)}
          />
        </div>
      </div>

      <Tiles scope={scope} />
      <div className="grid gap-4 lg:grid-cols-2">
        <Trend
          title="New-card decline rate by week"
          query={trendQueries(scope).newCard}
        />
        <Trend
          title="Rebill first-attempt failure rate by week"
          query={trendQueries(scope).rebill}
        />
      </div>
      <Reasons scope={scope} />
      <Checks scope={scope} />
      <Recovery scope={scope} />
      <div className="grid gap-4 lg:grid-cols-2">
        <Missed scope={scope} pspOfAccount={pspOfAccount} />
        <Coverage scope={scope} pspOfAccount={pspOfAccount} />
      </div>
    </div>
  )
}

function Picker({
  label,
  value,
  options,
  onChange,
}: {
  label: string
  value: string
  options: { value: string; label: string }[]
  onChange: (value: string) => void
}) {
  // Base UI's select cannot hold "", so the empty option travels as "all".
  const items = options.map((o) => ({ ...o, value: o.value || "all" }))
  return (
    <Select
      items={items}
      value={value || "all"}
      onValueChange={(v) => onChange(!v || v === "all" ? "" : String(v))}
    >
      <SelectTrigger className="w-44" aria-label={label}>
        <SelectValue placeholder={label} />
      </SelectTrigger>
      <SelectContent>
        {items.map((o) => (
          <SelectItem key={o.value} value={o.value}>
            {o.label}
          </SelectItem>
        ))}
      </SelectContent>
    </Select>
  )
}

function Panel({
  title,
  children,
  className,
}: {
  title: string
  children: React.ReactNode
  className?: string
}) {
  return (
    <Card className={className}>
      <CardHeader>
        <CardTitle className="text-sm">{title}</CardTitle>
      </CardHeader>
      <CardContent>{children}</CardContent>
    </Card>
  )
}

function Loading() {
  return <Skeleton className="h-24 w-full" />
}

function Empty({ label = "Nothing in this range." }: { label?: string }) {
  return <p className="text-sm text-muted-foreground">{label}</p>
}

// --- tiles --------------------------------------------------------------------------

function Tiles({ scope }: { scope: Scope }) {
  return (
    <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-6">
      {TILES.map((tile) => (
        <TileCard key={tile.id} tile={tile} scope={scope} />
      ))}
    </div>
  )
}

function TileCard({ tile, scope }: { tile: Tile; scope: Scope }) {
  const { data, isPending } = useQuery(metrics(tile.query(scope)))
  const value = tileValue(data, tile)
  return (
    <Link
      to={listURL(tile.list, tile.listFilters, scope, data?.range)}
      className="rounded-xl focus-visible:ring-2 focus-visible:ring-ring focus-visible:outline-none"
    >
      <Card className="h-full gap-2 transition-colors hover:bg-muted/40">
        <CardHeader>
          <CardTitle className="text-xs font-medium tracking-wider text-muted-foreground uppercase">
            {tile.title}
          </CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-1">
          {isPending || !value ? (
            <Skeleton className="h-8 w-20" />
          ) : (
            <>
              <span className="text-2xl font-semibold tabular-nums">
                {formatMeasure(value.value, value.unit)}
              </span>
              <span
                className={cn(
                  "text-xs",
                  value.change === null || value.change === 0
                    ? "text-muted-foreground"
                    : value.worse
                      ? "text-failed"
                      : "text-settled"
                )}
              >
                {formatChange(value)}
              </span>
            </>
          )}
        </CardContent>
      </Card>
    </Link>
  )
}

// --- trends -----------------------------------------------------------------------------

function Trend({
  title,
  query,
}: {
  title: string
  query: ReturnType<typeof trendQueries>["newCard"]
}) {
  const { data, isPending } = useQuery(metrics(query))
  return (
    <Panel title={title}>
      <div className="h-56">
        {isPending || !data ? (
          <Loading />
        ) : (
          <WidgetVizView viz="line" result={data} query={query} />
        )}
      </div>
    </Panel>
  )
}

// --- reasons ------------------------------------------------------------------------------

function Reasons({ scope }: { scope: Scope }) {
  const { data, isPending } = useQuery(metrics(reasonsQuery(scope)))
  const groups = reasonGroups(data)
  const link = (filters: Record<string, string[]>) =>
    listURL("attempts", filters, scope, data?.range)
  return (
    <Panel title="Decline reasons">
      {isPending ? (
        <Loading />
      ) : groups.length === 0 ? (
        <Empty label="No declines in this range." />
      ) : (
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>Category / reason</TableHead>
              <TableHead>Code</TableHead>
              <TableHead className="text-right">Declines</TableHead>
              <TableHead className="text-right">Share</TableHead>
              <TableHead className="text-right">Previous period</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {groups.map((g) => (
              <React.Fragment key={g.category}>
                <TableRow className="bg-muted/30">
                  <TableCell colSpan={2} className="font-medium">
                    <Link
                      className="hover:underline"
                      to={link({ category: [g.category] })}
                    >
                      {g.category}
                    </Link>
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {g.count}
                  </TableCell>
                  <TableCell className="text-right tabular-nums">
                    {pct(g.share)}
                  </TableCell>
                  <TableCell />
                </TableRow>
                {g.rows.map((r) => (
                  <TableRow key={`${r.reason}|${r.code}`}>
                    <TableCell className="pl-6">
                      <Link
                        className="hover:underline"
                        to={link({
                          category: [r.category],
                          reason: r.reason ? [r.reason] : [],
                        })}
                      >
                        {r.reason || "—"}
                      </Link>
                    </TableCell>
                    <TableCell>
                      {r.code ? (
                        <Link
                          className="font-mono text-xs hover:underline"
                          to={link({
                            category: [r.category],
                            response_code: [r.code],
                          })}
                        >
                          {r.code}
                        </Link>
                      ) : (
                        "—"
                      )}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {r.count}
                    </TableCell>
                    <TableCell className="text-right tabular-nums">
                      {pct(r.share)}
                    </TableCell>
                    <TableCell
                      className={cn(
                        "text-right tabular-nums",
                        r.count > r.previous
                          ? "text-failed"
                          : "text-muted-foreground"
                      )}
                    >
                      {r.previous}
                    </TableCell>
                  </TableRow>
                ))}
              </React.Fragment>
            ))}
          </TableBody>
        </Table>
      )}
    </Panel>
  )
}

// --- AVS / CVV ------------------------------------------------------------------------------

function Checks({ scope }: { scope: Scope }) {
  const queries = checkQueries(scope)
  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <CheckTable
        title="New cards by AVS result"
        dimension="avs_result"
        query={queries.avs}
        scope={scope}
      />
      <CheckTable
        title="New cards by CVV result"
        dimension="cvv_result"
        query={queries.cvv}
        scope={scope}
      />
    </div>
  )
}

function CheckTable({
  title,
  dimension,
  query,
  scope,
}: {
  title: string
  dimension: "avs_result" | "cvv_result"
  query: ReturnType<typeof checkQueries>["avs"]
  scope: Scope
}) {
  const { data, isPending } = useQuery(metrics(query))
  const rows = checkRows(data)
  return (
    <Panel title={title}>
      {isPending ? (
        <Loading />
      ) : rows.length === 0 ? (
        <Empty />
      ) : (
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>Result</TableHead>
              <TableHead className="text-right">Attempts</TableHead>
              <TableHead className="text-right">Declined</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => (
              <TableRow key={r.result}>
                <TableCell className="font-mono text-xs">
                  {r.result || "none"}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  <Link
                    className="hover:underline"
                    to={listURL(
                      "attempts",
                      { ...NEW_CARD, [dimension]: r.result ? [r.result] : [] },
                      scope,
                      data?.range
                    )}
                  >
                    {r.attempts}
                  </Link>
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  <Link
                    className="hover:underline"
                    to={listURL(
                      "attempts",
                      {
                        ...NEW_CARD,
                        [dimension]: r.result ? [r.result] : [],
                        category: FAILED_CATEGORIES,
                      },
                      scope,
                      data?.range
                    )}
                  >
                    {pct(r.failureRate)}
                  </Link>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Panel>
  )
}

// --- recovery ---------------------------------------------------------------------------------

function Recovery({ scope }: { scope: Scope }) {
  const q = recoveryQueries(scope)
  const rate = useQuery(metrics(q.rate))
  const byAttempt = useQuery(metrics(q.byAttempt))
  const byDays = useQuery(metrics(q.byDays))
  const loading = rate.isPending || byAttempt.isPending || byDays.isPending
  return (
    <div className="grid gap-4 lg:grid-cols-2">
      <Panel title="Dunning recovery by retry (share of failed cycles)">
        {loading ? (
          <Loading />
        ) : (
          <CurveTable
            curves={recoveryCurves(
              rate.data,
              byAttempt.data,
              "recovery_attempt"
            )}
            steps={RECOVERY_ATTEMPTS.map((s) => `#${s}`)}
            scope={scope}
            range={rate.data?.range}
          />
        )}
      </Panel>
      <Panel title="Dunning recovery by days since the first failure">
        {loading ? (
          <Loading />
        ) : (
          <CurveTable
            curves={recoveryCurves(rate.data, byDays.data, "days_to_recover")}
            steps={RECOVERY_DAYS.map((d) => `≤ ${d}d`)}
            scope={scope}
            range={rate.data?.range}
          />
        )}
      </Panel>
    </div>
  )
}

function CurveTable({
  curves,
  steps,
  scope,
  range,
}: {
  curves: RecoveryCurve[]
  steps: string[]
  scope: Scope
  range?: MetricsResult["range"]
}) {
  if (curves.length === 0) return <Empty label="No recovered cycles." />
  return (
    <Table>
      <TableHeader>
        <TableRow className="hover:bg-transparent">
          <TableHead>Owner</TableHead>
          {steps.map((s) => (
            <TableHead key={s} className="text-right">
              {s}
            </TableHead>
          ))}
        </TableRow>
      </TableHeader>
      <TableBody>
        {curves.map((c) => (
          <TableRow key={c.owner}>
            <TableCell>
              <Link
                className="hover:underline"
                to={listURL(
                  "cycles",
                  {
                    owner: [c.owner],
                    first_outcome: FIRST_FAILED,
                    outcome: ["collected"],
                  },
                  scope,
                  range
                )}
              >
                {ownerLabel(c.owner)}
              </Link>
            </TableCell>
            {c.points.map((p, i) => (
              <TableCell key={steps[i]} className="text-right tabular-nums">
                {pct(p)}
              </TableCell>
            ))}
          </TableRow>
        ))}
      </TableBody>
    </Table>
  )
}

// --- missed and coverage ------------------------------------------------------------------------

function Missed({
  scope,
  pspOfAccount,
}: {
  scope: Scope
  pspOfAccount: (account: string) => string | undefined
}) {
  const { data, isPending } = useQuery(metrics(missedQuery(scope)))
  const rows = missedRows(data)
  return (
    <Panel title="Missed rebills">
      {isPending ? (
        <Loading />
      ) : rows.length === 0 ? (
        <Empty label="No missed rebills in this range." />
      ) : (
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>Reason</TableHead>
              <TableHead>PSP</TableHead>
              <TableHead className="text-right">Cycles</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => (
              <TableRow key={`${r.reason}|${r.account}`}>
                <TableCell>{r.reason}</TableCell>
                <TableCell className="text-xs">{r.account}</TableCell>
                <TableCell className="text-right tabular-nums">
                  <Link
                    className="hover:underline"
                    to={listURL(
                      "cycles",
                      { first_outcome: ["missed"], miss_reason: [r.reason] },
                      scope,
                      data?.range,
                      pspOfAccount(r.account)
                    )}
                  >
                    {r.count}
                  </Link>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Panel>
  )
}

function Coverage({
  scope,
  pspOfAccount,
}: {
  scope: Scope
  pspOfAccount: (account: string) => string | undefined
}) {
  const { data, isPending } = useQuery(metrics(coverageQuery(scope)))
  const rows = coverageRows(data)
  const link = (account: string, via: string) =>
    listURL(
      "attempts",
      { source: ["provider_schedule"], observed_via: [via] },
      scope,
      data?.range,
      pspOfAccount(account)
    )
  return (
    <Panel title="Provider charges seen by webhook">
      {isPending ? (
        <Loading />
      ) : rows.length === 0 ? (
        <Empty label="No provider-scheduled charges in this range." />
      ) : (
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>PSP</TableHead>
              <TableHead className="text-right">Webhook</TableHead>
              <TableHead className="text-right">Pull</TableHead>
              <TableHead className="text-right">Coverage</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => (
              <TableRow key={r.account}>
                <TableCell className="text-xs">{r.account}</TableCell>
                <TableCell className="text-right tabular-nums">
                  <Link
                    className="hover:underline"
                    to={link(r.account, "webhook")}
                  >
                    {r.webhook}
                  </Link>
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  <Link
                    className="hover:underline"
                    to={link(r.account, "pull")}
                  >
                    {r.pull}
                  </Link>
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {pct(r.coverage)}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </Panel>
  )
}
