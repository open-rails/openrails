import { HugeiconsIcon } from "@hugeicons/react"
import { ArrowLeft01Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { toast } from "sonner"
import { useForm } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { Fact } from "@/components/fact-card"
import { FormFieldErrors } from "@/components/form-field-errors"
import { StatusBadge } from "@/components/status-badge"
import { TypedConfirmDialog } from "@/components/typed-confirm-dialog"
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
import { Switch } from "@/components/ui/switch"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import type { SubscriptionReprice } from "@/lib/api/types"
import { formatDate, formatMicros, shortId } from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { adminQueries } from "@/lib/queries"
import { toastApiError } from "@/lib/toast"

export function SubscriptionDetailPage() {
  const { id = "" } = useParams()
  const navigate = useNavigate()
  const { data: sub, isPending: loading } = useQuery(
    adminQueries.subscription(id)
  )
  // #777: pending-reprice badge — at most one scheduled reprice can exist per
  // subscription at a time.
  const { data: scheduledReprices } = useQuery(
    adminQueries.subscriptionReprices(id)
  )
  const pendingReprice = scheduledReprices?.items?.[0]
  if (loading) return <p className="text-sm text-muted-foreground">Loading…</p>
  if (!sub)
    return (
      <p className="text-sm text-muted-foreground">Subscription not found.</p>
    )

  const cancellable =
    sub.status === "active" ||
    sub.status === "past_due" ||
    sub.status === "unknown"
  const resumable = sub.status === "cancelled" || sub.status === "past_due"

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
        <div>
          <h2 className="flex items-center gap-2 text-sm">
            {sub.id} <StatusBadge status={sub.status} />
          </h2>
          <p className="text-xs text-muted-foreground">
            {sub.rail} · {sub.rail_subscription_id || "no rail id"}
          </p>
        </div>
        <div className="ml-auto flex gap-2">
          {resumable && (
            <ResumeButton id={sub.id} customerId={sub.customer_id} />
          )}
          <ChangePaymentMethodDialog
            subscriptionId={sub.id}
            customerId={sub.customer_id}
            rail={sub.rail}
          />
          {cancellable && (
            <CancelDialog id={sub.id} customerId={sub.customer_id} />
          )}
        </div>
      </div>

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Fact label="Customer">
          {sub.customer_id ? (
            <Link
              className="text-xs underline-offset-2 hover:underline"
              to={`/customers/${sub.customer_id}`}
            >
              {shortId(sub.customer_id, 13)}
            </Link>
          ) : (
            "—"
          )}
        </Fact>
        <Fact label="Email">{sub.user_email ?? "—"}</Fact>
        <Fact label="Started">{formatDate(sub.started_at)}</Fact>
        <Fact label="Current period">
          {formatDate(sub.current_period_starts_at)} →{" "}
          {formatDate(sub.current_period_ends_at)}
        </Fact>
        <Fact label="Price">
          <div className="flex flex-col gap-1">
            <span className="flex items-center gap-2">
              {sub.price?.amount !== undefined && sub.price?.currency ? (
                <Link
                  className="underline-offset-2 hover:underline"
                  to={`/catalog/prices/${sub.price_id}`}
                >
                  {formatMicros(sub.price.amount, sub.price.currency)}
                </Link>
              ) : (
                <Link
                  className="text-xs underline-offset-2 hover:underline"
                  to={`/catalog/prices/${sub.price_id}`}
                >
                  {shortId(sub.price_id ?? "—", 13)}
                </Link>
              )}
              {sub.price?.archived && (
                <Badge
                  variant="secondary"
                  title="Pinned to an archived (prior) version"
                >
                  prior version
                </Badge>
              )}
            </span>
            {sub.price?.key && (
              <span className="text-xs text-muted-foreground">
                {sub.price.key}
              </span>
            )}
            {pendingReprice && (
              <PendingRepriceBadge
                subscriptionId={sub.id}
                reprice={pendingReprice}
              />
            )}
          </div>
        </Fact>
        <Fact label="Grace ends">{formatDate(sub.grace_ends_at)}</Fact>
        <Fact label="Retries">
          {sub.retry_attempts ?? 0} · next {formatDate(sub.next_retry_at)}
        </Fact>
        <Fact label="Cancelled">
          {sub.cancelled_at
            ? `${formatDate(sub.cancelled_at)} (${sub.cancel_type ?? "?"})`
            : "—"}
        </Fact>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Payments</CardTitle>
        </CardHeader>
        <CardContent>
          {!sub.payments?.length ? (
            <p className="text-sm text-muted-foreground">
              No payments recorded.
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Payment</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Amount</TableHead>
                  <TableHead>Purchased</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {sub.payments.map((p) => (
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
                    <TableCell>{formatMicros(p.amount, p.currency)}</TableCell>
                    <TableCell>{formatDate(p.purchased_at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>
    </div>
  )
}

function CancelDialog({ id, customerId }: { id: string; customerId?: string }) {
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const cancel = useMutation(
    adminMutations.cancelSubscription(queryClient, id, customerId)
  )
  const form = useForm({
    defaultValues: { reason: "", revokeAccess: false },
  })

  const handleOpenChange = (next: boolean) => {
    setOpen(next)
    if (!next) {
      form.reset()
      cancel.reset()
    }
  }

  return (
    <>
      <Button variant="destructive" size="sm" onClick={() => setOpen(true)}>
        Cancel subscription
      </Button>
      <TypedConfirmDialog
        open={open}
        onOpenChange={handleOpenChange}
        title="Cancel subscription"
        description="Terminal cancellation at the payment rail. This is a last resort — dunning parks subscriptions as past_due without losing entitlements."
        confirmationWord="CANCEL"
        actionLabel="Cancel subscription"
        onConfirm={async () => {
          try {
            await cancel.mutateAsync(form.state.values)
            toast.success("Subscription cancelled")
          } catch (err) {
            toastApiError(err, "Cancel subscription")
            throw err
          }
        }}
      >
        <div className="grid gap-3">
          <form.Field name="reason">
            {(field) => (
              <div className="grid gap-1.5">
                <Label htmlFor="cancel-reason">Reason</Label>
                <Input
                  id="cancel-reason"
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(event) => field.handleChange(event.target.value)}
                  placeholder="why is this being cancelled?"
                />
              </div>
            )}
          </form.Field>
          <form.Field name="revokeAccess">
            {(field) => (
              <div className="flex items-center gap-2">
                <Switch
                  id="cancel-revoke"
                  checked={field.state.value}
                  onCheckedChange={field.handleChange}
                />
                <Label htmlFor="cancel-revoke">
                  Also revoke access immediately
                </Label>
              </div>
            )}
          </form.Field>
        </div>
      </TypedConfirmDialog>
    </>
  )
}

function ResumeButton({ id, customerId }: { id: string; customerId?: string }) {
  const queryClient = useQueryClient()
  const resume = useMutation(
    adminMutations.resumeSubscription(queryClient, id, customerId)
  )
  return (
    <Button
      variant="outline"
      size="sm"
      disabled={resume.isPending}
      onClick={async () => {
        try {
          await resume.mutateAsync()
          toast.success("Resume queued")
        } catch (err) {
          toastApiError(err, "Resume subscription")
        }
      }}
    >
      {resume.isPending ? "Queuing…" : "Resume"}
    </Button>
  )
}

function ChangePaymentMethodDialog({
  subscriptionId,
  customerId,
  rail,
}: {
  subscriptionId: string
  customerId?: string
  rail: string
}) {
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const changePaymentMethod = useMutation(
    adminMutations.changeSubscriptionPaymentMethod(
      queryClient,
      subscriptionId,
      customerId
    )
  )
  const { data: pms } = useQuery(
    adminQueries.customerPaymentMethods(open ? customerId : undefined)
  )
  const form = useForm({
    defaultValues: { paymentMethodId: "" },
    onSubmit: async ({ value }) => {
      try {
        await changePaymentMethod.mutateAsync(value.paymentMethodId)
        toast.success("Payment method updated")
        handleOpenChange(false)
      } catch (err) {
        toastApiError(err, "Change payment method")
      }
    },
  })

  const handleOpenChange = (next: boolean) => {
    setOpen(next)
    if (!next) {
      form.reset()
      changePaymentMethod.reset()
    }
  }

  // Payment-method swap is an NMI-only operation today (see
  // update_subscription_payment_method.go); other rails 400.
  const supported = rail === "nmi"
  return (
    <Dialog open={open} onOpenChange={handleOpenChange}>
      <DialogTrigger
        render={
          <Button
            variant="outline"
            size="sm"
            disabled={!supported}
            title={
              supported
                ? undefined
                : `Payment-method change is not supported on ${rail}`
            }
          >
            Change payment method
          </Button>
        }
      />
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Change payment method</DialogTitle>
          <DialogDescription>
            Points future renewals of this subscription at another stored
            payment method (same rail).
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
          <form.Field
            name="paymentMethodId"
            validators={{
              onChange: ({ value }) =>
                value ? undefined : "Pick a stored payment method",
            }}
          >
            {(field) => (
              <div className="grid gap-1.5">
                <Label htmlFor="subscription-payment-method">
                  Payment method
                </Label>
                <Select
                  value={field.state.value}
                  onValueChange={(value) => field.handleChange(value ?? "")}
                >
                  <SelectTrigger
                    id="subscription-payment-method"
                    aria-invalid={field.state.meta.errors.length > 0}
                  >
                    <SelectValue placeholder="Pick a stored payment method" />
                  </SelectTrigger>
                  <SelectContent>
                    {(pms?.data ?? [])
                      .filter((pm) => pm.rail === rail)
                      .map((pm) => (
                        <SelectItem key={pm.id} value={pm.id}>
                          {pm.card?.brand ?? pm.type} ••••{" "}
                          {pm.card?.last4 ?? "????"} ({pm.rail})
                        </SelectItem>
                      ))}
                  </SelectContent>
                </Select>
                <FormFieldErrors errors={field.state.meta.errors} />
              </div>
            )}
          </form.Field>
          <DialogFooter>
            <form.Subscribe
              selector={(state) =>
                [
                  state.values.paymentMethodId,
                  state.canSubmit,
                  state.isSubmitting,
                ] as const
              }
            >
              {([paymentMethodId, canSubmit, isSubmitting]) => (
                <Button
                  type="submit"
                  disabled={!paymentMethodId || !canSubmit || isSubmitting}
                >
                  {isSubmitting ? "Updating…" : "Update"}
                </Button>
              )}
            </form.Subscribe>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

// PendingRepriceBadge (#777) surfaces a subscription's scheduled price move
// (from the #773 primitive, usually scheduled in bulk by the price-change
// wizard) with an inline cancel — allowed until the effective date; an
// already-applied reprice cannot reach this badge (it's filtered to
// status=scheduled by the caller).
function PendingRepriceBadge({
  subscriptionId,
  reprice,
}: {
  subscriptionId: string
  reprice: SubscriptionReprice
}) {
  const queryClient = useQueryClient()
  const { data: toPrice } = useQuery(adminQueries.price(reprice.to_price_id))
  const cancel = useMutation(
    adminMutations.cancelSubscriptionReprice(queryClient, subscriptionId)
  )
  return (
    <span className="flex items-center gap-1.5">
      <Badge className="bg-amber-500/15 text-amber-600 dark:text-amber-400">
        moves to{" "}
        {toPrice
          ? formatMicros(toPrice.unit_amount, toPrice.currency)
          : shortId(reprice.to_price_id, 9)}{" "}
        on {formatDate(reprice.effective_at)}
      </Badge>
      <Button
        variant="ghost"
        size="sm"
        className="h-5 px-1.5 text-xs"
        disabled={cancel.isPending}
        onClick={async () => {
          try {
            await cancel.mutateAsync(reprice.id)
            toast.success("Reprice canceled")
          } catch (err) {
            toastApiError(err, "Cancel reprice")
          }
        }}
      >
        cancel
      </Button>
    </span>
  )
}
