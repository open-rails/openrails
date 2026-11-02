import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { FACT_TILES } from "#orck/admin/lib/tile-grids"
import { useAdminServices } from "#orck/admin/context"
import { HugeiconsIcon } from "@hugeicons/react"
import { ArrowLeft01Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { Link, useNavigate, useParams } from "#orck/admin/router"

import { useForm, useStore } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { Fact } from "#orck/admin/components/fact-card"
import { MissingRecord } from "#orck/admin/components/missing-record"
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
  customerName,
  formatCard,
  formatDate,
  formatNativeAmount,
  shortId,
} from "#orck/admin/lib/format"

// formatDay is a date without its time: "Dec 2, 2026".
const formatDay = (iso: string) =>
  new Date(iso).toLocaleDateString(undefined, { dateStyle: "medium" })

import { ChangeSubscriptionDialog } from "#orck/admin/pages/subscriptions/change-subscription-dialog"
import { dunningSummary } from "#orck/admin/pages/subscriptions/dunning"
import { useAdminUpdates } from "#orck/admin/lib/capabilities"

export function SubscriptionDetailPage() {
  const { text: adminText } = useAdminUI()

  const { adminQueries } = useAdminServices()

  const canUpdate = useAdminUpdates()
  const { id = "" } = useParams()
  const navigate = useNavigate()
  const {
    data: sub,
    isPending: loading,
    error,
  } = useQuery(adminQueries.subscription(id))
  const { data: paymentPage } = useQuery(
    adminQueries.payments({ subscription_id: id }, 20)
  )
  const payments = paymentPage?.data ?? []
  const { data: customer } = useQuery(
    adminQueries.customer(sub?.customer_id ?? "")
  )
  if (loading)
    return (
      <p className="text-sm text-muted-foreground">
        <AdminMessage id={"Loading…"} />
      </p>
    )
  if (!sub)
    return (
      <MissingRecord
        title="Subscription not found"
        error={error}
        back="/subscriptions"
        backLabel="Subscriptions"
      />
    )

  const cancellable =
    sub.status === "active" ||
    sub.status === "past_due" ||
    sub.status === "awaiting_method" ||
    sub.status === "unverified"
  // What the server changes: the plan of an active or past-due subscription;
  // the card of one still collecting, or of a renewal OpenRails can resume.
  const changeable = sub.status === "active" || sub.status === "past_due"
  const cardChangeable =
    changeable ||
    sub.status === "awaiting_method" ||
    (sub.resumable && sub.collection_policy === "engine")
  const customerLabel = customer ? customerName(customer) : undefined
  const planName = sub.product?.display_name

  return (
    <div className="flex flex-col gap-4">
      <div className="flex flex-wrap items-center gap-3">
        <Button
          variant="ghost"
          size="icon"
          onClick={() => navigate(-1)}
          aria-label={adminText("Back")}
        >
          <HugeiconsIcon icon={ArrowLeft01Icon} className="size-4" />
        </Button>
        <div className="min-w-0">
          <h1 className="flex flex-wrap items-center gap-2 text-2xl font-semibold tracking-tight">
            {planName ?? adminText("Subscription")}
            <StatusBadge status={sub.status} />
          </h1>
          <p className="text-xs [overflow-wrap:anywhere] text-muted-foreground">
            {sub.id} · {sub.rail}
            {sub.rail_subscription_id ? ` · ${sub.rail_subscription_id}` : ""}
          </p>
        </div>
        <div className="flex flex-wrap gap-2 sm:ml-auto">
          {canUpdate && sub.resumable && (
            <ResumeButton id={sub.id} customerId={sub.customer_id} />
          )}
          {canUpdate && changeable && (
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
          {canUpdate && cardChangeable && (
            <ChangePaymentMethodDialog
              subscriptionId={sub.id}
              customerId={sub.customer_id}
              rail={sub.rail}
              collectionPolicy={sub.collection_policy}
            />
          )}
          {canUpdate && cancellable && (
            <CancelDialog
              id={sub.id}
              customerId={sub.customer_id}
              customer={customerLabel}
              plan={planName}
              periodEnd={sub.current_period_ends_at}
            />
          )}
        </div>
      </div>

      <div className={FACT_TILES}>
        <Fact label={adminText("Customer")}>
          {sub.customer_id ? (
            <Link
              className="text-xs underline-offset-2 hover:underline"
              to={`/customers/${sub.customer_id}`}
            >
              {customerLabel ?? shortId(sub.customer_id, 13)}
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
                  to={`/catalog/prices/${encodeURIComponent(sub.price_id)}`}
                >
                  {formatNativeAmount(
                    sub.price.unit_amount,
                    sub.price.currency
                  )}
                </Link>
              ) : (
                <Link
                  className="text-xs underline-offset-2 hover:underline"
                  to={`/catalog/prices/${encodeURIComponent(sub.price_id)}`}
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
          {sub.canceled_at ? (
            <>
              {formatDate(sub.canceled_at)}
              {sub.cancel_type && (
                <span className="block text-xs text-muted-foreground">
                  {adminText(cancelledBy[sub.cancel_type] ?? sub.cancel_type)}
                </span>
              )}
            </>
          ) : (
            "—"
          )}
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
                  <TableHead className="text-muted-foreground @max-[36rem]/page:hidden">
                    <AdminMessage id={"Kind"} />
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    <AdminMessage id={"Amount"} />
                  </TableHead>
                  <TableHead className="text-muted-foreground @max-[36rem]/page:hidden">
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
                    <TableCell className="@max-[36rem]/page:hidden">
                      {p.kind}
                    </TableCell>
                    <TableCell>
                      {formatNativeAmount(p.amount, p.currency)}
                    </TableCell>
                    <TableCell className="@max-[36rem]/page:hidden">
                      {formatDate(p.created_at)}
                    </TableCell>
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

// cancelledBy says who ended a subscription, by its cancel_type.
const cancelledBy: Record<string, string> = {
  user: "By the customer",
  merchant: "By staff",
  expired: "Renewal payment failed",
  chargeback: "After a chargeback",
  upgrade: "Replaced by an upgrade",
}

function CancelDialog({
  id,
  customerId,
  customer,
  plan,
  periodEnd,
}: {
  id: string
  customerId?: string
  customer?: string
  plan?: string
  periodEnd: string | null
}) {
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
  const revokeAccess = useStore(
    form.store,
    (state) => state.values.revokeAccess
  )
  const who = customer ?? adminText("The customer")
  const access = plan ?? adminText("their access")

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
        <AdminMessage id={"Cancel membership"} />
      </Button>
      <TypedConfirmDialog
        open={open}
        onOpenChange={handleOpenChange}
        title={adminText("Cancel this membership?")}
        description={`${adminText(
          "Renewal stops now and can't be restarted from here."
        )} ${
          revokeAccess
            ? `${who} ${adminText("loses")} ${access} ${adminText("now. No refund is made.")}`
            : `${who} ${adminText("keeps")} ${access} ${
                periodEnd
                  ? `${adminText("until")} ${formatDay(periodEnd)}.`
                  : adminText("until the current period ends.")
              }`
        }`}
        confirmationWord="CANCEL"
        actionLabel={adminText("Cancel membership")}
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
              <Label className="flex min-h-10 items-center gap-2 font-normal">
                <input
                  type="checkbox"
                  className="size-4 accent-destructive"
                  checked={field.state.value}
                  onChange={(event) => field.handleChange(event.target.checked)}
                />
                <AdminMessage
                  id={"End access now instead (no refund is made)"}
                />
              </Label>
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
