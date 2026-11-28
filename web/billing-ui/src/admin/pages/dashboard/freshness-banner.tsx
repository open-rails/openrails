import * as React from "react"
import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import type { PSPKnownThrough } from "#orck/admin/lib/api/metrics"
import { formatNativeAmount } from "#orck/admin/lib/format"
import { cn } from "#orck/admin/lib/utils"
import { Button } from "#orck/admin/components/ui/button"

// What a stale PSP's cause means, and what fixes it.
const CAUSES: Record<string, { what: string; fix: string }> = {
  imported: {
    what: "imported, not connected",
    fix: "Connect it to bring the data up to date.",
  },
  not_configured: { what: "no credentials", fix: "Add its credentials." },
  credentials_refused: {
    what: "the provider refuses its credentials",
    fix: "Update its credentials.",
  },
  unreachable: {
    what: "the provider is not answering",
    fix: "Catch-up is automatic once it answers.",
  },
  catching_up: { what: "reading the gap now", fix: "" },
  webhooks_silent: {
    what: "webhooks silent while reads succeed",
    fix: "Register its webhook endpoint again.",
  },
}

const TONES: Record<string, string> = {
  critical: "border-destructive/50 bg-destructive/5",
  warning: "border-amber-500/50 bg-amber-500/5",
  info: "border-border bg-muted/40",
}

const day = (iso: string) =>
  new Date(iso).toLocaleDateString(undefined, {
    month: "short",
    day: "numeric",
    year: "numeric",
  })

const daysOld = (iso: string, now: number) =>
  Math.max(0, Math.floor((now - Date.parse(iso)) / 86_400_000))

// FreshnessBanner says, for each PSP whose data is out of date, what is known
// and since when, why, the fix, and what awaits it. When every PSP with data
// is stale the page shows it as of the latest known-through, with a switch
// to now.
export function FreshnessBanner({
  freshness,
  knownThrough,
  asOf,
  onAsOf,
}: {
  freshness: PSPKnownThrough[]
  knownThrough: string | null
  asOf: boolean
  onAsOf: (asOf: boolean) => void
}) {
  const { text } = useAdminUI()
  const [now] = React.useState(() => Date.now())
  const shown = freshness.filter(
    (f) => f.has_data && (!f.current || f.cause === "webhooks_silent")
  )
  if (shown.length === 0) return null
  return (
    <div className="flex flex-col gap-2" data-freshness>
      {shown.map((f) => {
        const cause = CAUSES[f.cause] ?? { what: f.cause, fix: "" }
        const since =
          f.failing_since &&
          (f.cause === "credentials_refused" || f.cause === "unreachable")
            ? `${text(" since ")}${day(f.failing_since)}`
            : ""
        return (
          <div
            key={f.psp_id}
            className={cn(
              "rounded-lg border px-4 py-3 text-sm",
              TONES[f.severity] ?? TONES.info
            )}
            data-psp={f.key}
            data-severity={f.severity}
          >
            <span className="font-medium">
              {f.key} ({f.rail.toUpperCase()})
            </span>
            {f.current ? (
              ": "
            ) : f.known_through ? (
              <>
                <AdminMessage id={": data through "} />
                {day(f.known_through)}
                {f.source === "import_records" ? (
                  <AdminMessage id={" (latest imported record)"} />
                ) : null}
                , {daysOld(f.known_through, now)}
                <AdminMessage id={" days old; "} />
              </>
            ) : (
              <AdminMessage id={": data through an unknown date; "} />
            )}
            {text(cause.what)}
            {since}. {cause.fix ? text(cause.fix) : null}
            {f.awaiting.map((a) => (
              <div key={a.currency} className="text-muted-foreground">
                {a.subscriptions}
                <AdminMessage id={" subscriptions, "} />
                {formatNativeAmount(a.mrr, a.currency)}
                <AdminMessage id={" MRR awaiting "} />
                {f.key}
              </div>
            ))}
          </div>
        )
      })}
      {knownThrough ? (
        <div className="flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
          {asOf ? (
            <>
              <AdminMessage id={"Showing data as of "} />
              {day(knownThrough)}.
              <Button size="sm" variant="outline" onClick={() => onAsOf(false)}>
                <AdminMessage id={"Show to now"} />
              </Button>
            </>
          ) : (
            <Button size="sm" variant="outline" onClick={() => onAsOf(true)}>
              <AdminMessage id={"Show as of "} />
              {day(knownThrough)}
            </Button>
          )}
        </div>
      ) : null}
    </div>
  )
}
