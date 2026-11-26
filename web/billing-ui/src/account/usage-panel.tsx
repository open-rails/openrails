import * as React from "react"
import { cn } from "cn"
import type { BillingAppearance } from "#orck/appearance"
import type { UsageGroupBy, UsageInterval } from "#orck/client/client"
import type { UsageSeries } from "#orck/client/types"
import { Button } from "#orck/components/ui/button"
import { useMessages } from "#orck/i18n/context"
import type { MessageKey } from "#orck/i18n/messages"
import { amountUnits } from "#orck/lib/money"
import { useCurrencyScales } from "#orck/react/config"
import { useUsageSeries } from "#orck/react/hooks"
import { useUiSettings } from "#orck/scope-context"
import { formatMoney } from "./format"
import { EmptyState, ErrorState, ListSkeleton, Section } from "./section"
import type { UsagePoint } from "./usage-chart"

// The chart library loads with the first chart, not with the package.
const UsageChart = React.lazy(() => import("./usage-chart"))

export interface UsagePanelProps {
  /** The currency (or merchant unit) usage is shown in. */
  currency: string
  /** The interval shown first; daily by default. */
  defaultInterval?: UsageInterval
  /** The zone shown first; the browser's by default. */
  defaultTimezone?: string
  groupBy?: UsageGroupBy
  appearance?: BillingAppearance
  className?: string
}

const intervals: UsageInterval[] = ["hour", "day", "week", "month"]

// How far back each interval looks: two days of hours, a month of days, half
// a year of weeks, a year of months; all within the series' bucket cap.
function windowStart(interval: UsageInterval, now: Date): Date {
  const d = new Date(now)
  switch (interval) {
    case "hour":
      d.setTime(d.getTime() - 48 * 3_600_000)
      break
    case "day":
      d.setTime(d.getTime() - 30 * 86_400_000)
      break
    case "week":
      d.setTime(d.getTime() - 26 * 7 * 86_400_000)
      break
    case "month":
      d.setUTCMonth(d.getUTCMonth() - 12)
      break
  }
  return d
}

function browserZone(): string {
  try {
    return Intl.DateTimeFormat().resolvedOptions().timeZone || "UTC"
  } catch {
    return "UTC"
  }
}

function zones(current: string): string[] {
  let list: string[]
  try {
    list = Intl.supportedValuesOf("timeZone")
  } catch {
    list = []
  }
  return [...new Set([current, "UTC", ...list])].sort()
}

const labelFormats: Record<UsageInterval, Intl.DateTimeFormatOptions> = {
  hour: { month: "short", day: "numeric", hour: "numeric" },
  day: { month: "short", day: "numeric" },
  week: { month: "short", day: "numeric" },
  month: { month: "short", year: "numeric" },
}

// points are a series' buckets as chart values: each bucket's cost less its
// volume discount, in major units.
function points(
  series: UsageSeries,
  interval: UsageInterval,
  zone: string,
  decimals: number | undefined,
  locale: string | undefined
): UsagePoint[] {
  let format: Intl.DateTimeFormat
  try {
    format = new Intl.DateTimeFormat(locale, {
      ...labelFormats[interval],
      timeZone: zone,
    })
  } catch {
    format = new Intl.DateTimeFormat(locale, labelFormats[interval])
  }
  return series.buckets.map((b) => {
    let units = -(amountUnits(b.discount) ?? 0n)
    let requests = 0
    for (const r of b.rows) {
      units += amountUnits(r.amount) ?? 0n
      requests += r.event_count
    }
    const scale = 10 ** (decimals ?? 0)
    return {
      startsAt: b.starts_at,
      label: format.format(new Date(b.starts_at)),
      value: Number(units) / scale,
      amount: units.toString(),
      requests,
    }
  })
}

/** The customer's usage over time: a chart by interval in a zone it picks. */
export function UsagePanel({
  currency,
  defaultInterval = "day",
  defaultTimezone,
  groupBy,
  appearance,
  className,
}: UsagePanelProps) {
  const m = useMessages()
  const { t } = m
  const { locale } = useUiSettings()
  const scales = useCurrencyScales()
  const [interval, setUsageInterval] =
    React.useState<UsageInterval>(defaultInterval)
  const [zone, setZone] = React.useState(() => defaultTimezone ?? browserZone())
  // The window is fixed when the interval changes, so a render reads nothing
  // again.
  const [now] = React.useState(() => new Date())
  const from = React.useMemo(
    () => windowStart(interval, now).toISOString(),
    [interval, now]
  )
  const state = useUsageSeries({
    currency,
    interval,
    timezone: zone,
    groupBy,
    from,
    to: now.toISOString(),
  })
  const zoneList = React.useMemo(() => zones(zone), [zone])
  const decimals = scales[currency.toUpperCase()]
  const data = React.useMemo(
    () =>
      state.series
        ? points(state.series, interval, zone, decimals, locale)
        : [],
    [state.series, interval, zone, decimals, locale]
  )
  const money = (amount: string) =>
    formatMoney(amount, currency, scales, locale) ?? amount
  const total = data.reduce((sum, p) => sum + BigInt(p.amount), 0n)
  const discount = (state.series?.buckets ?? []).reduce(
    (sum, b) => sum + (amountUnits(b.discount) ?? 0n),
    0n
  )

  let body: React.ReactNode
  if (state.series === null && state.error) {
    body = <ErrorState error={state.error} onRetry={state.refetch} />
  } else if (state.series === null) {
    body = <ListSkeleton rows={3} />
  } else if (data.every((p) => p.requests === 0 && p.value === 0)) {
    body = (
      <EmptyState
        title={t("usage.empty")}
        description={t("usage.emptyDescription")}
      />
    )
  } else {
    body = (
      <div className="grid gap-3">
        <div className="flex flex-wrap items-baseline gap-x-4 gap-y-1 text-sm">
          <span className="text-muted-foreground">{t("usage.total")}</span>
          <span data-testid="usage-total" className="font-medium tabular-nums">
            {money(total.toString())}
          </span>
          {discount !== 0n ? (
            <span
              data-testid="usage-discount"
              className="text-xs text-muted-foreground tabular-nums"
            >
              {t("usage.discount")} {money((-discount).toString())}
            </span>
          ) : null}
        </div>
        <div
          className={cn(
            "h-56 w-full text-xs",
            state.loading && "opacity-60 transition-opacity"
          )}
          aria-busy={state.loading || undefined}
          data-testid="usage-chart"
        >
          <React.Suspense fallback={null}>
            <UsageChart
              data={data}
              money={money}
              requestsLabel={t("usage.requests")}
            />
          </React.Suspense>
        </div>
      </div>
    )
  }

  const controls = (
    <div className="flex flex-wrap items-center gap-2">
      <div
        role="group"
        aria-label={t("usage.intervalLabel")}
        className="flex gap-1"
      >
        {intervals.map((i) => (
          <Button
            key={i}
            size="sm"
            variant={i === interval ? "secondary" : "ghost"}
            aria-pressed={i === interval}
            data-testid={`usage-interval-${i}`}
            onClick={() => setUsageInterval(i)}
          >
            {t(`usage.interval.${i}` as MessageKey)}
          </Button>
        ))}
      </div>
      <label className="flex items-center gap-1.5 text-xs text-muted-foreground">
        {t("usage.timezone")}
        <select
          value={zone}
          onChange={(e) => setZone(e.target.value)}
          data-testid="usage-timezone"
          className="h-8 max-w-48 rounded-md border border-input bg-transparent px-2 text-sm text-foreground outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 dark:bg-input/30"
        >
          {zoneList.map((z) => (
            <option key={z} value={z}>
              {z}
            </option>
          ))}
        </select>
      </label>
    </div>
  )

  return (
    <Section
      title={t("usage.title")}
      description={t("usage.description")}
      appearance={appearance}
      className={className}
      data-testid="usage-panel"
    >
      <div className="grid gap-4">
        {controls}
        {body}
      </div>
    </Section>
  )
}
