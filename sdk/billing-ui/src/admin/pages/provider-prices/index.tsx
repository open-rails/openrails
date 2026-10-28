import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Link, useSearchParams } from "#orck/admin/router"

import { CursorPager } from "#orck/admin/components/cursor-pager"
import { Badge } from "#orck/admin/components/ui/badge"
import { Button } from "#orck/admin/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "#orck/admin/components/ui/dialog"
import { Label } from "#orck/admin/components/ui/label"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "#orck/admin/components/ui/table"
import { Textarea } from "#orck/admin/components/ui/textarea"
import {
  useCatalogWrites,
  useMerchantConfig,
  useMerchantConfigUpdates,
} from "#orck/admin/lib/capabilities"
import { useUrlCursor } from "#orck/admin/lib/cursor-pages"
import { DIALOG_FORM } from "#orck/admin/lib/dialog-width"
import { shortId } from "#orck/admin/lib/format"
import type { ProviderCatalogObject } from "#orck/client/generated/wire"
import {
  selectClass,
  termsText,
  usePSPs,
} from "#orck/admin/pages/imports/shared"
import {
  CATALOG_MANAGE,
  MapButton,
  MapPriceDialog,
} from "#orck/admin/pages/provider-prices/map-dialog"

// The rails whose catalog OpenRails observes.
const OBSERVED_RAILS = ["nmi", "stripe"]

const SHOWN = {
  unmapped: false,
  mapped: true,
  all: undefined,
} as const

export function ProviderPricesPage() {
  return useMerchantConfig() ? <ProviderPrices /> : null
}

function ProviderPrices() {
  const { text, id: adminId } = useAdminUI()
  const { importQueries } = useAdminServices()
  const canUpdate = useMerchantConfigUpdates()
  const canMap = useCatalogWrites()
  const psps = usePSPs().list.filter((p) => OBSERVED_RAILS.includes(p.rail))
  const [params, setParams] = useSearchParams()
  const psp = psps.find((p) => p.id === params.get("psp")) ?? psps[0]
  const linked = params.get("ref")
  const shown = (params.get("show") ??
    (linked ? "all" : "unmapped")) as keyof typeof SHOWN
  const paging = useUrlCursor(50)
  const { data, isPending, isFetching } = useQuery(
    importQueries.providerCatalog(
      { psp: psp?.id ?? "", kind: "price", mapped: SHOWN[shown] },
      paging.size,
      paging.cursor || undefined
    )
  )
  const [mapping, setMapping] = React.useState<string | null>(null)
  const [dismissing, setDismissing] = React.useState<ProviderCatalogObject>()
  const set = (key: string, value: string) => {
    const next = new URLSearchParams(params)
    next.set(key, value)
    next.delete("cursor")
    setParams(next)
  }
  return (
    <div className="flex flex-col gap-4">
      <div>
        <h1 className="text-xl font-semibold">
          <AdminMessage id="Provider prices" />
        </h1>
        <p className="text-sm text-muted-foreground">
          <AdminMessage id="Prices each PSP's own catalog holds, as OpenRails last observed them. A subscription on an unmapped price cannot land: map it to a product, or dismiss it as not yours." />
        </p>
      </div>
      {canUpdate && !canMap && (
        <p className="text-sm text-muted-foreground" role="note">
          <AdminMessage id="Mapping a price edits the catalog: it needs " />
          <code>{CATALOG_MANAGE}</code>.
        </p>
      )}
      <div className="grid gap-3 md:grid-cols-3">
        <div className="grid gap-1.5">
          <Label htmlFor={adminId("prices-psp")}>
            <AdminMessage id="PSP" />
          </Label>
          <select
            id={adminId("prices-psp")}
            className={selectClass}
            value={psp?.id ?? ""}
            onChange={(e) => set("psp", e.target.value)}
          >
            {psps.map((p) => (
              <option key={p.id} value={p.id}>
                {p.key} ({p.rail})
              </option>
            ))}
          </select>
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor={adminId("prices-show")}>
            <AdminMessage id="Show" />
          </Label>
          <select
            id={adminId("prices-show")}
            className={selectClass}
            value={shown}
            onChange={(e) => set("show", e.target.value)}
          >
            <option value="unmapped">{text("Unmapped")}</option>
            <option value="mapped">{text("Mapped")}</option>
            <option value="all">{text("All")}</option>
          </select>
        </div>
      </div>
      {!psp ? (
        <p className="text-sm text-muted-foreground">
          <AdminMessage id="No PSP here has a catalog OpenRails observes." />
        </p>
      ) : (
        <>
          <div className="overflow-x-auto rounded-md border">
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  {[
                    "Price",
                    "Name",
                    "Terms",
                    "State",
                    "Parked",
                    "Mapped to",
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
                      <AdminMessage id="No provider prices match." />
                    </TableCell>
                  </TableRow>
                ) : (
                  data.data.map((o) => (
                    <TableRow
                      key={o.ref}
                      data-ref={o.ref}
                      className={
                        o.ref === linked ? "bg-primary/[0.06]" : undefined
                      }
                    >
                      <TableCell className="font-mono text-xs">
                        {o.ref}
                        {o.product_ref && (
                          <span className="block text-muted-foreground">
                            {o.product_ref}
                          </span>
                        )}
                      </TableCell>
                      <TableCell>{o.name ?? "—"}</TableCell>
                      <TableCell>{termsText(o.terms, psp.rail)}</TableCell>
                      <TableCell>
                        <ObjectState object={o} />
                      </TableCell>
                      <TableCell>{o.parked || "—"}</TableCell>
                      <TableCell>
                        {o.price ? (
                          <Link
                            className="underline"
                            to={`/catalog/prices/${o.price}`}
                          >
                            {shortId(o.price, 14)}
                          </Link>
                        ) : (
                          <AdminMessage id="Unmapped" />
                        )}
                      </TableCell>
                      <TableCell className="space-x-2 text-right whitespace-nowrap">
                        {canUpdate && !o.price && (
                          <MapButton onClick={() => setMapping(o.ref)} />
                        )}
                        {canUpdate && !o.price && o.finding && (
                          <Button
                            size="sm"
                            variant="ghost"
                            onClick={() => setDismissing(o)}
                          >
                            <AdminMessage id="Dismiss" />
                          </Button>
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
        </>
      )}
      {psp && mapping && (
        <MapPriceDialog
          psp={psp}
          priceRef={mapping}
          onClose={() => setMapping(null)}
        />
      )}
      {dismissing?.finding && (
        <DismissDialog
          finding={dismissing.finding}
          priceRef={dismissing.ref}
          onClose={() => setDismissing(undefined)}
        />
      )}
    </div>
  )
}

function ObjectState({ object }: { object: ProviderCatalogObject }) {
  const { text } = useAdminUI()
  const state = object.deleted_at
    ? "deleted"
    : !object.active
      ? "inactive"
      : object.retired
        ? "retired"
        : "active"
  return (
    <span className="flex gap-1">
      <Badge variant="secondary">{text(state)}</Badge>
      {object.finding && (
        <Link to={`/ops?finding=${object.finding}`}>
          <Badge variant="secondary" className="bg-held-surface text-held">
            {text("finding")}
          </Badge>
        </Link>
      )}
    </span>
  )
}

// DismissDialog ignores the price's unmapped finding: the price is not the
// merchant's to sell. Its parks stay listed.
function DismissDialog({
  finding,
  priceRef,
  onClose,
}: {
  finding: string
  priceRef: string
  onClose: () => void
}) {
  const { text, id: adminId } = useAdminUI()
  const { importMutations, toast, toastApiError } = useAdminServices()
  const queryClient = useQueryClient()
  const dismiss = useMutation(importMutations.dismissPrice(queryClient))
  const [notes, setNotes] = React.useState("")
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            <AdminMessage id="Dismiss provider price" />
          </DialogTitle>
          <DialogDescription>
            {priceRef} ·{" "}
            <AdminMessage id="its finding is ignored; subscriptions on it stay parked." />
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-1.5">
          <Label htmlFor={adminId("dismiss-notes")}>
            <AdminMessage id="Notes (required)" />
          </Label>
          <Textarea
            id={adminId("dismiss-notes")}
            value={notes}
            onChange={(e) => setNotes(e.target.value)}
          />
        </div>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            <AdminMessage id="Cancel" />
          </Button>
          <Button
            disabled={!notes.trim() || dismiss.isPending}
            onClick={async () => {
              try {
                await dismiss.mutateAsync({ finding, notes: notes.trim() })
                toast.success(text("Price dismissed"))
                onClose()
              } catch (err) {
                toastApiError(err, text("Dismiss provider price"))
              }
            }}
          >
            <AdminMessage id="Dismiss" />
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
