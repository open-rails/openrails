// Range picker for the dashboard and its reports: the presets, or a custom
// start and end date. The choice lives in the URL, so a link shows the same
// range.
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"

import {
  DEFAULT_PRESET,
  RANGE_PRESETS,
  rangeDates,
  rangeLabel,
} from "./range"
import { useDashboardRange } from "./use-range"

const CUSTOM = "custom"

export function RangePicker() {
  const [range, setRange] = useDashboardRange()
  const custom = "from" in range
  const items = [
    ...RANGE_PRESETS.map((p) => ({ value: p.value, label: p.label })),
    { value: CUSTOM, label: "Custom range" },
  ]
  return (
    <div className="flex flex-wrap items-center gap-2">
      <span
        className="hidden text-sm text-muted-foreground tabular-nums sm:inline"
        aria-live="polite"
      >
        {rangeLabel(range)}
      </span>
      <Select
        items={items}
        value={custom ? CUSTOM : range.preset}
        onValueChange={(next) => {
          if (!next) return
          setRange(next === CUSTOM ? rangeDates(range) : { preset: next })
        }}
      >
        <SelectTrigger size="sm" className="w-[150px]" aria-label="Date range">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          {items.map((item) => (
            <SelectItem key={item.value} value={item.value}>
              {item.label}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {custom && (
        <>
          <label className="flex items-center gap-1.5 text-xs font-medium">
            Start
            <Input
              type="date"
              className="h-8 w-auto"
              value={range.from}
              max={range.to}
              onChange={(e) => {
                const from = e.target.value
                if (from)
                  setRange({ from, to: from > range.to ? from : range.to })
              }}
            />
          </label>
          <label className="flex items-center gap-1.5 text-xs font-medium">
            End
            <Input
              type="date"
              className="h-8 w-auto"
              value={range.to}
              min={range.from}
              onChange={(e) => {
                const to = e.target.value
                if (to)
                  setRange({ from: to < range.from ? to : range.from, to })
              }}
            />
          </label>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setRange({ preset: DEFAULT_PRESET })}
          >
            Reset
          </Button>
        </>
      )}
    </div>
  )
}
