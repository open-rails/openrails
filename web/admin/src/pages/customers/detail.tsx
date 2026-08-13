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

import { FormFieldErrors } from "@/components/form-field-errors"
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
  formatDate,
  formatMicros,
  microsFromInput,
  shortId,
} from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { DIALOG_FORM } from "@/lib/dialog-width"
import { adminQueries } from "@/lib/queries"
import { toastApiError } from "@/lib/toast"

export function CustomerDetailPage() {
  const { customerId = "" } = useParams()
  const navigate = useNavigate()
  const { data: profile, isPending: loading } = useQuery(
    adminQueries.customer(customerId)
  )

  if (loading) return <p className="text-sm text-muted-foreground">Loading…</p>
  if (!profile)
    return <p className="text-sm text-muted-foreground">Customer not found.</p>

  return (
    <div className="flex flex-col gap-4">
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
            {profile.email ?? "Customer"}
          </h2>
          <p className="truncate text-xs text-muted-foreground">
            {profile.customer_id}
          </p>
        </div>
        {profile.trust_level && profile.trust_level !== "default" ? (
          <Badge variant="secondary">{profile.trust_level}</Badge>
        ) : null}
        <div className="ml-auto flex gap-2">
          <GrantEntitlementDialog customerId={customerId} />
          <GrantProductAccessDialog customerId={customerId} />
          <OffChannelPaymentDialog customerId={customerId} />
        </div>
      </div>

      {profile.credit_balance.length > 0 && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {profile.credit_balance.map((b) => (
            <Card key={b.currency}>
              <CardHeader className="pb-1">
                <CardTitle className="text-xs font-normal text-muted-foreground uppercase">
                  {b.display_name} balance
                </CardTitle>
              </CardHeader>
              <CardContent>
                <p className="text-lg font-semibold">
                  {formatMicros(b.balance, b.currency)}
                </p>
                <p className="text-xs text-muted-foreground">
                  held {formatMicros(b.held_balance, b.currency)} · owed{" "}
                  {formatMicros(b.outstanding_owed_amount, b.currency)}
                </p>
              </CardContent>
            </Card>
          ))}
        </div>
      )}

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
                    <TableRow>
                      <TableHead>Subscription</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead>Rail</TableHead>
                      <TableHead>Period ends</TableHead>
                      <TableHead>Email</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {profile.subscriptions.map((s) => (
                      <TableRow key={s.id}>
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
                        <TableCell>
                          {formatDate(s.current_period_ends_at)}
                        </TableCell>
                        <TableCell>{s.user_email ?? "—"}</TableCell>
                      </TableRow>
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
              {profile.payments.length === 0 ? (
                <p className="text-sm text-muted-foreground">No payments.</p>
              ) : (
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead>Payment</TableHead>
                      <TableHead>Status</TableHead>
                      <TableHead>Amount</TableHead>
                      <TableHead>Rail</TableHead>
                      <TableHead>Purchased</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {profile.payments.map((p) => (
                      <TableRow key={p.id}>
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
                        <TableCell>
                          {formatMicros(p.amount, p.currency)}
                        </TableCell>
                        <TableCell>{p.rail}</TableCell>
                        <TableCell>{formatDate(p.purchased_at)}</TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="text-sm">Payment methods</CardTitle>
            </CardHeader>
            <CardContent>
              {profile.payment_methods.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  No payment methods on file.
                </p>
              ) : (
                <div className="flex flex-wrap gap-3">
                  {profile.payment_methods.map((pm) => (
                    <div key={pm.id} className="rounded-md border p-3 text-sm">
                      <p className="font-medium">
                        {pm.card?.brand ?? pm.type} ••••{" "}
                        {pm.card?.last4 ?? "????"}
                      </p>
                      <p className="text-xs text-muted-foreground">
                        {pm.rail} · exp {pm.card?.exp_month ?? "??"}/
                        {pm.card?.exp_year ?? "????"}
                      </p>
                      {pm.health?.expiry_status &&
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
              {profile.entitlements.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  No active entitlements.
                </p>
              ) : (
                <div className="grid gap-4">
                  {profile.entitlements.map((e) => (
                    <div key={e.id} className="flex min-w-0 items-start gap-3">
                      <div className="min-w-0 flex-1">
                        <p className="truncate text-sm font-medium">
                          {e.entitlement}
                        </p>
                        <p className="mt-1 text-xs text-muted-foreground">
                          {e.source_type}
                        </p>
                        <p className="text-xs text-muted-foreground tabular-nums">
                          {formatDate(e.start_at)} to{" "}
                          {e.end_at ? formatDate(e.end_at) : "no end date"}
                        </p>
                      </div>
                      <RevokeEntitlementButton
                        customerId={customerId}
                        entitlementId={e.id}
                        label={`Revoke ${e.entitlement}`}
                      />
                    </div>
                  ))}
                </div>
              )}
            </CardContent>
          </Card>

          <Card>
            <CardHeader>
              <CardTitle className="text-sm">Product access</CardTitle>
            </CardHeader>
            <CardContent>
              {profile.product_access.length === 0 ? (
                <p className="text-sm text-muted-foreground">
                  No product access grants.
                </p>
              ) : (
                <div className="grid gap-4">
                  {profile.product_access.map((g) => (
                    <div key={g.id} className="flex min-w-0 items-start gap-3">
                      <div className="min-w-0 flex-1">
                        <div className="flex min-w-0 items-center gap-2">
                          <p className="truncate text-sm font-medium">
                            {shortId(g.product_id, 13)}
                          </p>
                          <StatusBadge status={g.status} />
                        </div>
                        <p className="mt-1 text-xs text-muted-foreground">
                          {g.source_type}
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

function RevokeEntitlementButton({
  customerId,
  entitlementId,
  label,
}: {
  customerId: string
  entitlementId: string
  label: string
}) {
  const queryClient = useQueryClient()
  const revoke = useMutation(
    adminMutations.revokeCustomerEntitlement(queryClient, customerId)
  )

  return (
    <RevokeButton
      label={label}
      busy={revoke.isPending}
      onRevoke={() => revoke.mutateAsync(entitlementId)}
      successMessage="Entitlement revoked"
    />
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

function GrantEntitlementDialog({ customerId }: { customerId: string }) {
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const grant = useMutation(
    adminMutations.grantCustomerEntitlement(queryClient, customerId)
  )
  const form = useForm({
    defaultValues: { entitlement: "", hours: "" },
    onSubmit: async ({ value }) => {
      try {
        await grant.mutateAsync({
          entitlement: value.entitlement.trim(),
          hours: value.hours ? Number(value.hours) : undefined,
        })
        toast.success("Entitlement granted")
        handleOpenChange(false)
      } catch (err) {
        toastApiError(err, "Grant entitlement")
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
            <HugeiconsIcon icon={Add01Icon} className="size-4" /> Entitlement
          </Button>
        }
      />
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>Grant entitlement</DialogTitle>
          <DialogDescription>
            Give this customer access without a payment. Use it for goodwill,
            support fixes, and trials.
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
              name="entitlement"
              validators={{
                onChange: ({ value }) =>
                  value.trim() ? undefined : "Enter an entitlement",
              }}
            >
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="ent-name">Entitlement</Label>
                  <p className="text-[13px] text-muted-foreground">
                    The name your product checks before unlocking a feature.
                  </p>
                  <Input
                    id="ent-name"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    placeholder="premium"
                    aria-invalid={field.state.meta.errors.length > 0}
                  />
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
                  return Number.isFinite(hours) && hours >= 1
                    ? undefined
                    : "Enter at least 1 hour"
                },
              }}
            >
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="ent-hours">How long it lasts</Label>
                  <p className="text-[13px] text-muted-foreground">
                    In hours. Leave it empty and the access never expires.
                  </p>
                  <Input
                    id="ent-hours"
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
          </div>
          <DialogFooter>
            <form.Subscribe
              selector={(state) =>
                [
                  state.values.entitlement,
                  state.canSubmit,
                  state.isSubmitting,
                ] as const
              }
            >
              {([entitlement, canSubmit, isSubmitting]) => (
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
                    disabled={!entitlement.trim() || !canSubmit || isSubmitting}
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

function GrantProductAccessDialog({ customerId }: { customerId: string }) {
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const grant = useMutation(
    adminMutations.grantCustomerProductAccess(queryClient, customerId)
  )
  const { data: products } = useQuery({
    ...adminQueries.products(),
    enabled: open,
  })
  const form = useForm({
    defaultValues: { productId: "", endsAt: "" },
    onSubmit: async ({ value }) => {
      try {
        await grant.mutateAsync({
          productId: value.productId,
          endsAt: value.endsAt
            ? new Date(value.endsAt).toISOString()
            : undefined,
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
            Give this customer one of your products without a payment. Nothing
            is charged and no subscription is started.
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
                      {(products?.items ?? []).map((p) => (
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
              name="endsAt"
              validators={{
                onChange: ({ value }) =>
                  value && Number.isNaN(new Date(value).getTime())
                    ? "Enter a valid end date"
                    : undefined,
              }}
            >
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="pa-ends">When access ends</Label>
                  <p className="text-[13px] text-muted-foreground">
                    Leave it empty and the access never expires.
                  </p>
                  <Input
                    id="pa-ends"
                    type="datetime-local"
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
    ...adminQueries.prices(),
    enabled: open,
  })
  const form = useForm({
    defaultValues: { priceId: "", transactionId: "", amount: "" },
    onSubmit: async ({ value }) => {
      try {
        const micros = value.amount ? microsFromInput(value.amount) : null
        const result = await recordPayment.mutateAsync({
          price_id: value.priceId,
          transaction_id: value.transactionId.trim(),
          ...(micros !== null && value.amount ? { amount: micros } : {}),
        })
        toast.success(
          result.status === "exists"
            ? "Payment already recorded"
            : "Payment recorded"
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
                      {(prices?.items ?? []).map((p) => (
                        <SelectItem key={p.id} value={p.id}>
                          {formatMicros(p.unit_amount, p.currency)}
                          {p.auto_renew ? " · recurring" : ""} ({shortId(p.id)})
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
                onChange: ({ value }) => {
                  if (!value) return undefined
                  const amount = microsFromInput(value)
                  return amount !== null && amount >= 0
                    ? undefined
                    : "Enter a non-negative amount"
                },
              }}
            >
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="oc-amount">Amount received</Label>
                  <p className="text-[13px] text-muted-foreground">
                    Only if it differs from the price above, for example a part
                    payment. Enter it the way you would write it, such as 19.90.
                  </p>
                  <Input
                    id="oc-amount"
                    type="number"
                    step="any"
                    min="0"
                    placeholder="Same as the price"
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
