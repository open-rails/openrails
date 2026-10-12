import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import { HugeiconsIcon } from "@hugeicons/react"
import { Add01Icon, Refresh01Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { Link } from "#orck/admin/router"

import { useForm } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { FormFieldErrors } from "#orck/admin/components/form-field-errors"
import { CursorPager } from "#orck/admin/components/cursor-pager"
import { useCursorPages } from "#orck/admin/lib/cursor-pages"
import { LinkedTableRow } from "#orck/admin/components/linked-table-row"
import { StatusBadge } from "#orck/admin/components/status-badge"
import { Badge } from "#orck/admin/components/ui/badge"
import { Button } from "#orck/admin/components/ui/button"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "#orck/admin/components/ui/dialog"
import { Input } from "#orck/admin/components/ui/input"
import { Label } from "#orck/admin/components/ui/label"
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "#orck/admin/components/ui/select"
import { Skeleton } from "#orck/admin/components/ui/skeleton"
import { Switch } from "#orck/admin/components/ui/switch"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "#orck/admin/components/ui/table"
import { DIALOG_FORM } from "#orck/admin/lib/dialog-width"
import type { Price, Product } from "#orck/client/generated/wire"
import {
  currencyScale,
  formatDate,
  nativeAmountFromInput,
  shortId,
  supportedCurrencies,
} from "#orck/admin/lib/format"
import { parseHours } from "#orck/admin/lib/duration"

import { CatalogApplicationDialog } from "#orck/admin/pages/catalog/application-dialog"
import { CatalogCopilotPanel } from "#orck/admin/pages/catalog/copilot-panel"
import {
  priceAmountLabel,
  priceIntervalLabel,
} from "#orck/admin/pages/catalog/price-format"
import { PriceChangeWizard } from "#orck/admin/pages/catalog/price-wizard"
import {
  useAccess,
  useCatalogWrites,
  useMerchantConfig,
  useMerchantConfigUpdates,
} from "#orck/admin/lib/capabilities"

// Each part of the catalog is its own page, reached from the sub-items the
// sidebar reveals under Catalog. The shell around them is identical, so it
// lives here rather than being repeated three times.
const CatalogWritesContext = React.createContext(false)

function CatalogSection({
  title,
  children,
}: {
  title: string
  children: React.ReactNode
}) {
  const assistants = useAccess()?.assistants
  const writesAllowed = useCatalogWrites()
  return (
    <CatalogWritesContext.Provider value={writesAllowed}>
      <div className="flex flex-col gap-4">
        <h1 className="text-2xl font-semibold tracking-tight">{title}</h1>
        <CatalogCopilotPanel
          enabled={assistants?.catalog_copilot ?? false}
          draftingEnabled={
            writesAllowed && (assistants?.catalog_drafting ?? false)
          }
        />
        {!writesAllowed && (
          <p className="text-sm text-muted-foreground">
            <AdminMessage
              id={
                "Catalog updates are unavailable. You can still browse products and prices."
              }
            />
          </p>
        )}
        {children}
      </div>
    </CatalogWritesContext.Provider>
  )
}

export function CatalogProductsPage() {
  const { text: adminText } = useAdminUI()

  return (
    <CatalogSection title={adminText("Products")}>
      <ProductsTab />
    </CatalogSection>
  )
}

export function CatalogPricesPage() {
  const { text: adminText } = useAdminUI()

  return (
    <CatalogSection title={adminText("Prices")}>
      <PricesTab />
    </CatalogSection>
  )
}

export function CatalogDriftPage() {
  const { text: adminText } = useAdminUI()
  const merchantConfig = useMerchantConfig()
  if (!merchantConfig) return null
  return (
    <div className="flex flex-col gap-4">
      <h1 className="text-2xl font-semibold tracking-tight">
        {adminText("Drift")}
      </h1>
      <DriftTab />
    </div>
  )
}

const CATALOG_PAGE_SIZE = 100

function ProductsTab() {
  const { adminQueries } = useAdminServices()

  const writesAllowed = React.useContext(CatalogWritesContext)
  const pages = useCursorPages("", CATALOG_PAGE_SIZE)
  const {
    data,
    isPending: loading,
    isFetching,
  } = useQuery(
    adminQueries.products({
      limit: pages.size,
      cursor: pages.cursor,
      errorAction: "Load products",
    })
  )

  return (
    <div className="flex flex-col gap-3">
      <div className="flex justify-end gap-2">
        {writesAllowed && (
          <>
            <CatalogApplicationDialog />
            <ProductDialog />
          </>
        )}
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
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Product"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Key"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Tier"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Entitlements"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"State"} />
                </TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {(data?.data ?? []).map((p) => (
                <ProductRow key={p.id} product={p} />
              ))}
              {!data?.data?.length && (
                <TableRow>
                  <TableCell
                    colSpan={6}
                    className="h-24 text-center text-muted-foreground"
                  >
                    <AdminMessage id={"No products yet."} />
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </div>
      )}
      <CursorPager
        pages={pages}
        rows={data?.data?.length ?? 0}
        nextCursor={data?.next_cursor}
        busy={isFetching}
        sizes={[50, 100, 200]}
      />
    </div>
  )
}

function ProductRow({ product }: { product: Product }) {
  const { toast, toastApiError, adminMutations } = useAdminServices()

  const writesAllowed = React.useContext(CatalogWritesContext)
  const queryClient = useQueryClient()
  const setProductActive = useMutation(
    adminMutations.setProductActive(queryClient)
  )
  const toggle = async () => {
    try {
      await setProductActive.mutateAsync({
        id: product.id,
        active: product.archived,
        revision: product.revision,
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
          {product.entitlements.map((e) => (
            <Badge key={e} variant="secondary" className="text-[10px]">
              {e}
            </Badge>
          ))}
        </span>
      </TableCell>
      <TableCell>
        {product.archived ? (
          <Badge variant="secondary">
            <AdminMessage id={"archived"} />
          </Badge>
        ) : (
          <StatusBadge status="active" />
        )}
      </TableCell>
      <TableCell className="text-right">
        {writesAllowed && (
          <div className="flex justify-end gap-2">
            {writesAllowed && <ProductDialog product={product} />}
            <Button
              variant="outline"
              size="sm"
              disabled={!writesAllowed || setProductActive.isPending}
              onClick={toggle}
            >
              {product.archived ? "Activate" : "Deactivate"}
            </Button>
          </div>
        )}
      </TableCell>
    </TableRow>
  )
}

function productFormValues(product?: Product) {
  return {
    key: product?.key ?? "",
    name: product?.display_name ?? "",
    description: product?.description ?? "",
    tierGroup: product?.tier_group ?? "",
    tierRank: String(product?.tier_rank ?? 0),
    entitlements: (product?.entitlements ?? []).join(","),
  }
}

function ProductDialog({ product }: { product?: Product }) {
  const { toast, toastApiError, adminMutations } = useAdminServices()

  const { text: adminText, id: adminId } = useAdminUI()

  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const createProduct = useMutation(adminMutations.createProduct(queryClient))
  const updateProduct = useMutation(adminMutations.updateProduct(queryClient))
  const form = useForm({
    defaultValues: productFormValues(product),
    onSubmit: async ({ value }) => {
      const entitlements = value.entitlements
        .split(",")
        .map((item) => item.trim())
        .filter(Boolean)

      try {
        if (product) {
          await updateProduct.mutateAsync({
            id: product.id,
            product: {
              display_name: value.name,
              description: value.description,
              tier_group: value.tierGroup || null,
              tier_rank: Number(value.tierRank) || 0,
              entitlements,
              expected_revision: product.revision,
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
            entitlements,
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
              <AdminMessage id={"Edit"} />
            </Button>
          ) : (
            <Button size="sm">
              <HugeiconsIcon icon={Add01Icon} className="size-4" />
              <AdminMessage id={" Product"} />
            </Button>
          )
        }
      />
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>{product ? "Edit product" : "New product"}</DialogTitle>
          <DialogDescription>
            <AdminMessage
              id={
                "A product is the thing a customer gets. You set what it costs afterwards, by adding a price to it."
              }
            />
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
                  <Field
                    label={adminText("Key")}
                    id={"p-key"}
                    hint="A short name you use in your own code. It cannot be changed later."
                  >
                    <Input
                      id={adminId("p-key")}
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(event) =>
                        field.handleChange(event.target.value)
                      }
                      placeholder={adminText("premium-monthly")}
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
                <Field
                  label={adminText("Display name")}
                  id={"p-name"}
                  hint="What customers see on checkout and invoices."
                >
                  <Input
                    id={adminId("p-name")}
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
                <Field label={adminText("Description")} id={"p-desc"}>
                  <Input
                    id={adminId("p-desc")}
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
                  <Field
                    label={adminText("Tier group")}
                    id={"p-tg"}
                    hint="Products in one group are upgrades of each other. Leave empty if this stands alone."
                  >
                    <Input
                      id={adminId("p-tg")}
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
                  <Field
                    label={adminText("Tier rank")}
                    id={"p-tr"}
                    hint="Higher rank means a higher tier in the group."
                  >
                    <Input
                      id={adminId("p-tr")}
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
                <Field
                  label={adminText("Entitlements")}
                  id={"p-ents"}
                  hint="The access names your product checks. Separate several with commas."
                >
                  <Input
                    id={adminId("p-ents")}
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    placeholder={adminText("premium, downloads")}
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
                <>
                  <Button
                    type="button"
                    variant="outline"
                    disabled={isSubmitting}
                    onClick={() => setOpen(false)}
                  >
                    <AdminMessage id={"Cancel"} />
                  </Button>
                  <Button
                    type="submit"
                    disabled={
                      isSubmitting || !name.trim() || (!product && !key.trim())
                    }
                  >
                    {isSubmitting ? "Saving…" : "Save product"}
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

function PricesTab() {
  const { adminQueries } = useAdminServices()

  const writesAllowed = React.useContext(CatalogWritesContext)
  const pages = useCursorPages("", CATALOG_PAGE_SIZE)
  const { data: products } = useQuery(
    adminQueries.allProducts({ errorAction: "Load products" })
  )
  const {
    data,
    isPending: loading,
    isFetching,
  } = useQuery(
    adminQueries.prices({
      limit: pages.size,
      cursor: pages.cursor,
      errorAction: "Load prices",
    })
  )
  return (
    <div className="flex flex-col gap-3">
      <div className="flex justify-end">
        {writesAllowed && <PriceDialog products={products?.data ?? []} />}
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
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Price"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Product"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Amount"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Renews"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Providers"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"State"} />
                </TableHead>
                <TableHead />
              </TableRow>
            </TableHeader>
            <TableBody>
              {(data?.data ?? []).map((price) => (
                <PriceRow
                  key={price.id}
                  price={price}
                  product={products?.data.find(
                    (p) => p.id === price.product_id
                  )}
                />
              ))}
              {!data?.data?.length && (
                <TableRow>
                  <TableCell
                    colSpan={7}
                    className="h-24 text-center text-muted-foreground"
                  >
                    <AdminMessage id={"No prices yet."} />
                  </TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
        </div>
      )}
      <CursorPager
        pages={pages}
        rows={data?.data?.length ?? 0}
        nextCursor={data?.next_cursor}
        busy={isFetching}
        sizes={[50, 100, 200]}
      />
    </div>
  )
}

function PriceRow({ price, product }: { price: Price; product?: Product }) {
  const { toast, toastApiError, adminMutations } = useAdminServices()

  const productName = product?.display_name ?? shortId(price.product_id, 13)
  const writesAllowed = React.useContext(CatalogWritesContext)
  const queryClient = useQueryClient()
  const setPriceActive = useMutation(adminMutations.setPriceActive(queryClient))
  const toggle = async () => {
    try {
      await setPriceActive.mutateAsync({
        id: price.id,
        active: price.archived,
        revision: price.revision,
      })
      toast.success(price.archived ? "Price activated" : "Price deactivated")
    } catch (err) {
      toastApiError(err, "Toggle price")
    }
  }
  return (
    <LinkedTableRow
      to={`/catalog/prices/${encodeURIComponent(price.id)}`}
      className={price.archived ? "opacity-60" : undefined}
    >
      <TableCell className="text-xs text-muted-foreground">
        <Link
          className="underline-offset-2 hover:underline"
          to={`/catalog/prices/${encodeURIComponent(price.id)}`}
        >
          {price.id}
        </Link>
      </TableCell>
      <TableCell className="font-medium">{productName}</TableCell>
      <TableCell className="tabular-nums">{priceAmountLabel(price)}</TableCell>
      <TableCell>{priceIntervalLabel(price)}</TableCell>
      <TableCell>
        <span className="flex flex-wrap gap-1">
          {/* psp_links is keyed by PSP key ("mobius"), not by rail — the rail
              is recorded inside the entry (or#812). */}
          {Object.entries(price.psps ?? {}).map(([psp, state]) => (
            <Badge
              key={psp}
              variant="secondary"
              className={
                state.status === "linked" ? "" : "bg-held-surface text-held"
              }
              title={state.message || undefined}
            >
              {psp}: {state.status}
            </Badge>
          ))}
        </span>
      </TableCell>
      <TableCell>
        {price.archived ? (
          <Badge variant="secondary">
            <AdminMessage id={"archived"} />
          </Badge>
        ) : (
          <StatusBadge status="active" />
        )}
      </TableCell>
      <TableCell className="text-right">
        {writesAllowed && (
          <div className="flex justify-end gap-2">
            {writesAllowed && (
              <PriceChangeWizard
                price={price}
                productName={productName}
                productKey={product?.key}
              />
            )}
            <Button
              variant="outline"
              size="sm"
              disabled={!writesAllowed || setPriceActive.isPending}
              onClick={toggle}
            >
              {price.archived ? "Activate" : "Deactivate"}
            </Button>
          </div>
        )}
      </TableCell>
    </LinkedTableRow>
  )
}

const durationError = ({ value }: { value: string }) =>
  !value || parseHours(value)
    ? undefined
    : "Use hours, days or weeks, such as 30 days"

function PriceDialog({ products }: { products: Product[] }) {
  const { toast, toastApiError, adminMutations } = useAdminServices()

  const { text: adminText, id: adminId } = useAdminUI()

  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const createPrice = useMutation(adminMutations.createPrice(queryClient))
  const form = useForm({
    defaultValues: {
      productId: "",
      amount: "",
      currency: "USD",
      accessDuration: "",
      billingInterval: "",
    },
    onSubmit: async ({ value }) => {
      const unitAmount = nativeAmountFromInput(value.amount, value.currency)
      if (unitAmount === null || BigInt(unitAmount) <= 0n) return
      const accessHours = parseHours(value.accessDuration)
      const billingHours = parseHours(value.billingInterval)
      if (
        (value.accessDuration && !accessHours) ||
        (value.billingInterval && !billingHours)
      )
        return
      try {
        await createPrice.mutateAsync({
          product_id: value.productId,
          unit_amount: unitAmount,
          currency: value.currency,
          ...(accessHours ? { access_duration_hours: accessHours } : {}),
          ...(billingHours ? { billing_interval_hours: billingHours } : {}),
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
            <HugeiconsIcon icon={Add01Icon} className="size-4" />
            <AdminMessage id={" Price"} />
          </Button>
        }
      />
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            <AdminMessage id={"New price"} />
          </DialogTitle>
          <DialogDescription>
            <AdminMessage
              id={
                "These terms cannot be changed later. To charge a different amount, create another price and deactivate this one."
              }
            />
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
                <Field label={adminText("Product")} id={"pr-prod"}>
                  <Select
                    items={products.map((product) => ({
                      value: product.id,
                      label: `${product.display_name} (${product.key})`,
                    }))}
                    value={field.state.value || null}
                    onValueChange={(value) => field.handleChange(value ?? "")}
                  >
                    <SelectTrigger id={adminId("pr-prod")} className="w-full">
                      <SelectValue placeholder={adminText("Pick a product…")} />
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
            {/* Amount and currency are one money value, so they sit on one row
                under one label — two labelled columns pull apart whenever the
                hint wraps. */}
            <Field
              label={adminText("Amount")}
              id={"pr-amount"}
              hint="Enter it the way you would write it, such as 19.90."
            >
              <div className="flex gap-2">
                <form.Field
                  name="amount"
                  validators={{
                    onBlurListenTo: ["currency"],
                    onBlur: ({ value, fieldApi }) =>
                      BigInt(
                        nativeAmountFromInput(
                          value,
                          fieldApi.form.getFieldValue("currency")
                        ) ?? "0"
                      ) > 0n
                        ? undefined
                        : "Enter an amount greater than zero",
                  }}
                >
                  {(field) => (
                    <div className="flex-1">
                      <Input
                        id={adminId("pr-amount")}
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
                    </div>
                  )}
                </form.Field>
                <form.Field
                  name="currency"
                  validators={{
                    onChange: ({ value }) =>
                      currencyScale(value) === undefined
                        ? `Use ${supportedCurrencies.join(", ")}`
                        : undefined,
                  }}
                >
                  {(field) => (
                    <Input
                      id={adminId("pr-cur")}
                      aria-label={adminText("Currency")}
                      className="w-24"
                      // Registered currency codes are normalized to uppercase.
                      value={field.state.value}
                      maxLength={Math.max(
                        ...supportedCurrencies.map((code) => code.length)
                      )}
                      autoCapitalize="characters"
                      spellCheck={false}
                      placeholder={adminText("USD")}
                      onBlur={field.handleBlur}
                      onChange={(event) =>
                        field.handleChange(event.target.value.toUpperCase())
                      }
                    />
                  )}
                </form.Field>
              </div>
            </Field>
            <form.Field
              name="accessDuration"
              validators={{ onBlur: durationError }}
            >
              {(field) => (
                <Field
                  label={adminText("Access duration")}
                  id={"pr-dur"}
                  hint="Such as 30 days, 1 week or 12 hours. Leave it empty for access that never expires."
                >
                  <Input
                    id={adminId("pr-dur")}
                    placeholder={adminText("Never expires")}
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    aria-invalid={field.state.meta.errors.length > 0}
                  />
                  <FormFieldErrors errors={field.state.meta.errors} />
                </Field>
              )}
            </form.Field>
            <form.Field
              name="billingInterval"
              validators={{ onBlur: durationError }}
            >
              {(field) => (
                <div className="grid gap-3 rounded-md border p-3">
                  <div className="flex items-center justify-between gap-4">
                    <Label htmlFor={adminId("pr-renew")}>
                      <AdminMessage id={"Recurring billing"} />
                    </Label>
                    <Switch
                      id={adminId("pr-renew")}
                      checked={!!field.state.value}
                      onCheckedChange={(checked) =>
                        field.handleChange(checked ? "30 days" : "")
                      }
                    />
                  </div>
                  {field.state.value ? (
                    <Field
                      label={adminText("Billing interval")}
                      id={"pr-billing"}
                      hint="Time between payments, such as 30 days, until the customer cancels."
                    >
                      <Input
                        id={adminId("pr-billing")}
                        required
                        value={field.state.value}
                        onBlur={field.handleBlur}
                        onChange={(event) =>
                          field.handleChange(event.target.value)
                        }
                        aria-invalid={field.state.meta.errors.length > 0}
                      />
                      <FormFieldErrors errors={field.state.meta.errors} />
                    </Field>
                  ) : (
                    <p className="text-xs text-muted-foreground">
                      <AdminMessage id={"One-time payment."} />
                    </p>
                  )}
                </div>
              )}
            </form.Field>
          </div>
          <DialogFooter>
            <Button
              type="button"
              variant="outline"
              onClick={() => handleOpenChange(false)}
            >
              <AdminMessage id={"Cancel"} />
            </Button>
            <form.Subscribe
              selector={(state) =>
                [
                  state.values.productId,
                  state.values.amount,
                  state.values.currency,
                  state.values.accessDuration,
                  state.values.billingInterval,
                  state.isSubmitting,
                ] as const
              }
            >
              {([
                productId,
                amount,
                currency,
                accessDuration,
                billingInterval,
                isSubmitting,
              ]) => (
                <Button
                  type="submit"
                  disabled={
                    isSubmitting ||
                    !productId ||
                    BigInt(nativeAmountFromInput(amount, currency) ?? "0") <=
                      0n ||
                    durationError({ value: accessDuration }) !== undefined ||
                    durationError({ value: billingInterval }) !== undefined
                  }
                >
                  {isSubmitting ? "Creating…" : "Create price"}
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
  const { toast, toastApiError, adminMutations, adminQueries } =
    useAdminServices()

  const queryClient = useQueryClient()
  const canRead = useMerchantConfig()
  const canRefresh = useMerchantConfigUpdates()
  const { data, isPending: loading } = useQuery(adminQueries.catalogDrift(canRead))
  const refreshDrift = useMutation(adminMutations.refreshPSPs(queryClient))

  return (
    <div className="flex flex-col gap-3">
      <div className="flex justify-end">
        {canRefresh && (
          <Button
            variant="outline"
            size="sm"
            disabled={refreshDrift.isPending}
            onClick={async () => {
              try {
                await refreshDrift.mutateAsync()
                toast.success(
                  "Re-reading every PSP: new drift appears here as it is found"
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
            <AdminMessage id={"Refresh scan"} />
          </Button>
        )}
      </div>
      {loading ? (
        <div className="flex flex-col gap-3 py-2">
          {Array.from({ length: 4 }).map((_, i) => (
            <Skeleton key={i} className="h-6 w-full" />
          ))}
        </div>
      ) : !data?.data?.length ? (
        <p className="text-sm text-muted-foreground">
          <AdminMessage id={"No open drift events. Catalog is in sync."} />
        </p>
      ) : (
        <div className="overflow-x-auto">
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Provider"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Kind"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Resource"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Field"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"OpenRails"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Remote"} />
                </TableHead>
                <TableHead className="text-muted-foreground">
                  <AdminMessage id={"Detected"} />
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {data.data.map((e) => (
                <TableRow key={e.id}>
                  <TableCell>{e.provider ?? "—"}</TableCell>
                  <TableCell>
                    {e.finding_type.replace(/^catalog\./, "")}
                  </TableCell>
                  <TableCell className="text-xs">
                    {e.resource_type}{" "}
                    {shortId(e.resource_id || e.external_resource_id || "", 13)}
                  </TableCell>
                  <TableCell>{e.field || "—"}</TableCell>
                  <TableCell className="max-w-40 truncate">
                    {e.openrails_value || "—"}
                  </TableCell>
                  <TableCell className="max-w-40 truncate">
                    {e.external_value || "—"}
                  </TableCell>
                  <TableCell>{formatDate(e.created_at)}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        </div>
      )}
    </div>
  )
}

// hint carries the invisible constraint: what a value is for, or what happens if
// it is left empty. Anything the control already says is left out.
function Field({
  label,
  id,
  hint,
  children,
}: {
  label: string
  id: string
  hint?: string
  children: React.ReactNode
}) {
  const { id: adminId } = useAdminUI()

  return (
    <div className="grid gap-1.5">
      <Label htmlFor={adminId(id)}>{label}</Label>
      {hint ? (
        <p className="text-[13px] text-muted-foreground">{hint}</p>
      ) : null}
      {children}
    </div>
  )
}
