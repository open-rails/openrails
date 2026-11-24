import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
// Dashboard › Reports: an explorer that charts any measure in the metrics
// registry with its dimensions, the daily subscription report and the
// monthly renewal report over the dashboard's range, each with a CSV
// download.
import { HugeiconsIcon } from "@hugeicons/react"
import { Download01Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { Navigate } from "#orck/admin/router"
import { useQuery } from "@tanstack/react-query"

import { Button } from "#orck/admin/components/ui/button"
import {
  Card,
  CardAction,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle,
} from "#orck/admin/components/ui/card"
import { Skeleton } from "#orck/admin/components/ui/skeleton"
import {
  Table,
  TableBody,
  TableCell,
  TableFooter,
  TableHead,
  TableHeader,
  TableRow,
} from "#orck/admin/components/ui/table"
import { ApiError } from "#orck/admin/lib/api/client"
import { useAccess } from "#orck/admin/lib/capabilities"
import { downloadCsv } from "#orck/admin/lib/csv"

import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "#orck/admin/components/ui/select"
import { CHART_VIZ, type WidgetViz } from "#orck/admin/lib/api/metrics"

import { draftOf, QueryBuilder, queryOf, type Draft } from "./query-builder"
import { rangeDates, widgetQuery, type DashboardRange } from "./range"
import { RangePicker } from "./range-picker"
import { measureLabel, useRegistry } from "./registry"
import { useDashboardRange } from "./use-range"
import {
  MAX_DAYS,
  churnQuery,
  dailyCsv,
  dailyQuery,
  dailyRows,
  monthlyCsv,
  monthlyRows,
  rangeDays,
  renewalsQuery,
  resultCsv,
} from "./reports-model"
import { WidgetVizView } from "./widget-viz"

const utcDay = (date: string) =>
  new Date(`${date}T00:00:00Z`).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
    timeZone: "UTC",
  })

const utcMonth = (month: string) =>
  new Date(`${month}-01T00:00:00Z`).toLocaleDateString(undefined, {
    month: "short",
    year: "numeric",
    timeZone: "UTC",
  })

const signed = (n: number) =>
  n > 0 ? `+${n.toLocaleString()}` : n.toLocaleString()

const percent = (ratio: number | null) =>
  ratio === null
    ? "—"
    : `${(ratio * 100).toLocaleString(undefined, { maximumFractionDigits: 2 })}%`

const suffix = (range: DashboardRange) => {
  const { from, to } = rangeDates(range)
  return `${from}-to-${to}`
}

export function ReportsPage() {
  const access = useAccess()
  const [range] = useDashboardRange()
  if (!access) return null
  if (!access.metrics) return <Navigate to="/" replace />
  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h1 className="text-2xl font-semibold tracking-tight">
          <AdminMessage id={"Reports"} />
        </h1>
        <RangePicker />
      </div>
      <Explorer range={range} />
      <DailyReport range={range} />
      <MonthlyReport range={range} />
    </div>
  )
}

function failure(error: unknown) {
  if (error instanceof ApiError && error.isPermissionDenied)
    return "Your role cannot read metrics."
  return error instanceof Error ? error.message : "The report failed to load."
}

function ReportCard({
  title,
  description,
  onDownload,
  canDownload,
  children,
}: {
  title: string
  description: string
  onDownload: () => void
  canDownload: boolean
  children: React.ReactNode
}) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>{title}</CardTitle>
        <CardDescription>{description}</CardDescription>
        <CardAction>
          <Button
            size="sm"
            variant="outline"
            onClick={onDownload}
            disabled={!canDownload}
          >
            <HugeiconsIcon icon={Download01Icon} className="size-4" />
            <AdminMessage id={"Download CSV"} />
          </Button>
        </CardAction>
      </CardHeader>
      <CardContent className="overflow-x-auto">{children}</CardContent>
    </Card>
  )
}

function ReportState({
  loading,
  error,
  retry,
  empty,
}: {
  loading: boolean
  error: unknown
  retry: () => void
  empty: boolean
}) {
  if (loading)
    return (
      <div className="grid gap-2" aria-busy>
        {Array.from({ length: 5 }).map((_, i) => (
          <Skeleton key={i} className="h-6 w-full" />
        ))}
      </div>
    )
  if (error)
    return (
      <div
        role="alert"
        className="flex flex-wrap items-center gap-3 text-sm text-destructive"
      >
        {failure(error)}
        <Button size="sm" variant="outline" onClick={retry}>
          <AdminMessage id={"Retry"} />
        </Button>
      </div>
    )
  if (empty)
    return (
      <p className="py-6 text-center text-sm text-muted-foreground">
        <AdminMessage id={"Nothing in this range. Pick a wider range."} />
      </p>
    )
  return null
}

const EXPLORE_VIZ = CHART_VIZ.filter((v) => v !== "kpi" && v !== "stat")

// Explorer charts any measure of the registry over the page's range, split
// by its dimensions, with its table as CSV.
function Explorer({ range }: { range: DashboardRange }) {
  const { id: adminId, text: adminText } = useAdminUI()
  const { adminQueries } = useAdminServices()
  const registry = useRegistry()
  const [draft, setDraft] = React.useState<Draft>(() =>
    draftOf({
      measures: ["cash"],
      by: ["time"],
      grain: "auto",
      range: { last: "30d" },
    })
  )
  const [viz, setViz] = React.useState<WidgetViz>("bar")
  const query = widgetQuery(
    {
      id: "explore",
      title: "",
      viz,
      query: queryOf(draft, { measures: [], range: { last: "30d" } }),
      grid: { x: 0, y: 0, w: 1, h: 1 },
      fixed_range: false,
    },
    { range }
  )
  const result = useQuery(adminQueries.widgetMetrics(query))
  const drawn: WidgetViz =
    draft.time || viz === "table" ? viz : viz === "donut" ? viz : "table"
  return (
    <ReportCard
      title={adminText("Explore")}
      description={adminText(
        "Chart any measure, split by its dimensions, over the range picked."
      )}
      canDownload={!!result.data?.rows.length}
      onDownload={() => {
        if (!result.data) return
        const csv = resultCsv(result.data)
        downloadCsv(
          `${draft.measures.join("-")}-${suffix(range)}.csv`,
          csv.header,
          csv.rows
        )
      }}
    >
      <div className="flex flex-col gap-4">
        <div className="grid gap-4 lg:grid-cols-[1fr_12rem]">
          <QueryBuilder draft={draft} onChange={setDraft} registry={registry} />
          <div className="flex flex-col gap-1.5">
            <label htmlFor={adminId("explore-viz")} className="text-sm">
              <AdminMessage id={"Chart type"} />
            </label>
            <Select
              items={EXPLORE_VIZ.map((v) => ({
                value: v,
                label: measureLabel(v),
              }))}
              value={viz}
              onValueChange={(v) => v && setViz(v as WidgetViz)}
            >
              <SelectTrigger id={adminId("explore-viz")} className="w-full">
                <SelectValue />
              </SelectTrigger>
              <SelectContent>
                {EXPLORE_VIZ.map((v) => (
                  <SelectItem key={v} value={v}>
                    {measureLabel(v)}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
        </div>
        <div className="h-72">
          {result.data ? (
            <WidgetVizView
              viz={drawn}
              result={result.data}
              query={query}
              registry={registry}
            />
          ) : (
            <ReportState
              loading={result.isPending}
              error={result.error}
              retry={() => void result.refetch()}
              empty={false}
            />
          )}
        </div>
      </div>
    </ReportCard>
  )
}

function DailyReport({ range }: { range: DashboardRange }) {
  const { text: adminText } = useAdminUI()

  const { adminQueries } = useAdminServices()

  const tooLong = rangeDays(range) > MAX_DAYS
  const query = useQuery({
    ...adminQueries.widgetMetrics(dailyQuery(range)),
    enabled: !tooLong,
  })
  const rows = query.data ? dailyRows(query.data) : []
  const totals = rows.reduce(
    (t, r) => ({
      started: t.started + r.started,
      canceled: t.canceled + r.canceled,
      net: t.net + r.net,
    }),
    { started: 0, canceled: 0, net: 0 }
  )
  return (
    <ReportCard
      title={adminText("Daily subscriptions")}
      description={adminText(
        "Subscriptions started and canceled each day (UTC), newest first."
      )}
      canDownload={rows.length > 0}
      onDownload={() => {
        const csv = dailyCsv(rows)
        downloadCsv(
          `daily-subscriptions-${suffix(range)}.csv`,
          csv.header,
          csv.rows
        )
      }}
    >
      {tooLong ? (
        <p className="py-6 text-center text-sm text-muted-foreground">
          <AdminMessage id={"The daily report covers at most "} />
          {MAX_DAYS}
          <AdminMessage id={" days. Pick a shorter range."} />
        </p>
      ) : rows.length === 0 || query.error ? (
        <ReportState
          loading={query.isPending}
          error={query.error}
          retry={() => void query.refetch()}
          empty={rows.length === 0}
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>
                <AdminMessage id={"Date"} />
              </TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"New"} />
              </TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"Canceled"} />
              </TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"Net change"} />
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {[...rows].reverse().map((r) => (
              <TableRow key={r.date}>
                <TableCell className="tabular-nums">{utcDay(r.date)}</TableCell>
                <TableCell className="text-right tabular-nums">
                  {r.started.toLocaleString()}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {r.canceled.toLocaleString()}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {signed(r.net)}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
          <TableFooter>
            <TableRow>
              <TableCell>
                <AdminMessage id={"Total"} />
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {totals.started.toLocaleString()}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {totals.canceled.toLocaleString()}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {signed(totals.net)}
              </TableCell>
            </TableRow>
          </TableFooter>
        </Table>
      )}
    </ReportCard>
  )
}

function MonthlyReport({ range }: { range: DashboardRange }) {
  const { text: adminText } = useAdminUI()

  const { adminQueries } = useAdminServices()

  const renewals = useQuery(adminQueries.widgetMetrics(renewalsQuery(range)))
  const churn = useQuery(adminQueries.widgetMetrics(churnQuery(range)))
  const rows =
    renewals.data && churn.data ? monthlyRows(renewals.data, churn.data) : []
  const error = renewals.error ?? churn.error
  return (
    <ReportCard
      title={adminText("Monthly renewals")}
      description={adminText(
        "Renewal payments, cancellations and churn each calendar month (UTC); churn is the month's cancellations over the subscriptions it began with."
      )}
      canDownload={rows.length > 0}
      onDownload={() => {
        const csv = monthlyCsv(rows)
        downloadCsv(
          `monthly-renewals-${suffix(range)}.csv`,
          csv.header,
          csv.rows
        )
      }}
    >
      {rows.length === 0 || error ? (
        <ReportState
          loading={renewals.isPending || churn.isPending}
          error={error}
          retry={() => {
            void renewals.refetch()
            void churn.refetch()
          }}
          empty={rows.length === 0}
        />
      ) : (
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>
                <AdminMessage id={"Month"} />
              </TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"Renewals"} />
              </TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"Canceled"} />
              </TableHead>
              <TableHead className="text-right">
                <AdminMessage id={"Churn"} />
              </TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {[...rows].reverse().map((r) => (
              <TableRow key={r.month}>
                <TableCell>{utcMonth(r.month)}</TableCell>
                <TableCell className="text-right tabular-nums">
                  {r.renewals.toLocaleString()}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {r.canceled.toLocaleString()}
                </TableCell>
                <TableCell className="text-right tabular-nums">
                  {percent(r.churn)}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      )}
    </ReportCard>
  )
}
