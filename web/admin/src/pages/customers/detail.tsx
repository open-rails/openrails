import { HugeiconsIcon } from "@hugeicons/react"
import {
  Add01Icon,
  ArrowLeft01Icon,
  Delete02Icon,
} from "@hugeicons/core-free-icons"
import * as React from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { toast } from "sonner"
import { useForm } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { priceAmountLabel } from "@/pages/catalog/price-format"
import { FormFieldErrors } from "@/components/form-field-errors"
import { LinkedTableRow } from "@/components/linked-table-row"
import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
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
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import {
  formatCard,
  formatCardExpiry,
  formatDate,
  formatNativeAmount,
  nativeAmountFromInput,
  shortId,
} from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { DIALOG_FORM } from "@/lib/dialog-width"
import { adminQueries, queryKeys } from "@/lib/queries"
import { toastApiError } from "@/lib/toast"
import { CustomerInvoiceProfileSection } from "./invoice-profile"
import { CollectionDefaultBadges } from "./collection-default-badges"
import { CustomerUsageRatesSection } from "./usage-rates"
import { CustomerCreditSupportSection } from "./credits"

export function CustomerDetailPage() {
  const { customerId = "" } = useParams()
  const navigate = useNavigate()
  const queryClient = useQueryClient()
  const { data: profile, isPending: loading } = useQuery(
    adminQueries.customer(customerId)
  )

  if (loading) return <p className="text-sm text-muted-foreground">Loading…</p>
  if (!profile)
    return <p className="text-sm text-muted-foreground">Customer not found.</p>
  const payments = profile.payments ?? []
  const methods = profile.payment_methods ?? []

  return (
    <div className="flex min-w-0 flex-col gap-4">
      <div className="flex items-center gap-3">
        <Button
          variant="ghost"
          size="icon"
          onClick={() => navigate(-1)}
          aria-label="Back"
        >
          <HugeiconsIcon icon={ArrowLeft01Icon} className="size-4" />
        </Button>
        <div className="min-w-0">
          <h2 className="truncate text-base font-semibold">
            {profile.customer.email ?? "Customer"}
          </h2>
          <p className="truncate text-xs text-muted-foreground">
            {profile.customer.id}
          </p>
        </div>
        <div className="ml-auto flex gap-2">
          <GrantProductAccessDialog customerId={customerId} />
          <OffChannelPaymentDialog customerId={customerId} />
        </div>
      </div>

      {profile.balances.length > 0 && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {profile.balances.map((b) => (
            <Card key={b.currency}>
              <CardHeader className="pb-1">
                <CardTitle className="text-xs font-normal text-muted-foreground uppercase">
                  {b.currency} balance
                </CardTitle>
              </CardHeader>
              <CardContent>
                <p className="text-lg font-semibold">
                  {formatNativeAmount(b.balance_amount, b.currency)}
                </p>
                <p className="text-xs text-muted-foreground">
                  held {formatNativeAmount(b.held_amount, b.currency)} · owed{" "}
                  {formatNativeAmount(b.owed_amount, b.currency)}
                </p>
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      <CustomerCreditSupportSection
        customerId={customerId}
        currencies={profile.balances.map((balance) => balance.currency)}
      />
      <CustomerUsageRatesSection customerId={customerId} />
      <CustomerInvoiceProfileSection customerId={customerId} />

      <div className="grid gap-4 lg:grid-cols-3 lg:items-start">
        <div className="grid gap-4 lg:col-span-2">
          <Card>
            <CardHeader>
              <CardTitle className="text-sm">Subscriptions</CardTitle>
            </CardHeader>
            <CardContent>
              {profile.subscriptions.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  No subscriptions.
                </p>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow className="hover:bg-transparent">
                      <TableHead className="text-muted-foreground">
                        Subscription
                      </TableHead>
                      <TableHead className="text-muted-foreground">
                        Status
                      </TableHead>
                      <TableHead className="text-muted-foreground">
                        Rail
                      </TableHead>
                      <TableHead className="text-muted-foreground">
                        Period ends
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {profile.subscriptions.map((s) => (
                      <LinkedTableRow key={s.id} to={`/subscriptions/${s.id}`}>
                        <TableCell>
                          <Link
                            className="text-xs underline-offset-2 hover:underline"
                            to={`/subscriptions/${s.id}`}
                          >
                            {shortId(s.id, 13)}
                          </Link>
                        </TableCell>
                        <TableCell>
                          <StatusBadge status={s.status} />
                        </TableCell>
                        <TableCell>{s.rail}</TableCell>
                        <TableCell className="tabular-nums">
                          {formatDate(s.current_period_ends_at)}
                        </TableCell>
                      </LinkedTableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="text-sm">Payments</CardTitle>
            </CardHeader>
            <CardContent>
              {payments.length === 0 ? (
                <p className="text-sm text-muted-foreground">No payments.</p>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow className="hover:bg-transparent">
                      <TableHead className="text-muted-foreground">
                        Payment
                      </TableHead>
                      <TableHead className="text-muted-foreground">
                        Status
                      </TableHead>
                      <TableHead className="text-muted-foreground">
                        Amount
                      </TableHead>
                      <TableHead className="text-muted-foreground">
                        Kind
                      </TableHead>
                      <TableHead className="text-muted-foreground">
                        Rail
                      </TableHead>
                      <TableHead className="text-muted-foreground">
                        Purchased
                      </TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {payments.map((p) => (
                      <LinkedTableRow key={p.id} to={`/payments/${p.id}`}>
                        <TableCell>
                          <Link
                            className="text-xs underline-offset-2 hover:underline"
                            to={`/payments/${p.id}`}
                          >
                            {shortId(p.id, 13)}
                          </Link>
                        </TableCell>
                        <TableCell>
                          <StatusBadge status={p.status} />
                        </TableCell>
                        <TableCell className="tabular-nums">
                          {formatNativeAmount(p.amount, p.currency)}
                        </TableCell>
                        <TableCell>{p.kind}</TableCell>
                        <TableCell>{p.rail ?? p.channel}</TableCell>
                        <TableCell className="tabular-nums">
                          {formatDate(p.created_at)}
                        </TableCell>
                      </LinkedTableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="text-sm">Payment methods</CardTitle>
              <Button
                variant="ghost"
                size="sm"
                onClick={() =>
                  void queryClient.invalidateQueries({
                    queryKey: queryKeys.customer(customerId),
                  })
                }
              >
                Refresh payment methods
              </Button>
            </CardHeader>
            <CardContent>
              {methods.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  No payment methods on file.
                </p>
              ) : (
                <div className="flex flex-wrap gap-3">
                  {methods.map((pm) => (
                    <div key={pm.id} className="rounded-md border p-3 text-sm">
                      <p className="font-medium">{formatCard(pm.card)}</p>
                      <p className="text-xs text-muted-foreground">
                        {pm.rail} · exp {formatCardExpiry(pm.card)}
                      </p>
                      <CollectionDefaultBadges
                        currencies={pm.collection_currencies ?? []}
                      />
                      {pm.health.expiry_status &&
                        pm.health.expiry_status !== "valid" && (
                          <Badge
                            variant="secondary"
                            className="mt-1 bg-held-surface text-held"
                          >
                            {pm.health.expiry_status}
                          </Badge>
                        )}
                    </div>
                  ))}
                </div>
              )}
            </CardContent>
          </Card>
        </div>
        <div className="grid gap-4">
          <Card>
            <CardHeader>
              <CardTitle className="text-sm">Entitlements</CardTitle>
            </CardHeader>
            <CardContent>
              <p className="mb-3 text-xs text-muted-foreground">
                The keys of the products this customer holds now.
              </p>
              {profile.entitlements.data.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  No entitlements.
                </p>
              ) : (
                <div className="flex flex-wrap gap-2">
                  {profile.entitlements.data.map((e) => (
                    <Badge key={e.entitlement} variant="secondary">
                      {e.entitlement}
                    </Badge>
                  ))}
                  {profile.entitlements.next_cursor && (
                    <span className="text-xs text-muted-foreground">
                      and more
                    </span>
                  )}
                </div>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="text-sm">Product access</CardTitle>
            </CardHeader>
            <CardContent>
              {profile.product_access.data.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  No product access.
                </p>
              ) : (
                <div className="grid gap-4">
                  {profile.product_access.data.map((g) => (
                    <div key={g.id} className="flex min-w-0 items-start gap-3">
                      <div className="min-w-0 flex-1">
                        <div className="flex min-w-0 items-center gap-2">
                          <p className="truncate text-sm font-medium">
                            {g.product_name || shortId(g.product_id, 13)}
                          </p>
                          <StatusBadge status={g.status} />
                        </div>
                        <p className="mt-1 text-xs text-muted-foreground">
                          {g.source_type === "grant"
                            ? `Granted (${g.grant_reason ?? "staff"})${g.note ? `: ${g.note}` : ""}`
                            : g.source_type}
                        </p>
                        <p className="text-xs text-muted-foreground tabular-nums">
                          {g.ends_at
                            ? `Ends ${formatDate(g.ends_at)}`
                            : "No end date"}
                        </p>
                      </div>
                      <RevokeProductAccessButton
                        customerId={customerId}
                        grantId={g.id}
                        label="Revoke product access"
                      />
                    </div>
                  ))}
                </div>
              )}
            </CardContent>
          </Card>
        </div>
      </div>
    </div>
  )
}

function RevokeButton({
  label,
  busy,
  onRevoke,
  successMessage,
}: {
  label: string
  busy: boolean
  onRevoke: () => Promise<unknown>
  successMessage: string
}) {
  return (
    <Button
      variant="ghost"
      size="icon"
      aria-label={label}
      disabled={busy}
      onClick={async () => {
        try {
          await onRevoke()
          toast.success(successMessage)
        } catch (err) {
          toastApiError(err, label)
        }
      }}
    >
      <HugeiconsIcon icon={Delete02Icon} className="size-4 text-destructive" />
    </Button>
  )
}

function RevokeProductAccessButton({
  customerId,
  grantId,
  label,
}: {
  customerId: string
  grantId: string
  label: string
}) {
  const queryClient = useQueryClient()
  const revoke = useMutation(
    adminMutations.revokeCustomerProductAccess(queryClient, customerId)
  )

  return (
    <RevokeButton
      label={label}
      busy={revoke.isPending}
      onRevoke={() => revoke.mutateAsync(grantId)}
      successMessage="Product access revoked"
    />
  )
}

function GrantProductAccessDialog({ customerId }: { customerId: string }) {
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const grant = useMutation(
    adminMutations.grantCustomerProductAccess(queryClient, customerId)
  )
  const { data: products } = useQuery({
    ...adminQueries.allProducts(),
    enabled: open,
  })
  const form = useForm({
    defaultValues: { productId: "", hours: "", note: "" },
    onSubmit: async ({ value }) => {
      try {
        await grant.mutateAsync({
          productId: value.productId,
          hours: value.hours ? Number(value.hours) : undefined,
          note: value.note.trim() || undefined,
        })
        toast.success("Product access granted")
        handleOpenChange(false)
      } catch (err) {
        toastApiError(err, "Grant product access")
      }
    },
  })

  const handleOpenChange = (next: boolean) => {
    setOpen(next)
    if (!next) {
      form.reset()
      grant.reset()
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger
        render={
          <Button variant="outline" size="sm">
            <HugeiconsIcon icon={Add01Icon} className="size-4" /> Product access
          </Button>
        }
      />
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>Grant product access</DialogTitle>
          <DialogDescription>
            Give this customer one of your products without a payment. They
            hold its keys while the grant lasts, including keys you add to the
            product later.
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
            <form.Field
              name="productId"
              validators={{
                onChange: ({ value }) => (value ? undefined : "Pick a product"),
              }}
            >
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="pa-product">Product</Label>
                  <Select
                    value={field.state.value}
                    onValueChange={(value) => field.handleChange(value ?? "")}
                  >
                    <SelectTrigger
                      className="w-full"
                      id="pa-product"
                      aria-invalid={field.state.meta.errors.length > 0}
                    >
                      <SelectValue placeholder="Pick a product" />
                    </SelectTrigger>
                    <SelectContent>
                      {(products?.data ?? []).map((p) => (
                        <SelectItem key={p.id} value={p.id}>
                          {p.display_name} ({p.key})
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <FormFieldErrors errors={field.state.meta.errors} />
                </div>
              )}
            </form.Field>
            <form.Field
              name="hours"
              validators={{
                onChange: ({ value }) => {
                  if (!value) return undefined
                  const hours = Number(value)
                  return Number.isInteger(hours) && hours >= 1
                    ? undefined
                    : "Enter at least 1 hour"
                },
              }}
            >
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="pa-hours">How long it lasts</Label>
                  <p className="text-[13px] text-muted-foreground">
                    In hours, after any access they already have. Leave it
                    empty and the access never expires.
                  </p>
                  <Input
                    id="pa-hours"
                    type="number"
                    min="1"
                    placeholder="Never expires"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    aria-invalid={field.state.meta.errors.length > 0}
                  />
                  <FormFieldErrors errors={field.state.meta.errors} />
                </div>
              )}
            </form.Field>
            <form.Field name="note">
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="pa-note">Note</Label>
                  <Input
                    id="pa-note"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    placeholder="Why it was granted"
                  />
                </div>
              )}
            </form.Field>
          </div>
          <DialogFooter>
            <form.Subscribe
              selector={(state) =>
                [
                  state.values.productId,
                  state.canSubmit,
                  state.isSubmitting,
                ] as const
              }
            >
              {([productId, canSubmit, isSubmitting]) => (
                <>
                  <Button
                    type="button"
                    variant="outline"
                    disabled={isSubmitting}
                    onClick={() => handleOpenChange(false)}
                  >
                    Cancel
                  </Button>
                  <Button
                    type="submit"
                    disabled={!productId || !canSubmit || isSubmitting}
                  >
                    {isSubmitting ? "Granting…" : "Grant access"}
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

function OffChannelPaymentDialog({ customerId }: { customerId: string }) {
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const recordPayment = useMutation(
    adminMutations.recordCustomerOffChannelPayment(queryClient, customerId)
  )
  const { data: prices } = useQuery({
    ...adminQueries.allPrices(),
    enabled: open,
  })
  const receivedAmount = (value: string, priceId: string) =>
    nativeAmountFromInput(
      value,
      prices?.data.find((price) => price.id === priceId)?.currency ?? ""
    )
  const amountError = (value: string, priceId: string) => {
    const price = prices?.data.find((price) => price.id === priceId)
    if (!value)
      return price?.customer_amount
        ? "Enter the deposit amount received"
        : undefined
    const amount = receivedAmount(value, priceId)
    if (amount === null || BigInt(amount) < 0n)
      return "Enter a non-negative amount"
    const range = price?.customer_amount
    if (
      range &&
      (BigInt(amount) < BigInt(range.min_amount) ||
        BigInt(amount) > BigInt(range.max_amount))
    ) {
      return "Enter an amount within the deposit limits"
    }
    return undefined
  }
  const form = useForm({
    defaultValues: { priceId: "", transactionId: "", amount: "" },
    onSubmit: async ({ value }) => {
      const invalid = amountError(value.amount, value.priceId)
      if (invalid) {
        toast.error(invalid)
        return
      }
      const amount = value.amount
        ? receivedAmount(value.amount, value.priceId)
        : undefined
      if (amount === null || amount?.startsWith("-")) {
        toast.error("Enter a non-negative amount in the price's currency")
        return
      }
      try {
        const result = await recordPayment.mutateAsync({
          price_id: value.priceId,
          transaction_id: value.transactionId.trim(),
          ...(amount !== undefined ? { amount } : {}),
        })
        toast.success(
          result.recorded ? "Payment recorded" : "Payment already recorded"
        )
        handleOpenChange(false)
      } catch (err) {
        toastApiError(err, "Record off-channel payment")
      }
    },
  })

  const handleOpenChange = (next: boolean) => {
    setOpen(next)
    if (!next) {
      form.reset()
      recordPayment.reset()
    }
  }

  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger
        render={
          <Button variant="outline" size="sm">
            <HugeiconsIcon icon={Add01Icon} className="size-4" /> Off-channel
            payment
          </Button>
        }
      />
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>Record a payment taken elsewhere</DialogTitle>
          <DialogDescription>
            For money you already collected outside OpenRails, such as a bank
            transfer or a card taken over the phone. This records the sale and
            starts the customer's access. It does not charge anyone.
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
            <form.Field
              name="priceId"
              validators={{
                onChange: ({ value }) => (value ? undefined : "Pick a price"),
              }}
            >
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="oc-price">Price</Label>
                  <Select
                    value={field.state.value}
                    onValueChange={(value) => field.handleChange(value ?? "")}
                  >
                    <SelectTrigger
                      className="w-full"
                      id="oc-price"
                      aria-invalid={field.state.meta.errors.length > 0}
                    >
                      <SelectValue placeholder="Pick a price" />
                    </SelectTrigger>
                    <SelectContent>
                      {(prices?.data ?? []).map((p) => (
                        <SelectItem key={p.id} value={p.id}>
                          {priceAmountLabel(p)}
                          {p.billing_interval_hours ? " · recurring" : ""} ({shortId(p.id)})
                        </SelectItem>
                      ))}
                    </SelectContent>
                  </Select>
                  <FormFieldErrors errors={field.state.meta.errors} />
                </div>
              )}
            </form.Field>
            <form.Field
              name="transactionId"
              validators={{
                onChange: ({ value }) =>
                  value.trim() ? undefined : "Enter a transaction id",
              }}
            >
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="oc-txn">Reference</Label>
                  {/* The reference is the idempotency key: naming that plainly
                      is what stops the same payment being recorded twice. */}
                  <p className="text-[13px] text-muted-foreground">
                    Your reference for this payment, such as the bank transfer
                    number. Recording the same reference twice will not charge
                    or credit the customer again.
                  </p>
                  <Input
                    id="oc-txn"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    placeholder="TRF-4471"
                    aria-invalid={field.state.meta.errors.length > 0}
                  />
                  <FormFieldErrors errors={field.state.meta.errors} />
                </div>
              )}
            </form.Field>
            <form.Field
              name="amount"
              validators={{
                onChangeListenTo: ["priceId"],
                onChange: ({ value, fieldApi }) => {
                  return amountError(
                    value,
                    fieldApi.form.getFieldValue("priceId")
                  )
                },
              }}
            >
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="oc-amount">Amount received</Label>
                  <p className="text-[13px] text-muted-foreground">
                    Required for a customer-selected deposit; optional for a
                    fixed price if the received amount differs. Enter it as a
                    decimal, such as 19.90.
                  </p>
                  <Input
                    id="oc-amount"
                    type="number"
                    step="any"
                    min="0"
                    placeholder="Amount received, e.g. 100.00"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    aria-invalid={field.state.meta.errors.length > 0}
                  />
                  <FormFieldErrors errors={field.state.meta.errors} />
                </div>
              )}
            </form.Field>
          </div>
          <DialogFooter>
            <form.Subscribe
              selector={(state) =>
                [
                  state.values.priceId,
                  state.values.transactionId,
                  state.canSubmit,
                  state.isSubmitting,
                ] as const
              }
            >
              {([priceId, transactionId, canSubmit, isSubmitting]) => (
                <>
                  <Button
                    type="button"
                    variant="outline"
                    disabled={isSubmitting}
                    onClick={() => handleOpenChange(false)}
                  >
                    Cancel
                  </Button>
                  <Button
                    type="submit"
                    disabled={
                      !priceId ||
                      !transactionId.trim() ||
                      !canSubmit ||
                      isSubmitting
                    }
                  >
                    {isSubmitting ? "Recording…" : "Record payment"}
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
