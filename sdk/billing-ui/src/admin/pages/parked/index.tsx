import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import * as React from "react"
import { useQuery } from "@tanstack/react-query"
import { Link, useSearchParams } from "#orck/admin/router"

import { CursorPager } from "#orck/admin/components/cursor-pager"
import { StatusBadge } from "#orck/admin/components/status-badge"
import { Badge } from "#orck/admin/components/ui/badge"
import { Button } from "#orck/admin/components/ui/button"
import { Label } from "#orck/admin/components/ui/label"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "#orck/admin/components/ui/table"
import {
  useMerchantConfig,
  useMerchantConfigUpdates,
} from "#orck/admin/lib/capabilities"
import { useUrlCursor } from "#orck/admin/lib/cursor-pages"
import { formatDate, shortId } from "#orck/admin/lib/format"
import type { ParkedSubscription } from "#orck/client/generated/wire"
import {
  ReconcileDialog,
  pspKey,
  selectClass,
  usePSPs,
} from "#orck/admin/pages/imports/shared"
import {
  MapButton,
  MapPriceDialog,
} from "#orck/admin/pages/provider-prices/map-dialog"

const REASONS: ParkedSubscription["reason"][] = [
  "price_unmapped",
  "customer_unlinked",
  "product_held",
]

export function ParkedPage() {
  return useMerchantConfig() ? <Parked /> : null
}

// Parked are live provider subscriptions OpenRails could not land. Each
// lands by itself, once, when its cause is fixed.
function Parked() {
  const { text, id: adminId } = useAdminUI()
  const { importQueries } = useAdminServices()
  const canUpdate = useMerchantConfigUpdates()
  const psps = usePSPs()
  const [params, setParams] = useSearchParams()
  const filters = {
    psp: params.get("psp") || undefined,
    reason: (params.get("reason") || undefined) as
      ParkedSubscription["reason"] | undefined,
    released: params.get("released") === "true",
  }
  const paging = useUrlCursor(50)
  const { data, isPending, isFetching } = useQuery(
    importQueries.parked(filters, paging.size, paging.cursor || undefined)
  )
  const [mapping, setMapping] = React.useState<ParkedSubscription>()
  const [reconciling, setReconciling] = React.useState<string>()
  const set = (key: string, value: string) => {
    const next = new URLSearchParams(params)
    if (value) next.set(key, value)
    else next.delete(key)
    next.delete("cursor")
    setParams(next)
  }
  const mappingPSP = mapping && psps.byId.get(mapping.psp)
  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-xl font-semibold">
          <AdminMessage id="Parked subscriptions" />
        </h1>
        <p className="text-sm text-muted-foreground">
          <AdminMessage id="Live provider subscriptions OpenRails could not land. Each lands once, by itself, when its cause is fixed." />
        </p>
      </div>
      <div className="grid items-end gap-3 md:grid-cols-3">
        <div className="grid gap-1.5">
          <Label htmlFor={adminId("parked-psp")}>
            <AdminMessage id="PSP" />
          </Label>
          <select
            id={adminId("parked-psp")}
            className={selectClass}
            value={filters.psp ?? ""}
            onChange={(e) => set("psp", e.target.value)}
          >
            <option value="">{text("All PSPs")}</option>
            {psps.list.map((p) => (
              <option key={p.id} value={p.id}>
                {p.key}
              </option>
            ))}
          </select>
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor={adminId("parked-reason")}>
            <AdminMessage id="Reason" />
          </Label>
          <select
            id={adminId("parked-reason")}
            className={selectClass}
            value={filters.reason ?? ""}
            onChange={(e) => set("reason", e.target.value)}
          >
            <option value="">{text("All reasons")}</option>
            {REASONS.map((r) => (
              <option key={r} value={r}>
                {r}
              </option>
            ))}
          </select>
        </div>
        <label className="flex h-8 items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={filters.released}
            onChange={(e) => set("released", e.target.checked ? "true" : "")}
          />
          <AdminMessage id="Show released" />
        </label>
      </div>
      <div className="overflow-x-auto rounded-md border">
        <Table>
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              {[
                "PSP",
                "Provider subscription",
                "Reason",
                "Object",
                "Since",
                "Status",
                "",
              ].map((h, i) => (
                <TableHead key={i} className="text-muted-foreground">
                  {h && text(h)}
                </TableHead>
              ))}
            </TableRow>
          </TableHeader>
          <TableBody>
            {isPending ? (
              <TableRow>
                <TableCell colSpan={7}>
                  <AdminMessage id="Loading…" />
                </TableCell>
              </TableRow>
            ) : !data?.data.length ? (
              <TableRow>
                <TableCell colSpan={7} className="text-muted-foreground">
                  <AdminMessage id="Nothing is parked." />
                </TableCell>
              </TableRow>
            ) : (
              data.data.map((p) => (
                <TableRow key={p.id} data-ref={p.subscription_ref}>
                  <TableCell>{pspKey(psps.byId, p.psp)}</TableCell>
                  <TableCell className="font-mono text-xs">
                    {p.subscription_ref}
                  </TableCell>
                  <TableCell>{p.reason}</TableCell>
                  <TableCell className="font-mono text-xs">
                    {p.price_ref ?? p.customer_ref ?? "—"}
                  </TableCell>
                  <TableCell>{formatDate(p.parked_at)}</TableCell>
                  <TableCell>
                    <ParkStatus park={p} />
                  </TableCell>
                  <TableCell className="text-right whitespace-nowrap">
                    {canUpdate && !p.released_at && !p.import_owned && (
                      <Remedy
                        park={p}
                        onMap={() => setMapping(p)}
                        onReconcile={() => setReconciling(p.psp)}
                      />
                    )}
                  </TableCell>
                </TableRow>
              ))
            )}
          </TableBody>
        </Table>
      </div>
      <CursorPager
        pages={paging}
        rows={data?.data.length ?? 0}
        nextCursor={data?.next_cursor}
        busy={isFetching}
      />
      {mapping?.price_ref && mappingPSP && (
        <MapPriceDialog
          psp={mappingPSP}
          priceRef={mapping.price_ref}
          onClose={() => setMapping(undefined)}
        />
      )}
      {reconciling && (
        <ReconcileDialog
          psp={reconciling}
          onClose={() => setReconciling(undefined)}
        />
      )}
    </div>
  )
}

function ParkStatus({ park }: { park: ParkedSubscription }) {
  const { text } = useAdminUI()
  if (park.released_at)
    return (
      <span className="flex items-center gap-1">
        <StatusBadge status={park.released_by ?? "released"} />
        {park.subscription_id && (
          <Link
            className="underline"
            to={`/subscriptions/${park.subscription_id}`}
          >
            {shortId(park.subscription_id, 12)}
          </Link>
        )}
      </span>
    )
  // An import of the PSP stages it: Apply lands it, and no finding is raised.
  if (park.import_owned)
    return <Badge variant="secondary">{text("waiting for import")}</Badge>
  return <StatusBadge status="open" />
}

function Remedy({
  park,
  onMap,
  onReconcile,
}: {
  park: ParkedSubscription
  onMap: () => void
  onReconcile: () => void
}) {
  switch (park.reason) {
    case "price_unmapped":
      return <MapButton onClick={onMap} />
    case "customer_unlinked":
      return (
        <Button size="sm" variant="outline" onClick={onReconcile}>
          <AdminMessage id="Reconcile PSP" />
        </Button>
      )
    case "product_held":
      return (
        <span className="text-xs text-muted-foreground">
          <AdminMessage id="The customer holds this product already: cancel one at the provider." />
        </span>
      )
  }
}
