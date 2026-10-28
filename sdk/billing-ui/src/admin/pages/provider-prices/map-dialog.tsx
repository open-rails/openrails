import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import * as React from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { Button } from "#orck/admin/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "#orck/admin/components/ui/dialog"
import { Input } from "#orck/admin/components/ui/input"
import { Label } from "#orck/admin/components/ui/label"
import { useCatalogWrites } from "#orck/admin/lib/capabilities"
import { DIALOG_FORM } from "#orck/admin/lib/dialog-width"
import type { PSP } from "#orck/client/generated/wire"
import { selectClass, termsText } from "#orck/admin/pages/imports/shared"

export const CATALOG_MANAGE = "merchant:catalog:manage"

// MapButton opens the Map dialog; without catalog manage it is disabled and
// names the permission it needs.
export function MapButton({ onClick }: { onClick: () => void }) {
  const { text } = useAdminUI()
  const allowed = useCatalogWrites()
  return (
    <Button
      size="sm"
      variant="outline"
      disabled={!allowed}
      title={allowed ? undefined : `${text("Needs ")}${CATALOG_MANAGE}`}
      onClick={onClick}
    >
      <AdminMessage id="Map to product" />
    </Button>
  )
}

// MapPriceDialog maps one observed provider price to a product. Its terms are
// the provider's; by default it is not a tier-change target (archived).
export function MapPriceDialog({
  psp,
  priceRef,
  onClose,
}: {
  psp: PSP
  priceRef: string
  onClose: () => void
}) {
  const { text, id: adminId } = useAdminUI()
  const { adminQueries, importQueries, importMutations, toast, toastApiError } =
    useAdminServices()
  const queryClient = useQueryClient()
  const observed = useQuery(importQueries.providerPrice(psp.id, priceRef))
  const products = useQuery(adminQueries.allProducts()).data?.data.filter(
    (p) => !p.archived
  )
  const [productId, setProductId] = React.useState("")
  const [key, setKey] = React.useState(`${psp.key}-${priceRef}`)
  const [offer, setOffer] = React.useState(false)
  const map = useMutation(importMutations.mapPrice(queryClient))
  const product = products?.find((p) => p.id === productId)
  const object = observed.data
  return (
    <Dialog open onOpenChange={(open) => !open && onClose()}>
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            <AdminMessage id="Map provider price" />
          </DialogTitle>
          <DialogDescription>
            {psp.key} · {priceRef}
            {object?.name ? ` · ${object.name}` : ""}
          </DialogDescription>
        </DialogHeader>
        <dl className="grid grid-cols-[8rem_1fr] gap-1 text-sm">
          <dt className="text-muted-foreground">
            <AdminMessage id="Terms" />
          </dt>
          <dd data-testid="map-terms">
            {observed.isPending
              ? "…"
              : object
                ? termsText(object.terms, psp.rail)
                : text("Not observed yet: read from the provider when mapped")}
          </dd>
          {object?.retired && (
            <>
              <dt className="text-muted-foreground">
                <AdminMessage id="Retired" />
              </dt>
              <dd>
                <AdminMessage id="The provider retired it: it keeps billing its subscriptions and is never sold." />
              </dd>
            </>
          )}
        </dl>
        <div className="grid gap-1.5">
          <Label htmlFor={adminId("map-product")}>
            <AdminMessage id="Product" />
          </Label>
          <select
            id={adminId("map-product")}
            className={selectClass}
            value={productId}
            onChange={(e) => setProductId(e.target.value)}
          >
            <option value="">{text("Choose a product")}</option>
            {products?.map((p) => (
              <option key={p.id} value={p.id}>
                {p.display_name} ({p.key})
              </option>
            ))}
          </select>
          {product && (
            <p className="text-sm" data-testid="map-entitlements">
              <AdminMessage id="Grants: " />
              {product.entitlements.length
                ? product.entitlements.join(", ")
                : text("no entitlements")}
            </p>
          )}
        </div>
        <div className="grid gap-1.5">
          <Label htmlFor={adminId("map-key")}>
            <AdminMessage id="Price key" />
          </Label>
          <Input
            id={adminId("map-key")}
            value={key}
            onChange={(e) => setKey(e.target.value)}
          />
        </div>
        <label className="flex items-center gap-2 text-sm">
          <input
            type="checkbox"
            checked={offer}
            onChange={(e) => setOffer(e.target.checked)}
          />
          <AdminMessage id="Offer as a tier-change target" />
        </label>
        <DialogFooter>
          <Button variant="outline" onClick={onClose}>
            <AdminMessage id="Cancel" />
          </Button>
          <Button
            disabled={!productId || !key.trim() || map.isPending}
            onClick={async () => {
              try {
                await map.mutateAsync({
                  product_id: productId,
                  key: key.trim(),
                  psp: psp.key,
                  ...(psp.rail === "nmi"
                    ? { plan_id: priceRef }
                    : { price_id: priceRef }),
                  archived: !offer,
                })
                toast.success(text("Price mapped"))
                onClose()
              } catch (err) {
                toastApiError(err, text("Map provider price"))
              }
            }}
          >
            <AdminMessage id="Map" />
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
