import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
// Build a metrics query from the registry: any measure, over time at a
// grain, split by the dimensions its measures share, filtered by one of
// them. The widget editor and the Reports page's explorer both use it, so
// every measure in GET /v1/metrics/schema can be charted.
import type { ReactNode } from "react"
import { Button } from "#orck/admin/components/ui/button"
import { Input } from "#orck/admin/components/ui/input"
import { Label } from "#orck/admin/components/ui/label"
import { Switch } from "#orck/admin/components/ui/switch"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "#orck/admin/components/ui/select"
import type { QueryMetricsParams } from "#orck/admin/lib/api/metrics"

import { measureLabel, type Registry } from "./registry"

const NONE = "none"
const GRAINS = ["auto", "day", "week", "month", "quarter", "year"]

// Draft is a query being built: its measures, whether it runs over time
// and at what grain, up to two dimensions to split by, and one filter.
export interface Draft {
  measures: string[]
  time: boolean
  grain: string
  dims: string[]
  filter?: { dim: string; values: string[] }
}

export function draftOf(query: QueryMetricsParams): Draft {
  const by = query.by ?? []
  const [dim, values] = Object.entries(query.filters ?? {}).find(
    ([d]) => d !== "currency"
  ) ?? [undefined, undefined]
  return {
    measures: query.measures,
    time: by.includes("time"),
    grain: query.grain || "auto",
    dims: by.filter((d) => d !== "time"),
    filter: dim && values ? { dim, values } : undefined,
  }
}

// queryOf is the query a draft asks, keeping base's range, comparison and
// the order and limit that still name what it groups.
export function queryOf(
  draft: Draft,
  base: QueryMetricsParams
): QueryMetricsParams {
  const by = [...(draft.time ? ["time"] : []), ...draft.dims]
  const filters: Record<string, string[]> = {}
  if (base.filters?.currency) filters.currency = base.filters.currency
  if (draft.filter?.values.length)
    filters[draft.filter.dim] = draft.filter.values
  const named = (o: { measure?: string; dimension?: string }) =>
    o.measure ? draft.measures.includes(o.measure) : by.includes(o.dimension!)
  const order = base.order?.every(named) ? base.order : undefined
  return {
    measures: draft.measures,
    by: by.length ? by : undefined,
    grain: draft.time ? draft.grain : undefined,
    range: base.range ?? { last: "30d" },
    filters: Object.keys(filters).length ? filters : undefined,
    order,
    limit: order ? base.limit : undefined,
    compare: base.compare,
  }
}

function Field({
  id,
  label,
  children,
}: {
  id: string
  label: string
  children: ReactNode
}) {
  return (
    <div className="flex flex-col gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  )
}

function Choice({
  id,
  value,
  items,
  onChange,
  label,
}: {
  id: string
  value: string
  items: { value: string; label: string }[]
  onChange: (value: string) => void
  label?: string
}) {
  return (
    <Select
      items={items}
      value={value}
      onValueChange={(next) => next && onChange(next)}
    >
      <SelectTrigger id={id} className="w-full" aria-label={label}>
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
  )
}

export function QueryBuilder({
  draft,
  onChange,
  registry,
  multiple = true,
}: {
  draft: Draft
  onChange: (draft: Draft) => void
  registry: Registry
  // multiple lets the draft chart more than one measure.
  multiple?: boolean
}) {
  const { id: adminId, text: adminText } = useAdminUI()
  const business = registry.measures.filter((m) => m.group !== "ops")
  const ops = registry.measures.filter((m) => m.group === "ops")
  const measureItems = [...business, ...ops].map((m) => ({
    value: m.name,
    label: `${measureLabel(m.name)}${m.group === "ops" ? " (ops)" : ""}`,
  }))
  const dims = registry.dims(draft.measures).filter((d) => d !== "currency")
  const dimItems = [
    { value: NONE, label: adminText("Nothing") },
    ...dims.map((d) => ({ value: d, label: measureLabel(d) })),
  ]
  const keep = (next: Draft): Draft => {
    // Dimensions and the filter follow the measures: one they no longer
    // share is dropped, and a measure read per a dimension (a balance per
    // account) is split by it.
    const shared = registry.dims(next.measures)
    const filter =
      next.filter && shared.includes(next.filter.dim) ? next.filter : undefined
    let dims = next.dims.filter((d) => shared.includes(d))
    for (const name of next.measures) {
      const per = registry.measure(name)?.per
      if (per && !dims.includes(per) && filter?.dim !== per)
        dims = [...dims, per]
    }
    return { ...next, dims, filter }
  }
  const setMeasure = (i: number, name: string) => {
    const measures = [...draft.measures]
    measures[i] = name
    onChange(keep({ ...draft, measures }))
  }
  const setDim = (i: number, dim: string) => {
    const next = [...draft.dims]
    if (dim === NONE) next.splice(i)
    else next[i] = dim
    onChange({ ...draft, dims: next.filter((d, j) => next.indexOf(d) === j) })
  }
  const values = draft.filter
    ? registry.values(draft.measures, draft.filter.dim)
    : null
  const describe = draft.measures
    .map((name) => registry.measure(name))
    .filter((m) => m !== undefined)

  return (
    <div className="flex flex-col gap-3">
      <div className="grid gap-2 sm:grid-cols-2">
        {draft.measures.map((name, i) => (
          <Field
            key={i}
            id={adminId(`measure-${i}`)}
            label={i === 0 ? adminText("Measure") : adminText("And")}
          >
            <div className="flex gap-1">
              <Choice
                id={adminId(`measure-${i}`)}
                value={name}
                items={measureItems}
                onChange={(next) => setMeasure(i, next)}
              />
              {i > 0 ? (
                <Button
                  variant="ghost"
                  size="sm"
                  onClick={() =>
                    onChange(
                      keep({
                        ...draft,
                        measures: draft.measures.filter((_, j) => j !== i),
                      })
                    )
                  }
                >
                  <AdminMessage id={"Remove"} />
                </Button>
              ) : null}
            </div>
          </Field>
        ))}
      </div>
      {multiple && draft.measures.length < 4 ? (
        <Button
          variant="outline"
          size="sm"
          className="w-fit"
          onClick={() =>
            onChange(
              keep({
                ...draft,
                measures: [
                  ...draft.measures,
                  measureItems.find((m) => !draft.measures.includes(m.value))
                    ?.value ?? draft.measures[0],
                ],
              })
            )
          }
        >
          <AdminMessage id={"Add a measure"} />
        </Button>
      ) : null}
      {describe.map((m) => (
        <p key={m.name} className="text-xs text-muted-foreground">
          <span className="font-medium">{measureLabel(m.name)}</span>:{" "}
          {m.description}
          {m.utc_days ? (
            <>
              {" "}
              <AdminMessage id={"Days are UTC dates."} />
            </>
          ) : null}
        </p>
      ))}
      <div className="flex flex-wrap items-end gap-3">
        <div className="flex items-center gap-2">
          <Switch
            id={adminId("query-time")}
            checked={draft.time}
            onCheckedChange={(time) => onChange({ ...draft, time })}
          />
          <Label htmlFor={adminId("query-time")}>
            <AdminMessage id={"Over time"} />
          </Label>
        </div>
        {draft.time ? (
          <div className="w-36">
            <Choice
              id={adminId("query-grain")}
              label={adminText("Grain")}
              value={draft.grain}
              items={GRAINS.map((g) => ({
                value: g,
                label: g === "auto" ? adminText("Auto") : measureLabel(g),
              }))}
              onChange={(grain) => onChange({ ...draft, grain })}
            />
          </div>
        ) : null}
      </div>
      <div className="grid gap-2 sm:grid-cols-2">
        {[0, 1].map((i) =>
          i <= draft.dims.length ? (
            <Field
              key={i}
              id={adminId(`query-dim-${i}`)}
              label={i === 0 ? adminText("Split by") : adminText("Then by")}
            >
              <Choice
                id={adminId(`query-dim-${i}`)}
                value={draft.dims[i] ?? NONE}
                items={dimItems}
                onChange={(dim) => setDim(i, dim)}
              />
            </Field>
          ) : null
        )}
      </div>
      <div className="grid gap-2 sm:grid-cols-[12rem_1fr]">
        <Field id={adminId("query-filter")} label={adminText("Only where")}>
          <Choice
            id={adminId("query-filter")}
            value={draft.filter?.dim ?? NONE}
            items={dimItems}
            onChange={(dim) =>
              onChange({
                ...draft,
                filter: dim === NONE ? undefined : { dim, values: [] },
              })
            }
          />
        </Field>
        {draft.filter ? (
          values ? (
            <div className="flex flex-wrap items-end gap-1">
              {values.map((v) => {
                const on = draft.filter!.values.includes(v)
                return (
                  <Button
                    key={v}
                    size="sm"
                    variant={on ? "secondary" : "outline"}
                    aria-pressed={on}
                    onClick={() =>
                      onChange({
                        ...draft,
                        filter: {
                          dim: draft.filter!.dim,
                          values: on
                            ? draft.filter!.values.filter((x) => x !== v)
                            : [...draft.filter!.values, v],
                        },
                      })
                    }
                  >
                    {v === "" ? adminText("(none)") : v}
                  </Button>
                )
              })}
            </div>
          ) : (
            <Field
              id={adminId("query-filter-values")}
              label={adminText("Values, comma separated")}
            >
              <Input
                id={adminId("query-filter-values")}
                value={draft.filter.values.join(",")}
                onChange={(e) =>
                  onChange({
                    ...draft,
                    filter: {
                      dim: draft.filter!.dim,
                      values: e.target.value
                        .split(",")
                        .map((v) => v.trim())
                        .filter(Boolean),
                    },
                  })
                }
              />
            </Field>
          )
        ) : null}
      </div>
    </div>
  )
}
