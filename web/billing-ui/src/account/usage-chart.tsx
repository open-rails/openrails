import {
  Bar,
  BarChart,
  CartesianGrid,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from "recharts"

/** One bucket of a usage chart: its label and cost in major units. */
export interface UsagePoint {
  startsAt: string
  label: string
  value: number
  amount: string
  requests: number
}

/** A usage series as bars; loaded lazily by UsagePanel. */
export default function UsageChart({
  data,
  money,
  requestsLabel,
}: {
  data: UsagePoint[]
  money: (amount: string) => string
  requestsLabel: string
}) {
  return (
    <ResponsiveContainer width="100%" height="100%">
      <BarChart data={data} margin={{ left: 0, right: 8, top: 8, bottom: 0 }}>
        <CartesianGrid vertical={false} stroke="var(--color-border)" />
        <XAxis
          dataKey="label"
          tickLine={false}
          axisLine={false}
          minTickGap={16}
          tick={{ fill: "var(--color-muted-foreground)" }}
        />
        <YAxis
          width={56}
          tickLine={false}
          axisLine={false}
          tick={{ fill: "var(--color-muted-foreground)" }}
        />
        <Tooltip
          cursor={{ fill: "var(--color-muted)" }}
          content={({ active, payload }) => {
            const p = active
              ? (payload?.[0]?.payload as UsagePoint | undefined)
              : undefined
            if (!p) return null
            return (
              <div className="grid gap-0.5 rounded-md border bg-popover px-2.5 py-1.5 text-xs text-popover-foreground shadow-md">
                <span className="font-medium">{p.label}</span>
                <span className="tabular-nums">{money(p.amount)}</span>
                <span className="text-muted-foreground tabular-nums">
                  {requestsLabel}: {p.requests}
                </span>
              </div>
            )
          }}
        />
        <Bar
          dataKey="value"
          fill="var(--color-primary)"
          radius={[3, 3, 0, 0]}
          isAnimationActive={false}
        />
      </BarChart>
    </ResponsiveContainer>
  )
}
