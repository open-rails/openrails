import { HugeiconsIcon } from "@hugeicons/react"
import {
  Add01Icon,
  Refresh01Icon,
  Upload01Icon,
} from "@hugeicons/core-free-icons"
import * as React from "react"
import { Link, useSearchParams } from "react-router-dom"
import { toast } from "sonner"
import { useForm } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { FormFieldErrors } from "@/components/form-field-errors"
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
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "@/components/ui/select"
import { Skeleton } from "@/components/ui/skeleton"
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
import { getBootstrap } from "@/lib/api/client"
import type { CatalogPrice, CatalogProduct } from "@/lib/api/types"
import {
  formatDate,
  formatMicros,
  microsFromInput,
  shortId,
} from "@/lib/format"
import { toastApiError } from "@/lib/toast"
import { adminMutations } from "@/lib/mutations"
import { CatalogCopilotPanel } from "@/pages/catalog/copilot-panel"
import { priceIntervalLabel } from "@/pages/catalog/price-format"
import { PriceChangeWizard } from "@/pages/catalog/price-wizard"
import { adminQueries } from "@/lib/queries"

const LINE_TAB =
  "flex-none px-0 after:bg-primary group-data-horizontal/tabs:after:bottom-[-1px]"

function catalogCopilotEnabled(): boolean {
  try {
    return getBootstrap().catalog_copilot_enabled
  } catch {
    return false
  }
}

function catalogDraftingEnabled(): boolean {
  try {
    return getBootstrap().catalog_drafting_enabled
  } catch {
    return false
  }
}

export function CatalogPage() {
  const [params, setParams] = useSearchParams()
  const tab = params.get("tab") || "products"

  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-2xl font-semibold tracking-tight">Catalog</h1>
      <CatalogCopilotPanel
        enabled={catalogCopilotEnabled()}
        draftingEnabled={catalogDraftingEnabled()}
      />
      <Tabs
        value={tab}
        onValueChange={(v) => {
          const p = new URLSearchParams(params)
          if (!v || v === "products") p.delete("tab")
          else p.set("tab", v)
          setParams(p)
        }}
        className="flex flex-col gap-4"
      >
        <TabsList
          variant="line"
          className="w-full justify-start gap-6 rounded-none p-0"
        >
          <TabsTrigger value="products" className={LINE_TAB}>
            Products
          </TabsTrigger>
          <TabsTrigger value="prices" className={LINE_TAB}>
            Prices
          </TabsTrigger>
          <TabsTrigger value="drift" className={LINE_TAB}>
            Drift
          </TabsTrigger>
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
    </div>
  )
}

function ProductsTab() {
  const { data, isPending: loading } = useQuery(
    adminQueries.products({ errorAction: "Load products" })
  )

  return (
    <div className="flex flex-col gap-3">
      <div className="flex justify-end gap-2">
        <PublishDialog />
        <ProductDialog />
      </div>
      {loading ? (
        <div className="flex flex-col gap-3 py-2">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-6 w-full" />
          ))}
        </div>
      ) : (
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
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
                <ProductRow key={p.id} product={p} />
              ))}
              {!data?.items?.length && (
                <TableRow>
                  <TableCell
                    colSpan={6}
                    className="h-24 text-center text-muted-foreground"
                  >
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

function ProductRow({ product }: { product: CatalogProduct }) {
  const queryClient = useQueryClient()
  const setProductActive = useMutation(
    adminMutations.setProductActive(queryClient)
  )
  const toggle = async () => {
    try {
      await setProductActive.mutateAsync({
        id: product.id,
        active: product.archived,
      })
      toast.success(
        product.archived ? "Product activated" : "Product deactivated"
      )
    } catch (err) {
      toastApiError(err, "Toggle product")
    }
  }
  return (
    <TableRow className={product.archived ? "opacity-60" : undefined}>
      <TableCell className="font-medium">{product.display_name}</TableCell>
      <TableCell className="text-xs text-muted-foreground">
        {product.key}
      </TableCell>
      <TableCell>
        {product.tier_group
          ? `${product.tier_group} #${product.tier_rank}`
          : "—"}
      </TableCell>
      <TableCell>
        <span className="flex flex-wrap gap-1">
          {Object.keys(product.entitlements_spec ?? {}).map((e) => (
            <Badge key={e} variant="secondary" className="text-[10px]">
              {e}
            </Badge>
          ))}
        </span>
      </TableCell>
      <TableCell>
        {product.archived ? (
          <Badge variant="secondary">archived</Badge>
        ) : (
          <StatusBadge status="active" />
        )}
      </TableCell>
      <TableCell className="text-right">
        <div className="flex justify-end gap-2">
          <ProductDialog product={product} />
          <Button
            variant="outline"
            size="sm"
            disabled={setProductActive.isPending}
            onClick={toggle}
          >
            {product.archived ? "Activate" : "Deactivate"}
          </Button>
        </div>
      </TableCell>
    </TableRow>
  )
}

function productFormValues(product?: CatalogProduct) {
  return {
    key: product?.key ?? "",
    name: product?.display_name ?? "",
    description: product?.description ?? "",
    tierGroup: product?.tier_group ?? "",
    tierRank: String(product?.tier_rank ?? 0),
    entitlements: Object.keys(product?.entitlements_spec ?? {}).join(","),
  }
}

function ProductDialog({ product }: { product?: CatalogProduct }) {
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const createProduct = useMutation(adminMutations.createProduct(queryClient))
  const updateProduct = useMutation(adminMutations.updateProduct(queryClient))
  const form = useForm({
    defaultValues: productFormValues(product),
    onSubmit: async ({ value }) => {
      const spec: Record<string, number | null> = {}
      for (const entitlement of value.entitlements
        .split(",")
        .map((item) => item.trim())
        .filter(Boolean)) {
        spec[entitlement] = null
      }

      try {
        if (product) {
          await updateProduct.mutateAsync({
            id: product.id,
            product: {
              display_name: value.name,
              description: value.description,
              tier_group: value.tierGroup || undefined,
              tier_rank: Number(value.tierRank) || 0,
              entitlements_spec: spec,
              set_entitlements: true,
            },
          })
          toast.success("Product updated")
        } else {
          await createProduct.mutateAsync({
            key: value.key,
            display_name: value.name,
            description: value.description,
            tier_group: value.tierGroup || undefined,
            tier_rank: Number(value.tierRank) || 0,
            entitlements_spec: spec,
          })
          toast.success("Product created")
        }
        form.reset(value)
        setOpen(false)
      } catch (err) {
        toastApiError(err, product ? "Update product" : "Create product")
      }
    },
  })

  const handleOpenChange = (next: boolean) => {
    if (next) form.reset(productFormValues(product))
    setOpen(next)
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger
        render={
          product ? (
            <Button variant="outline" size="sm">
              Edit
            </Button>
          ) : (
            <Button size="sm">
              <HugeiconsIcon icon={Add01Icon} className="size-4" /> Product
            </Button>
          )
        }
      />
      <DialogContent>
        <DialogHeader>
          <DialogTitle>{product ? "Edit product" : "New product"}</DialogTitle>
          <DialogDescription>
            Entitlements are plain strings; comma-separate several.
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(event) => {
            event.preventDefault()
            event.stopPropagation()
            void form.handleSubmit()
          }}
          className="grid gap-4"
        >
          <div className="grid gap-3">
            {!product && (
              <form.Field
                name="key"
                validators={{
                  onBlur: ({ value }) =>
                    value.trim() ? undefined : "Enter a product key",
                }}
              >
                {(field) => (
                  <Field label="Key" id="p-key">
                    <Input
                      id="p-key"
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(event) =>
                        field.handleChange(event.target.value)
                      }
                      placeholder="premium-monthly"
                      aria-invalid={field.state.meta.errors.length > 0}
                    />
                    <FormFieldErrors errors={field.state.meta.errors} />
                  </Field>
                )}
              </form.Field>
            )}
            <form.Field
              name="name"
              validators={{
                onBlur: ({ value }) =>
                  value.trim() ? undefined : "Enter a display name",
              }}
            >
              {(field) => (
                <Field label="Display name" id="p-name">
                  <Input
                    id="p-name"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    aria-invalid={field.state.meta.errors.length > 0}
                  />
                  <FormFieldErrors errors={field.state.meta.errors} />
                </Field>
              )}
            </form.Field>
            <form.Field name="description">
              {(field) => (
                <Field label="Description" id="p-desc">
                  <Input
                    id="p-desc"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                  />
                </Field>
              )}
            </form.Field>
            <div className="grid grid-cols-2 gap-3">
              <form.Field name="tierGroup">
                {(field) => (
                  <Field label="Tier group (optional)" id="p-tg">
                    <Input
                      id="p-tg"
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(event) =>
                        field.handleChange(event.target.value)
                      }
                    />
                  </Field>
                )}
              </form.Field>
              <form.Field name="tierRank">
                {(field) => (
                  <Field label="Tier rank" id="p-tr">
                    <Input
                      id="p-tr"
                      type="number"
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(event) =>
                        field.handleChange(event.target.value)
                      }
                    />
                  </Field>
                )}
              </form.Field>
            </div>
            <form.Field name="entitlements">
              {(field) => (
                <Field label="Entitlements (comma-separated)" id="p-ents">
                  <Input
                    id="p-ents"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    placeholder="premium, downloads"
                  />
                </Field>
              )}
            </form.Field>
          </div>
          <DialogFooter>
            <form.Subscribe
              selector={(state) =>
                [
                  state.values.key,
                  state.values.name,
                  state.isSubmitting,
                ] as const
              }
            >
              {([key, name, isSubmitting]) => (
                <Button
                  type="submit"
                  disabled={
                    isSubmitting || !name.trim() || (!product && !key.trim())
                  }
                >
                  {isSubmitting ? "Saving…" : "Save"}
                </Button>
              )}
            </form.Subscribe>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function PricesTab() {
  const { data: products } = useQuery(adminQueries.products())
  const { data, isPending: loading } = useQuery(
    adminQueries.prices({ errorAction: "Load prices" })
  )
  const productName = (id: string) =>
    products?.items.find((p) => p.id === id)?.display_name ?? shortId(id, 13)

  return (
    <div className="flex flex-col gap-3">
      <div className="flex justify-end">
        <PriceDialog products={products?.items ?? []} />
      </div>
      {loading ? (
        <div className="flex flex-col gap-3 py-2">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-6 w-full" />
          ))}
        </div>
      ) : (
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
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
                <PriceRow
                  key={price.id}
                  price={price}
                  productName={productName(price.product_id)}
                />
              ))}
              {!data?.items?.length && (
                <TableRow>
                  <TableCell
                    colSpan={7}
                    className="h-24 text-center text-muted-foreground"
                  >
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
}: {
  price: CatalogPrice
  productName: string
}) {
  const queryClient = useQueryClient()
  const setPriceActive = useMutation(adminMutations.setPriceActive(queryClient))
  const toggle = async () => {
    try {
      await setPriceActive.mutateAsync({
        id: price.id,
        active: price.archived,
      })
      toast.success(price.archived ? "Price activated" : "Price deactivated")
    } catch (err) {
      toastApiError(err, "Toggle price")
    }
  }
  return (
    <TableRow className={price.archived ? "opacity-60" : undefined}>
      <TableCell className="text-xs text-muted-foreground">
        <Link
          className="underline-offset-2 hover:underline"
          to={`/catalog/prices/${price.id}`}
        >
          {shortId(price.id, 13)}
        </Link>
      </TableCell>
      <TableCell className="font-medium">{productName}</TableCell>
      <TableCell className="tabular-nums">
        {formatMicros(price.unit_amount, price.currency)}
      </TableCell>
      <TableCell>{priceIntervalLabel(price)}</TableCell>
      <TableCell>
        <span className="flex flex-wrap gap-1">
          {/* psp_links is keyed by PSP key ("mobius"), not by rail — the rail
              is recorded inside the entry (or#812). */}
          {Object.entries(price.providers ?? {}).map(([psp, state]) => (
            <Badge
              key={psp}
              variant="secondary"
              className={
                state.status === "linked" ? "" : "bg-held-surface text-held"
              }
              title={state.message}
            >
              {psp}: {state.status}
            </Badge>
          ))}
        </span>
      </TableCell>
      <TableCell>
        {price.archived ? (
          <Badge variant="secondary">archived</Badge>
        ) : (
          <StatusBadge status="active" />
        )}
      </TableCell>
      <TableCell className="text-right">
        <div className="flex justify-end gap-2">
          <PriceChangeWizard price={price} productName={productName} />
          <Button
            variant="outline"
            size="sm"
            disabled={setPriceActive.isPending}
            onClick={toggle}
          >
            {price.archived ? "Activate" : "Deactivate"}
          </Button>
        </div>
      </TableCell>
    </TableRow>
  )
}

function PriceDialog({ products }: { products: CatalogProduct[] }) {
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const createPrice = useMutation(adminMutations.createPrice(queryClient))
  const form = useForm({
    defaultValues: {
      productId: "",
      amount: "",
      currency: "usd",
      durationHours: "",
      autoRenew: true,
    },
    onSubmit: async ({ value }) => {
      try {
        await createPrice.mutateAsync({
          product_id: value.productId,
          unit_amount: microsFromInput(value.amount) ?? 0,
          currency: value.currency,
          ...(value.durationHours
            ? { access_duration_hours: Number(value.durationHours) }
            : {}),
          auto_renew: value.autoRenew,
        })
        form.reset()
        toast.success("Price created")
        setOpen(false)
      } catch (err) {
        toastApiError(err, "Create price")
      }
    },
  })

  const handleOpenChange = (next: boolean) => {
    if (next) form.reset()
    setOpen(next)
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger
        render={
          <Button size="sm">
            <HugeiconsIcon icon={Add01Icon} className="size-4" /> Price
          </Button>
        }
      />
      <DialogContent>
        <DialogHeader>
          <DialogTitle>New price</DialogTitle>
          <DialogDescription>
            Financial terms are immutable after creation: a price change is a
            new price plus archiving the old one.
          </DialogDescription>
        </DialogHeader>
        <form
          onSubmit={(event) => {
            event.preventDefault()
            event.stopPropagation()
            void form.handleSubmit()
          }}
          className="grid gap-4"
        >
          <div className="grid gap-3">
            <form.Field name="productId">
              {(field) => (
                <Field label="Product" id="pr-prod">
                  <Select
                    items={products.map((product) => ({
                      value: product.id,
                      label: `${product.display_name} (${product.key})`,
                    }))}
                    value={field.state.value || null}
                    onValueChange={(value) => field.handleChange(value ?? "")}
                  >
                    <SelectTrigger id="pr-prod" className="w-full">
                      <SelectValue placeholder="Pick a product…" />
                    </SelectTrigger>
                    <SelectContent>
                      {products.map((product) => (
                        <SelectItem key={product.id} value={product.id}>
                          {product.display_name} ({product.key})
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                </Field>
              )}
            </form.Field>
            <div className="grid grid-cols-2 gap-3">
              <form.Field
                name="amount"
                validators={{
                  onBlur: ({ value }) =>
                    microsFromInput(value)
                      ? undefined
                      : "Enter an amount greater than zero",
                }}
              >
                {(field) => (
                  <Field label="Amount (major units)" id="pr-amount">
                    <Input
                      id="pr-amount"
                      type="number"
                      step="any"
                      min="0"
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(event) =>
                        field.handleChange(event.target.value)
                      }
                      aria-invalid={field.state.meta.errors.length > 0}
                    />
                    <FormFieldErrors errors={field.state.meta.errors} />
                  </Field>
                )}
              </form.Field>
              <form.Field name="currency">
                {(field) => (
                  <Field label="Currency" id="pr-cur">
                    <Input
                      id="pr-cur"
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(event) =>
                        field.handleChange(event.target.value)
                      }
                    />
                  </Field>
                )}
              </form.Field>
            </div>
            <div className="grid grid-cols-2 gap-3">
              <form.Field name="durationHours">
                {(field) => (
                  <Field
                    label="Access duration (hours, empty = durable)"
                    id="pr-dur"
                  >
                    <Input
                      id="pr-dur"
                      type="number"
                      min="1"
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(event) =>
                        field.handleChange(event.target.value)
                      }
                    />
                  </Field>
                )}
              </form.Field>
              <form.Field name="autoRenew">
                {(field) => (
                  <div className="flex items-end gap-2 pb-1">
                    <Switch
                      id="pr-renew"
                      checked={field.state.value}
                      onCheckedChange={field.handleChange}
                    />
                    <Label htmlFor="pr-renew">Auto-renew</Label>
                  </div>
                )}
              </form.Field>
            </div>
          </div>
          <DialogFooter>
            <form.Subscribe
              selector={(state) =>
                [
                  state.values.productId,
                  state.values.amount,
                  state.isSubmitting,
                ] as const
              }
            >
              {([productId, amount, isSubmitting]) => (
                <Button
                  type="submit"
                  disabled={
                    isSubmitting || !productId || !microsFromInput(amount)
                  }
                >
                  {isSubmitting ? "Creating…" : "Create"}
                </Button>
              )}
            </form.Subscribe>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function DriftTab() {
  const queryClient = useQueryClient()
  const { data, isPending: loading } = useQuery(adminQueries.catalogDrift())
  const refreshDrift = useMutation(
    adminMutations.refreshCatalogDrift(queryClient)
  )

  return (
    <div className="flex flex-col gap-3">
      <div className="flex justify-end">
        <Button
          variant="outline"
          size="sm"
          disabled={refreshDrift.isPending}
          onClick={async () => {
            try {
              const report = await refreshDrift.mutateAsync()
              toast.success(
                `Drift scan done: ${report.new_events} new, ${report.resolved_events} resolved`
              )
            } catch (err) {
              toastApiError(err, "Refresh drift")
            }
          }}
        >
          <HugeiconsIcon
            icon={Refresh01Icon}
            className={
              refreshDrift.isPending ? "size-4 animate-spin" : "size-4"
            }
          />
          {""}
          Refresh scan
        </Button>
      </div>
      {loading ? (
        <div className="flex flex-col gap-3 py-2">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-6 w-full" />
          ))}
        </div>
      ) : !data?.items?.length ? (
        <p className="text-sm text-muted-foreground">
          No open drift events. Catalog is in sync.
        </p>
      ) : (
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
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
                  <TableCell className="text-xs">
                    {e.openrails_resource_type}
                    {""}
                    {shortId(
                      e.openrails_resource_id ?? e.external_resource_id ?? "",
                      13
                    )}
                  </TableCell>
                  <TableCell>{e.field ?? "—"}</TableCell>
                  <TableCell className="max-w-40 truncate">
                    {e.openrails_value ?? "—"}
                  </TableCell>
                  <TableCell className="max-w-40 truncate">
                    {e.external_value ?? "—"}
                  </TableCell>
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

function PublishDialog() {
  const [open, setOpen] = React.useState(false)
  const [plan, setPlan] = React.useState<string>()
  const queryClient = useQueryClient()
  const publish = useMutation(adminMutations.publishCatalog(queryClient))

  const run = async (manifest: string, planOnly: boolean) => {
    let parsed: unknown
    try {
      parsed = JSON.parse(manifest)
    } catch {
      toast.error("Manifest must be valid JSON")
      return
    }
    try {
      const res = await publish.mutateAsync({ manifest: parsed, planOnly })
      setPlan(JSON.stringify(res, null, 2))
      if (!planOnly) {
        toast.success("Catalog published")
      }
    } catch (err) {
      toastApiError(err, planOnly ? "Plan catalog publish" : "Publish catalog")
    }
  }
  const form = useForm({
    defaultValues: { manifest: "" },
    onSubmit: async ({ value }) => run(value.manifest, false),
  })

  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger
        render={
          <Button variant="outline" size="sm">
            <HugeiconsIcon icon={Upload01Icon} className="size-4" /> Publish
          </Button>
        }
      />
      <DialogContent className="max-w-2xl">
        <form
          className="grid gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            event.stopPropagation()
            void form.handleSubmit()
          }}
        >
          <DialogHeader>
            <DialogTitle>Publish catalog manifest</DialogTitle>
            <DialogDescription>
              Terraform-style: paste the catalog manifest JSON, preview the
              plan, then apply (insert + overwrite).
            </DialogDescription>
          </DialogHeader>
          <form.Field name="manifest">
            {(field) => (
              <Textarea
                className="min-h-40 text-xs"
                placeholder='{"groups": ...}'
                value={field.state.value}
                onBlur={field.handleBlur}
                onChange={(event) => field.handleChange(event.target.value)}
              />
            )}
          </form.Field>
          {plan && (
            <pre className="max-h-60 overflow-auto rounded-md bg-muted p-3 text-xs">
              {plan}
            </pre>
          )}
          <DialogFooter>
            <form.Subscribe selector={(state) => state.values.manifest}>
              {(manifest) => (
                <>
                  <Button
                    type="button"
                    variant="outline"
                    disabled={publish.isPending || !manifest.trim()}
                    onClick={() => void run(manifest, true)}
                  >
                    Preview plan
                  </Button>
                  <Button
                    type="submit"
                    disabled={publish.isPending || !manifest.trim()}
                  >
                    {publish.isPending ? "Working…" : "Apply"}
                  </Button>
                </>
              )}
            </form.Subscribe>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

function Field({
  label,
  id,
  children,
}: {
  label: string
  id: string
  children: React.ReactNode
}) {
  return (
    <div className="grid gap-1.5">
      <Label htmlFor={id}>{label}</Label>
      {children}
    </div>
  )
}
