import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import { HugeiconsIcon } from "@hugeicons/react"
import { ArrowLeft01Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { Link, useNavigate, useParams } from "#orck/admin/router"

import { useForm } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { Fact } from "#orck/admin/components/fact-card"
import { FormFieldErrors } from "#orck/admin/components/form-field-errors"
import { StatusBadge } from "#orck/admin/components/status-badge"
import { TypedConfirmDialog } from "#orck/admin/components/typed-confirm-dialog"
import { Badge } from "#orck/admin/components/ui/badge"
import { Button } from "#orck/admin/components/ui/button"
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "#orck/admin/components/ui/card"
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
import { Switch } from "#orck/admin/components/ui/switch"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "#orck/admin/components/ui/table"
import type { ScheduledChange } from "#orck/admin/lib/api/types"
import { DIALOG_FORM } from "#orck/admin/lib/dialog-width"
import {
  formatCard,
  formatDate,
  formatNativeAmount,
  shortId,
} from "#orck/admin/lib/format"

import { ChangeSubscriptionDialog } from "#orck/admin/pages/subscriptions/change-subscription-dialog"
import { dunningSummary } from "#orck/admin/pages/subscriptions/dunning"
import { useAdminUpdates } from "#orck/admin/lib/capabilities"

export function SubscriptionDetailPage() {
  const { text: adminText } = useAdminUI()

  const { adminQueries } = useAdminServices()

  const canUpdate = useAdminUpdates()
  const { id = "" } = useParams()
  const navigate = useNavigate()
  const { data: sub, isPending: loading } = useQuery(
    adminQueries.subscription(id)
  )
  const { data: paymentPage } = useQuery(
    adminQueries.payments({ subscription_id: id }, 20)
  )
  const payments = paymentPage?.data ?? []
  if (loading)
    return (
      <p className="text-sm text-muted-foreground">
        <AdminMessage id={"Loading…"} />
      </p>
    )
  if (!sub)
    return (
      <p className="text-sm text-muted-foreground">
        <AdminMessage id={"Subscription not found."} />
      </p>
    )

  const cancellable =
    sub.status === "active" ||
    sub.status === "past_due" ||
    sub.status === "awaiting_method" ||
    sub.status === "unverified"
  const resumable = sub.status === "canceled" || sub.status === "past_due"

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-3">
        <Button
          variant="ghost"
          size="icon"
          onClick={() => navigate(-1)}
          aria-label={adminText("Back")}
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
          {canUpdate && resumable && (
            <ResumeButton id={sub.id} customerId={sub.customer_id} />
          )}
          {canUpdate && (
            <ChangeSubscriptionDialog
              subscriptionId={sub.id}
              customerId={sub.customer_id}
              productId={sub.product_id}
              priceId={sub.price_id}
              quantity={sub.quantity}
              currency={sub.price?.currency}
              collectionPolicy={sub.collection_policy}
              scheduledChange={sub.scheduled_change}
              rail={sub.rail}
              status={sub.status}
            />
          )}
          {canUpdate && (
            <ChangePaymentMethodDialog
              subscriptionId={sub.id}
              customerId={sub.customer_id}
              rail={sub.rail}
              collectionPolicy={sub.collection_policy}
            />
          )}
          {canUpdate && cancellable && (
            <CancelDialog id={sub.id} customerId={sub.customer_id} />
          )}
        </div>
      </div>

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Fact label={adminText("Customer")}>
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
        <Fact label={adminText("Started")}>{formatDate(sub.started_at)}</Fact>
        <Fact label={adminText("Current period")}>
          {formatDate(sub.current_period_starts_at)} →{" "}
          {formatDate(sub.current_period_ends_at)}
        </Fact>
        <Fact label={adminText("Price")}>
          <div className="flex flex-col gap-1">
            <span className="flex items-center gap-2">
              {sub.price?.unit_amount !== undefined && sub.price?.currency ? (
                <Link
                  className="underline-offset-2 hover:underline"
                  to={`/catalog/prices/${sub.price_id}`}
                >
                  {formatNativeAmount(
                    sub.price.unit_amount,
                    sub.price.currency
                  )}
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
                  title={adminText("Pinned to an archived (prior) version")}
                >
                  <AdminMessage id={"prior version"} />
                </Badge>
              )}
            </span>
            {sub.price?.key && (
              <span className="text-xs text-muted-foreground">
                {sub.price.key}
              </span>
            )}
            {sub.scheduled_change && (
              <ScheduledChangeBadge change={sub.scheduled_change} />
            )}
          </div>
        </Fact>
        {sub.quantity !== null && (
          <Fact label={adminText("Seats")}>
            {sub.quantity}
            {sub.scheduled_change?.quantity != null &&
              ` → ${sub.scheduled_change.quantity} at renewal`}
          </Fact>
        )}
        <Fact label={adminText("Dunning")}>
          {sub.dunning ? dunningSummary(sub.dunning) : "—"}
        </Fact>
        <Fact
          label={
            sub.dunning?.waiting_for_new_card ? "Card deadline" : "Final retry"
          }
        >
          {formatDate(sub.dunning?.final_retry_at)}
        </Fact>
        <Fact label={adminText("Last decline")}>
          {sub.dunning?.last_failure_reason ?? "—"}
        </Fact>
        <Fact label={adminText("Canceled")}>
          {sub.canceled_at
            ? `${formatDate(sub.canceled_at)} (${sub.cancel_type ?? "?"})`
            : "—"}
        </Fact>
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">
            <AdminMessage id={"Payments"} />
          </CardTitle>
        </CardHeader>
        <CardContent>
          {!payments.length ? (
            <p className="text-sm text-muted-foreground">
              <AdminMessage id={"No payments recorded."} />
            </p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className="text-muted-foreground">
                    <AdminMessage id={"Payment"} />
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    <AdminMessage id={"Status"} />
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    <AdminMessage id={"Kind"} />
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    <AdminMessage id={"Amount"} />
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    <AdminMessage id={"Created"} />
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {payments.map((p) => (
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
                    <TableCell>{p.kind}</TableCell>
                    <TableCell>
                      {formatNativeAmount(p.amount, p.currency)}
                    </TableCell>
                    <TableCell>{formatDate(p.created_at)}</TableCell>
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
  const { toast, toastApiError, adminMutations } = useAdminServices()

  const { text: adminText, id: adminId } = useAdminUI()

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
        <AdminMessage id={"Cancel subscription"} />
      </Button>
      <TypedConfirmDialog
        open={open}
        onOpenChange={handleOpenChange}
        title={adminText("Cancel subscription")}
        description={adminText(
          "Terminal cancellation at the payment rail. This is a last resort. Dunning parks subscriptions as past_due without losing entitlements."
        )}
        confirmationWord="CANCEL"
        actionLabel="Cancel subscription"
        onConfirm={async () => {
          try {
            await cancel.mutateAsync(form.state.values)
            toast.success("Subscription canceled")
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
                <Label htmlFor={adminId("cancel-reason")}>
                  <AdminMessage id={"Reason"} />
                </Label>
                <Input
                  id={adminId("cancel-reason")}
                  value={field.state.value}
                  onBlur={field.handleBlur}
                  onChange={(event) => field.handleChange(event.target.value)}
                  placeholder={adminText("why is this being canceled?")}
                />
              </div>
            )}
          </form.Field>
          <form.Field name="revokeAccess">
            {(field) => (
              <div className="flex items-center gap-2">
                <Switch
                  id={adminId("cancel-revoke")}
                  checked={field.state.value}
                  onCheckedChange={field.handleChange}
                />
                <Label htmlFor={adminId("cancel-revoke")}>
                  <AdminMessage id={"Also revoke access immediately"} />
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
  const { toast, toastApiError, adminMutations } = useAdminServices()

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

// The choice that makes the subscription follow the customer's default card.
const FOLLOW_DEFAULT = "default"

function ChangePaymentMethodDialog({
  subscriptionId,
  customerId,
  rail,
  collectionPolicy,
}: {
  subscriptionId: string
  customerId?: string
  rail: string
  collectionPolicy?: string
}) {
  const { toast, toastApiError, adminMutations, adminQueries } =
    useAdminServices()

  const { id: adminId, text: adminText } = useAdminUI()

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
        await changePaymentMethod.mutateAsync(
          value.paymentMethodId === FOLLOW_DEFAULT
            ? null
            : value.paymentMethodId
        )
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

  // A card subscription OpenRails collects, or an NMI schedule; a provider
  // that keeps the card (Stripe-owned, CCBill) or a wallet has none to change.
  const supported =
    rail !== "solana" &&
    (collectionPolicy === "engine" || collectionPolicy === "nmi_schedule")
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
            <AdminMessage id={"Change payment method"} />
          </Button>
        }
      />
      <DialogContent className={DIALOG_FORM}>
        <DialogHeader>
          <DialogTitle>
            <AdminMessage id={"Change payment method"} />
          </DialogTitle>
          <DialogDescription>
            <AdminMessage
              id={
                "Points future renewals of this subscription at another of the customer&apos;s stored cards (same rail), or back at their default card for its currency."
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
          <form.Field
            name="paymentMethodId"
            validators={{
              onChange: ({ value }) =>
                value ? undefined : "Pick a stored payment method",
            }}
          >
            {(field) => (
              <div className="grid gap-1.5">
                <Label htmlFor={adminId("subscription-payment-method")}>
                  <AdminMessage id={"Payment method"} />
                </Label>
                <Select
                  value={field.state.value}
                  onValueChange={(value) => field.handleChange(value ?? "")}
                >
                  <SelectTrigger
                    className="w-full"
                    id={adminId("subscription-payment-method")}
                    aria-invalid={field.state.meta.errors.length > 0}
                  >
                    <SelectValue
                      placeholder={adminText("Pick a stored payment method")}
                    />
                  </SelectTrigger>
                  <SelectContent>
                    <SelectItem value={FOLLOW_DEFAULT}>
                      <AdminMessage id={"The customer&apos;s default card"} />
                    </SelectItem>
                    {(pms ?? [])
                      .filter((pm) => pm.rail === rail)
                      .map((pm) => (
                        <SelectItem key={pm.id} value={pm.id}>
                          {formatCard(pm.card)} ({pm.rail})
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

// ScheduledChangeBadge shows the change waiting for the next renewal: a price
// migration's move or a scheduled downgrade. A change back to the current
// price clears it.
function ScheduledChangeBadge({ change }: { change: ScheduledChange }) {
  const { adminQueries } = useAdminServices()

  const { data: toPrice } = useQuery(adminQueries.price(change.price_id))
  return (
    <Badge className="bg-held-surface text-held">
      {change.source === "migration" ? "migrates" : "changes"}
      <AdminMessage id={" to"} />{" "}
      {toPrice
        ? formatNativeAmount(toPrice.unit_amount, toPrice.currency)
        : shortId(change.price_id, 9)}{" "}
      <AdminMessage id={"at the first renewal from "} />
      {formatDate(change.effective_at)}
    </Badge>
  )
}
