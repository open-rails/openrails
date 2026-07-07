import * as React from "react"
import { PlusIcon, RefreshCwIcon, UploadIcon } from "lucide-react"
import { Link } from "react-router-dom"
import { toast } from "sonner"

import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Switch } from "@/components/ui/switch"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs"
import { Textarea } from "@/components/ui/textarea"
import { useApiData } from "@/hooks/use-api-data"
import {
  activatePrice,
  activateProduct,
  createPrice,
  createProduct,
  deactivatePrice,
  deactivateProduct,
  listCatalogDrift,
  listPrices,
  listProducts,
  publishCatalog,
  refreshCatalogDrift,
  updateProduct,
} from "@/lib/api/endpoints"
import type { CatalogPrice, CatalogProduct } from "@/lib/api/types"
import { formatDate, formatMicros, microsFromInput, shortId } from "@/lib/format"
import { toastApiError } from "@/lib/toast"
import { priceIntervalLabel } from "@/pages/catalog/price-format"
import { PriceChangeWizard } from "@/pages/catalog/price-wizard"

export function CatalogPage() {
  return (
    <Tabs defaultValue="products" className="flex flex-col gap-4">
      <TabsList>
        <TabsTrigger value="products">Products</TabsTrigger>
        <TabsTrigger value="prices">Prices</TabsTrigger>
        <TabsTrigger value="drift">Drift</TabsTrigger>
      </TabsList>
      <TabsContent value="products">
        <ProductsTab />
      </TabsContent>
      <TabsContent value="prices">
        <PricesTab />
      </TabsContent>
      <TabsContent value="drift">
        <DriftTab />
      </TabsContent>
    </Tabs>
  )
}

function ProductsTab() {
  const { data, loading, error, reload } = useApiData(() => listProducts(1000, 0), [])
  React.useEffect(() => {
    if (error) toastApiError(error, "Load products")
  }, [error])

  return (
    <div className="flex flex-col gap-3">
      <div className="flex justify-end gap-2">
        <PublishDialog onDone={reload} />
        <ProductDialog onDone={reload} />
      </div>
      {loading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : (
        <div className="overflow-x-auto rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Product</TableHead>
                <TableHead>Key</TableHead>
                <TableHead>Tier</TableHead>
                <TableHead>Entitlements</TableHead>
                <TableHead>State</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {(data?.items ?? []).map((p) => (
                <ProductRow key={p.id} product={p} onDone={reload} />
              ))}
              {!data?.items?.length && (
                <TableRow>
                  <TableCell colSpan={6} className="h-24 text-center text-muted-foreground">
                    No products yet.
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  )
}

function ProductRow({ product, onDone }: { product: CatalogProduct; onDone: () => void }) {
  const [busy, setBusy] = React.useState(false)
  const toggle = async () => {
    setBusy(true)
    try {
      if (product.archived) await activateProduct(product.id)
      else await deactivateProduct(product.id)
      toast.success(product.archived ? "Product activated" : "Product deactivated")
      onDone()
    } catch (err) {
      toastApiError(err, "Toggle product")
    } finally {
      setBusy(false)
    }
  }
  return (
    <TableRow className={product.archived ? "opacity-60" : undefined}>
      <TableCell className="font-medium">{product.display_name}</TableCell>
      <TableCell className="font-mono text-xs">{product.key}</TableCell>
      <TableCell>{product.tier_group ? `${product.tier_group} #${product.tier_rank}` : "—"}</TableCell>
      <TableCell>
        <span className="flex flex-wrap gap-1">
          {Object.keys(product.entitlements_spec ?? {}).map((e) => (
            <Badge key={e} variant="secondary" className="text-[10px]">
              {e}
            </Badge>
          ))}
        </span>
      </TableCell>
      <TableCell>{product.archived ? <Badge variant="secondary">archived</Badge> : <StatusBadge status="active" />}</TableCell>
      <TableCell className="text-right">
        <div className="flex justify-end gap-2">
          <ProductDialog product={product} onDone={onDone} />
          <Button variant="outline" size="sm" disabled={busy} onClick={toggle}>
            {product.archived ? "Activate" : "Deactivate"}
          </Button>
        </div>
      </TableCell>
    </TableRow>
  )
}

function ProductDialog({ product, onDone }: { product?: CatalogProduct; onDone: () => void }) {
  const [open, setOpen] = React.useState(false)
  const [key, setKey] = React.useState(product?.key ?? "")
  const [name, setName] = React.useState(product?.display_name ?? "")
  const [description, setDescription] = React.useState(product?.description ?? "")
  const [tierGroup, setTierGroup] = React.useState(product?.tier_group ?? "")
  const [tierRank, setTierRank] = React.useState(String(product?.tier_rank ?? 0))
  const [entitlements, setEntitlements] = React.useState(
    Object.keys(product?.entitlements_spec ?? {}).join(", "),
  )
  const [busy, setBusy] = React.useState(false)

  const save = async () => {
    setBusy(true)
    try {
      const spec: Record<string, number | null> = {}
      for (const e of entitlements.split(",").map((s) => s.trim()).filter(Boolean)) spec[e] = null
      if (product) {
        await updateProduct(product.id, {
          display_name: name,
          description,
          tier_group: tierGroup || undefined,
          tier_rank: Number(tierRank) || 0,
          entitlements_spec: spec,
          set_entitlements: true,
        })
        toast.success("Product updated")
      } else {
        await createProduct({
          key,
          display_name: name,
          description,
          tier_group: tierGroup || undefined,
          tier_rank: Number(tierRank) || 0,
          entitlements_spec: spec,
        })
        toast.success("Product created")
      }
      setOpen(false)
      onDone()
    } catch (err) {
      toastApiError(err, product ? "Update product" : "Create product")
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        {product ? (
          <Button variant="outline" size="sm">Edit</Button>
        ) : (
          <Button size="sm">
            <PlusIcon className="size-4" /> Product
          </Button>
        )}
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{product ? "Edit product" : "New product"}</DialogTitle>
          <DialogDescription>
            Entitlements are plain strings; comma-separate several.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3">
          {!product && (
            <Field label="Key" id="p-key">
              <Input id="p-key" value={key} onChange={(e) => setKey(e.target.value)} placeholder="premium-monthly" />
            </Field>
          )}
          <Field label="Display name" id="p-name">
            <Input id="p-name" value={name} onChange={(e) => setName(e.target.value)} />
          </Field>
          <Field label="Description" id="p-desc">
            <Input id="p-desc" value={description} onChange={(e) => setDescription(e.target.value)} />
          </Field>
          <div className="grid grid-cols-2 gap-3">
            <Field label="Tier group (optional)" id="p-tg">
              <Input id="p-tg" value={tierGroup} onChange={(e) => setTierGroup(e.target.value)} />
            </Field>
            <Field label="Tier rank" id="p-tr">
              <Input id="p-tr" type="number" value={tierRank} onChange={(e) => setTierRank(e.target.value)} />
            </Field>
          </div>
          <Field label="Entitlements (comma-separated)" id="p-ents">
            <Input id="p-ents" value={entitlements} onChange={(e) => setEntitlements(e.target.value)} placeholder="premium, downloads" />
          </Field>
        </div>
        <DialogFooter>
          <Button disabled={busy || !name || (!product && !key)} onClick={save}>
            {busy ? "Saving…" : "Save"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function PricesTab() {
  const { data: products } = useApiData(() => listProducts(1000, 0), [])
  const { data, loading, error, reload } = useApiData(() => listPrices(), [])
  React.useEffect(() => {
    if (error) toastApiError(error, "Load prices")
  }, [error])
  const productName = (id: string) =>
    products?.items.find((p) => p.id === id)?.display_name ?? shortId(id, 13)

  return (
    <div className="flex flex-col gap-3">
      <div className="flex justify-end">
        <PriceDialog products={products?.items ?? []} onDone={reload} />
      </div>
      {loading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : (
        <div className="overflow-x-auto rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Price</TableHead>
                <TableHead>Product</TableHead>
                <TableHead>Amount</TableHead>
                <TableHead>Renews</TableHead>
                <TableHead>Providers</TableHead>
                <TableHead>State</TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {(data?.items ?? []).map((price) => (
                <PriceRow key={price.id} price={price} productName={productName(price.product_id)} onDone={reload} />
              ))}
              {!data?.items?.length && (
                <TableRow>
                  <TableCell colSpan={7} className="h-24 text-center text-muted-foreground">
                    No prices yet.
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  )
}

function PriceRow({
  price,
  productName,
  onDone,
}: {
  price: CatalogPrice
  productName: string
  onDone: () => void
}) {
  const [busy, setBusy] = React.useState(false)
  const toggle = async () => {
    setBusy(true)
    try {
      if (price.archived) await activatePrice(price.id)
      else await deactivatePrice(price.id)
      toast.success(price.archived ? "Price activated" : "Price deactivated")
      onDone()
    } catch (err) {
      toastApiError(err, "Toggle price")
    } finally {
      setBusy(false)
    }
  }
  return (
    <TableRow className={price.archived ? "opacity-60" : undefined}>
      <TableCell className="font-mono text-xs">
        <Link className="underline-offset-2 hover:underline" to={`/catalog/prices/${price.id}`}>
          {shortId(price.id, 13)}
        </Link>
      </TableCell>
      <TableCell>{productName}</TableCell>
      <TableCell>{formatMicros(price.unit_amount, price.currency)}</TableCell>
      <TableCell>{priceIntervalLabel(price)}</TableCell>
      <TableCell>
        <span className="flex flex-wrap gap-1">
          {Object.entries(price.providers ?? {}).map(([rail, state]) => (
            <Badge
              key={rail}
              variant="secondary"
              className={state.status === "linked" ? "" : "bg-amber-500/15 text-amber-600 dark:text-amber-400"}
              title={state.message}
            >
              {rail}: {state.status}
            </Badge>
          ))}
        </span>
      </TableCell>
      <TableCell>{price.archived ? <Badge variant="secondary">archived</Badge> : <StatusBadge status="active" />}</TableCell>
      <TableCell className="text-right">
        <div className="flex justify-end gap-2">
          <PriceChangeWizard price={price} productName={productName} onDone={onDone} />
          <Button variant="outline" size="sm" disabled={busy} onClick={toggle}>
            {price.archived ? "Activate" : "Deactivate"}
          </Button>
        </div>
      </TableCell>
    </TableRow>
  )
}

function PriceDialog({ products, onDone }: { products: CatalogProduct[]; onDone: () => void }) {
  const [open, setOpen] = React.useState(false)
  const [productId, setProductId] = React.useState("")
  const [amount, setAmount] = React.useState("")
  const [currency, setCurrency] = React.useState("usd")
  const [durationHours, setDurationHours] = React.useState("")
  const [autoRenew, setAutoRenew] = React.useState(true)
  const [busy, setBusy] = React.useState(false)
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button size="sm">
          <PlusIcon className="size-4" /> Price
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New price</DialogTitle>
          <DialogDescription>
            Financial terms are immutable after creation — a price change is a new price
            plus archiving the old one.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3">
          <Field label="Product" id="pr-prod">
            <select
              id="pr-prod"
              className="h-9 rounded-md border bg-transparent px-3 text-sm"
              value={productId}
              onChange={(e) => setProductId(e.target.value)}
            >
              <option value="">Pick a product…</option>
              {products.map((p) => (
                <option key={p.id} value={p.id}>
                  {p.display_name} ({p.key})
                </option>
              ))}
            </select>
          </Field>
          <div className="grid grid-cols-2 gap-3">
            <Field label="Amount (major units)" id="pr-amount">
              <Input id="pr-amount" type="number" step="any" min="0" value={amount} onChange={(e) => setAmount(e.target.value)} />
            </Field>
            <Field label="Currency" id="pr-cur">
              <Input id="pr-cur" value={currency} onChange={(e) => setCurrency(e.target.value)} />
            </Field>
          </div>
          <div className="grid grid-cols-2 gap-3">
            <Field label="Access duration (hours, empty = durable)" id="pr-dur">
              <Input id="pr-dur" type="number" min="1" value={durationHours} onChange={(e) => setDurationHours(e.target.value)} />
            </Field>
            <div className="flex items-end gap-2 pb-1">
              <Switch id="pr-renew" checked={autoRenew} onCheckedChange={setAutoRenew} />
              <Label htmlFor="pr-renew">Auto-renew</Label>
            </div>
          </div>
        </div>
        <DialogFooter>
          <Button
            disabled={busy || !productId || !microsFromInput(amount)}
            onClick={async () => {
              setBusy(true)
              try {
                await createPrice({
                  product_id: productId,
                  unit_amount: microsFromInput(amount) ?? 0,
                  currency,
                  ...(durationHours ? { access_duration_hours: Number(durationHours) } : {}),
                  auto_renew: autoRenew,
                })
                toast.success("Price created")
                setOpen(false)
                onDone()
              } catch (err) {
                toastApiError(err, "Create price")
              } finally {
                setBusy(false)
              }
            }}
          >
            {busy ? "Creating…" : "Create"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function DriftTab() {
  const { data, loading, error, reload } = useApiData(() => listCatalogDrift(200, 0), [])
  const [refreshing, setRefreshing] = React.useState(false)
  React.useEffect(() => {
    if (error) toastApiError(error, "Load drift")
  }, [error])

  return (
    <div className="flex flex-col gap-3">
      <div className="flex justify-end">
        <Button
          variant="outline"
          size="sm"
          disabled={refreshing}
          onClick={async () => {
            setRefreshing(true)
            try {
              const report = await refreshCatalogDrift()
              toast.success(
                `Drift scan done: ${report.new_events} new, ${report.resolved_events} resolved`,
              )
              reload()
            } catch (err) {
              toastApiError(err, "Refresh drift")
            } finally {
              setRefreshing(false)
            }
          }}
        >
          <RefreshCwIcon className={refreshing ? "size-4 animate-spin" : "size-4"} /> Refresh scan
        </Button>
      </div>
      {loading ? (
        <p className="text-sm text-muted-foreground">Loading…</p>
      ) : !data?.items?.length ? (
        <p className="text-sm text-muted-foreground">No open drift events — catalog is in sync.</p>
      ) : (
        <div className="overflow-x-auto rounded-md border">
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>Provider</TableHead>
                <TableHead>Kind</TableHead>
                <TableHead>Resource</TableHead>
                <TableHead>Field</TableHead>
                <TableHead>OpenRails</TableHead>
                <TableHead>Remote</TableHead>
                <TableHead>Detected</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.items.map((e) => (
                <TableRow key={e.id}>
                  <TableCell>{e.provider}</TableCell>
                  <TableCell>{e.kind}</TableCell>
                  <TableCell className="font-mono text-xs">
                    {e.openrails_resource_type} {shortId(e.openrails_resource_id ?? e.external_resource_id ?? "", 13)}
                  </TableCell>
                  <TableCell>{e.field ?? "—"}</TableCell>
                  <TableCell className="max-w-40 truncate">{e.openrails_value ?? "—"}</TableCell>
                  <TableCell className="max-w-40 truncate">{e.external_value ?? "—"}</TableCell>
                  <TableCell>{formatDate(e.detected_at)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  )
}

function PublishDialog({ onDone }: { onDone: () => void }) {
  const [open, setOpen] = React.useState(false)
  const [manifest, setManifest] = React.useState("")
  const [plan, setPlan] = React.useState<string>()
  const [busy, setBusy] = React.useState(false)

  const run = async (planOnly: boolean) => {
    let parsed: unknown
    try {
      parsed = JSON.parse(manifest)
    } catch {
      toast.error("Manifest must be valid JSON")
      return
    }
    setBusy(true)
    try {
      const res = await publishCatalog(parsed, planOnly ? { plan_only: true } : { insert: true, overwrite: true })
      setPlan(JSON.stringify(res, null, 2))
      if (!planOnly) {
        toast.success("Catalog published")
        onDone()
      }
    } catch (err) {
      toastApiError(err, planOnly ? "Plan catalog publish" : "Publish catalog")
    } finally {
      setBusy(false)
    }
  }

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button variant="outline" size="sm">
          <UploadIcon className="size-4" /> Publish
        </Button>
      </DialogTrigger>
      <DialogContent className="max-w-2xl">
        <DialogHeader>
          <DialogTitle>Publish catalog manifest</DialogTitle>
          <DialogDescription>
            Terraform-style: paste the catalog manifest JSON, preview the plan, then apply
            (insert + overwrite).
          </DialogDescription>
        </DialogHeader>
        <Textarea
          className="min-h-40 font-mono text-xs"
          placeholder='{"groups": ...}'
          value={manifest}
          onChange={(e) => setManifest(e.target.value)}
        />
        {plan && (
          <pre className="max-h-60 overflow-auto rounded-md bg-muted p-3 text-xs">{plan}</pre>
        )}
        <DialogFooter>
          <Button variant="outline" disabled={busy || !manifest.trim()} onClick={() => run(true)}>
            Preview plan
          </Button>
          <Button disabled={busy || !manifest.trim()} onClick={() => run(false)}>
            {busy ? "Working…" : "Apply"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function Field({ label, id, children }: { label: string; id: string; children: React.ReactNode }) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  )
}
