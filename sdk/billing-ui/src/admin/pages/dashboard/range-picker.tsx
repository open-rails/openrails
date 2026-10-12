import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useRef } from "react"
// Range picker for the dashboard and its reports: the presets, or a custom
// start and end date. The choice lives in the URL, so a link shows the same
// range.
import { Button } from "#orck/admin/components/ui/button"
import { Input } from "#orck/admin/components/ui/input"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "#orck/admin/components/ui/select"

import { DEFAULT_PRESET, RANGE_PRESETS, rangeDates, rangeLabel } from "./range"
import { useDashboardRange } from "./use-range"

const CUSTOM = "custom"

export function RangePicker() {
  const { text: adminText } = useAdminUI()

  const [range, setRange] = useDashboardRange()
  // A host router can publish its new URL after the next input edit. Read
  // both live fields; the committed URL key resets them on back/forward.
  const startInput = useRef<HTMLInputElement>(null)
  const endInput = useRef<HTMLInputElement>(null)
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
        <SelectTrigger
          size="sm"
          className="w-[150px]"
          aria-label={adminText("Date range")}
        >
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
        <div key={`${range.from}/${range.to}`} className="contents">
          <label className="flex items-center gap-1.5 text-xs font-medium">
            <AdminMessage id={"Start"} />
            <Input
              ref={startInput}
              type="date"
              className="h-8 w-auto"
              defaultValue={range.from}
              max={range.to}
              onChange={(e) => {
                const from = e.target.value
                const to = endInput.current?.value || range.to
                if (from) setRange({ from, to: from > to ? from : to })
              }}
            />
          </label>
          <label className="flex items-center gap-1.5 text-xs font-medium">
            <AdminMessage id={"End"} />
            <Input
              ref={endInput}
              type="date"
              className="h-8 w-auto"
              defaultValue={range.to}
              min={range.from}
              onChange={(e) => {
                const to = e.target.value
                const from = startInput.current?.value || range.from
                if (to) setRange({ from: to < from ? to : from, to })
              }}
            />
          </label>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setRange({ preset: DEFAULT_PRESET })}
          >
            <AdminMessage id={"Reset"} />
          </Button>
        </div>
      )}
    </div>
  )
}
