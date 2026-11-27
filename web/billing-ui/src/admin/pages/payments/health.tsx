import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { STAT_TILES_6 } from "#orck/admin/lib/tile-grids"
import { useAdminServices } from "#orck/admin/context"
// Payments → Health: decline and rebill-failure health from the metrics API.
// Every tile and cell opens the matching attempt or cycle list.
import { useMerchantConfig } from "#orck/admin/lib/capabilities"
import * as React from "react"
import { Link, useSearchParams } from "#orck/admin/router"
import { useQuery } from "@tanstack/react-query"

import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "#orck/admin/components/ui/card"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "#orck/admin/components/ui/select"
import { Skeleton } from "#orck/admin/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "#orck/admin/components/ui/table"
import type { MetricsResult } from "#orck/admin/lib/api/metrics"

import { cn } from "#orck/admin/lib/utils"
import { formatMeasure } from "#orck/admin/pages/dashboard/lib"
import { WidgetVizView } from "#orck/admin/pages/dashboard/widget-viz"

import {
  checkQueries,
  checkRows,
  coverageQuery,
  coverageRows,
  FAILED_OUTCOMES,
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

export function PaymentHealthPage() {
  const { text: adminText } = useAdminUI()

  const { adminQueries } = useAdminServices()

  const [params, setParams] = useSearchParams()
  const config = useMerchantConfig()
  const psps = useQuery({ ...adminQueries.psps(), enabled: config }).data?.data
  const pspId = params.get("psp_id") ?? ""
  const psp = psps?.find((p) => p.id === pspId)
  const scope: Scope = {
    owner: params.get("owner") ?? "",
    psp: psp ? { id: psp.id } : undefined,
    last: params.get("last") ?? "30d",
  }
  const set = (key: string, value: string) => {
    const p = new URLSearchParams(params)
    if (value) p.set(key, value)
    else p.delete(key)
    setParams(p)
  }
  const pspKey = (id: string) => psps?.find((p) => p.id === id)?.key ?? id

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold tracking-tight">
          <AdminMessage id={"Payment health"} />
        </h1>
        <div className="flex flex-wrap items-center gap-3">
          <Picker
            label={adminText("Owner")}
            value={scope.owner}
            options={OWNERS.map((o) => ({ value: o.value, label: o.label }))}
            onChange={(v) => set("owner", v)}
          />
          <Picker
            label={adminText("PSP")}
            value={pspId}
            options={[
              { value: "", label: "All PSPs" },
              ...(psps ?? []).map((p) => ({
                value: p.id,
                label: `${p.key} · ${p.rail}`,
              })),
            ]}
            onChange={(v) => set("psp_id", v)}
          />
          <Picker
            label={adminText("Range")}
            value={scope.last}
            options={RANGES.map((r) => ({ value: r.value, label: r.label }))}
            onChange={(v) => set("last", v === "30d" ? "" : v)}
          />
        </div>
      </div>

      <Tiles scope={scope} />
      <div className="grid gap-4 @min-[48rem]/page:grid-cols-2">
        <Trend
          title={adminText("New-card authorization rate by week")}
          query={trendQueries(scope).newCard}
        />
        <Trend
          title={adminText("First-try rebill approval by week")}
          query={trendQueries(scope).rebill}
        />
      </div>
      <Reasons scope={scope} />
      <Checks scope={scope} />
      <Recovery scope={scope} />
      <div className="grid gap-4 @min-[48rem]/page:grid-cols-2">
        <Missed scope={scope} pspKey={pspKey} />
        <Coverage scope={scope} pspKey={pspKey} />
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

function Tiles({ scope }: { scope: Scope }) {
  return (
    <div className={STAT_TILES_6}>
      {TILES.map((tile) => (
        <TileCard key={tile.id} tile={tile} scope={scope} />
      ))}
    </div>
  )
}

function TileCard({ tile, scope }: { tile: Tile; scope: Scope }) {
  const metrics = useAdminServices().adminQueries.widgetMetrics

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

function Trend({
  title,
  query,
}: {
  title: string
  query: ReturnType<typeof trendQueries>["newCard"]
}) {
  const metrics = useAdminServices().adminQueries.widgetMetrics

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

function Reasons({ scope }: { scope: Scope }) {
  const { text: adminText } = useAdminUI()

  const metrics = useAdminServices().adminQueries.widgetMetrics

  const { data, isPending } = useQuery(metrics(reasonsQuery(scope)))
  const groups = reasonGroups(data)
  const link = (filters: Record<string, string[]>) =>
    listURL("attempts", filters, scope, data?.range)
  return (
    <Panel title={adminText("Decline reasons")}>
      {isPending ? (
        <Loading />
      ) : groups.length === 0 ? (
        <Empty label={adminText("No declines in this range.")} />
      ) : (
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>
                <AdminMessage id={"Category / reason"} />
              </TableHead>
              <TableHead>
                <AdminMessage id={"Code"} />
              </TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"Declines"} />
              </TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"Share"} />
              </TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"Previous period"} />
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {groups.map((g) => (
              <React.Fragment key={g.cause}>
                <TableRow className="bg-muted/30">
                  <TableCell colSpan={2} className="font-medium">
                    <Link
                      className="hover:underline"
                      to={link({ decline_cause: [g.cause] })}
                    >
                      {g.cause}
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
                          decline_cause: [r.cause],
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
                            decline_cause: [r.cause],
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

function Checks({ scope }: { scope: Scope }) {
  const { text: adminText } = useAdminUI()

  const queries = checkQueries(scope)
  return (
    <div className="grid gap-4 @min-[48rem]/page:grid-cols-2">
      <CheckTable
        title={adminText("New cards by AVS result")}
        dimension="avs_result"
        query={queries.avs}
        scope={scope}
      />
      <CheckTable
        title={adminText("New cards by CVV result")}
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
  const metrics = useAdminServices().adminQueries.widgetMetrics

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
              <TableHead>
                <AdminMessage id={"Result"} />
              </TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"Attempts"} />
              </TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"Declined"} />
              </TableHead>
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
                        outcome: FAILED_OUTCOMES,
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

function Recovery({ scope }: { scope: Scope }) {
  const { text: adminText } = useAdminUI()

  const metrics = useAdminServices().adminQueries.widgetMetrics

  const q = recoveryQueries(scope)
  const rate = useQuery(metrics(q.rate))
  const byAttempt = useQuery(metrics(q.byAttempt))
  const byDays = useQuery(metrics(q.byDays))
  const loading = rate.isPending || byAttempt.isPending || byDays.isPending
  return (
    <div className="grid gap-4 @min-[48rem]/page:grid-cols-2">
      <Panel
        title={adminText("Dunning recovery by retry (share of failed cycles)")}
      >
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
      <Panel
        title={adminText("Dunning recovery by days since the first failure")}
      >
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
  const { text: adminText } = useAdminUI()

  if (curves.length === 0)
    return <Empty label={adminText("No recovered cycles.")} />
  return (
    <Table>
      <TableHeader>
        <TableRow className="hover:bg-transparent">
          <TableHead>
            <AdminMessage id={"Owner"} />
          </TableHead>
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

function Missed({
  scope,
  pspKey,
}: {
  scope: Scope
  pspKey: (id: string) => string
}) {
  const { text: adminText } = useAdminUI()

  const metrics = useAdminServices().adminQueries.widgetMetrics

  const { data, isPending } = useQuery(metrics(missedQuery(scope)))
  const rows = missedRows(data)
  return (
    <Panel title={adminText("Missed rebills")}>
      {isPending ? (
        <Loading />
      ) : rows.length === 0 ? (
        <Empty label={adminText("No missed rebills in this range.")} />
      ) : (
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>
                <AdminMessage id={"Reason"} />
              </TableHead>
              <TableHead>PSP</TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"Cycles"} />
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => (
              <TableRow key={`${r.reason}|${r.psp}`}>
                <TableCell>{r.reason}</TableCell>
                <TableCell className="text-xs">{pspKey(r.psp)}</TableCell>
                <TableCell className="text-right tabular-nums">
                  <Link
                    className="hover:underline"
                    to={listURL(
                      "cycles",
                      { first_outcome: ["missed"], miss_reason: [r.reason] },
                      scope,
                      data?.range,
                      r.psp || undefined
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
  pspKey,
}: {
  scope: Scope
  pspKey: (id: string) => string
}) {
  const { text: adminText } = useAdminUI()

  const metrics = useAdminServices().adminQueries.widgetMetrics

  const { data, isPending } = useQuery(metrics(coverageQuery(scope)))
  const rows = coverageRows(data)
  const link = (psp: string, via: string) =>
    listURL(
      "attempts",
      { source: ["provider_schedule"], observed_via: [via] },
      scope,
      data?.range,
      psp || undefined
    )
  return (
    <Panel title={adminText("Provider charges seen by webhook")}>
      {isPending ? (
        <Loading />
      ) : rows.length === 0 ? (
        <Empty
          label={adminText("No provider-scheduled charges in this range.")}
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>PSP</TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"Webhook"} />
              </TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"Pull"} />
              </TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"Coverage"} />
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {rows.map((r) => (
              <TableRow key={r.psp}>
                <TableCell className="text-xs">{pspKey(r.psp)}</TableCell>
                <TableCell className="text-right tabular-nums">
                  <Link className="hover:underline" to={link(r.psp, "webhook")}>
                    {r.webhook}
                  </Link>
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  <Link className="hover:underline" to={link(r.psp, "pull")}>
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
