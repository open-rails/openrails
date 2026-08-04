import { HugeiconsIcon } from "@hugeicons/react"
import { ArrowLeft01Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { toast } from "sonner"

import { Fact } from "@/components/fact-card"
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
import { useApiData } from "@/hooks/use-api-data"
import {
  cancelReprice,
  cancelSubscription,
  changeSubscriptionPaymentMethod,
  getCustomerPaymentMethods,
  getPrice,
  getSubscription,
  listReprices,
  resumeSubscription,
} from "@/lib/api/endpoints"
import type { SubscriptionReprice } from "@/lib/api/types"
import { formatDate, formatMicros, shortId } from "@/lib/format"
import { toastApiError } from "@/lib/toast"

export function SubscriptionDetailPage() {
  const { id = "" } = useParams()
  const navigate = useNavigate()
  const {
    data: sub,
    loading,
    error,
    reload,
  } = useApiData(() => getSubscription(id), [id])
  // #777: pending-reprice badge — at most one scheduled reprice can exist per
  // subscription at a time.
  const { data: scheduledReprices, reload: reloadReprices } = useApiData(
    () =>
      id
        ? listReprices({ subscription_id: id, status: "scheduled" })
        : Promise.resolve(null),
    [id]
  )
  const pendingReprice = scheduledReprices?.items?.[0]
  React.useEffect(() => {
    if (error) toastApiError(error, "Load subscription")
  }, [error])

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
          {resumable && <ResumeButton id={sub.id} onDone={reload} />}
          <ChangePaymentMethodDialog
            subscriptionId={sub.id}
            customerId={sub.customer_id}
            rail={sub.rail}
            onDone={reload}
          />
          {cancellable && <CancelDialog id={sub.id} onDone={reload} />}
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
                reprice={pendingReprice}
                onCancelled={() => {
                  reloadReprices()
                  reload()
                }}
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

function CancelDialog({ id, onDone }: { id: string; onDone: () => void }) {
  const [open, setOpen] = React.useState(false)
  const [reason, setReason] = React.useState("")
  const [revokeAccess, setRevokeAccess] = React.useState(false)
  return (
    <>
      <Button variant="destructive" size="sm" onClick={() => setOpen(true)}>
        Cancel subscription
      </Button>
      <TypedConfirmDialog
        open={open}
        onOpenChange={setOpen}
        title="Cancel subscription"
        description="Terminal cancellation at the payment rail. This is a last resort — dunning parks subscriptions as past_due without losing entitlements."
        confirmationWord="CANCEL"
        actionLabel="Cancel subscription"
        onConfirm={async () => {
          try {
            await cancelSubscription(id, reason, revokeAccess)
            toast.success("Subscription cancelled")
            onDone()
          } catch (err) {
            toastApiError(err, "Cancel subscription")
            throw err
          }
        }}
      >
        <div className="grid gap-3">
          <div className="grid gap-1.5">
            <Label htmlFor="cancel-reason">Reason</Label>
            <Input
              id="cancel-reason"
              value={reason}
              onChange={(e) => setReason(e.target.value)}
              placeholder="why is this being cancelled?"
            />
          </div>
          <div className="flex items-center gap-2">
            <Switch
              id="cancel-revoke"
              checked={revokeAccess}
              onCheckedChange={setRevokeAccess}
            />
            <Label htmlFor="cancel-revoke">
              Also revoke access immediately
            </Label>
          </div>
        </div>
      </TypedConfirmDialog>
    </>
  )
}

function ResumeButton({ id, onDone }: { id: string; onDone: () => void }) {
  const [busy, setBusy] = React.useState(false)
  return (
    <Button
      variant="outline"
      size="sm"
      disabled={busy}
      onClick={async () => {
        setBusy(true)
        try {
          await resumeSubscription(id)
          toast.success("Resume queued")
          onDone()
        } catch (err) {
          toastApiError(err, "Resume subscription")
        } finally {
          setBusy(false)
        }
      }}
    >
      {busy ? "Queuing…" : "Resume"}
    </Button>
  )
}

function ChangePaymentMethodDialog({
  subscriptionId,
  customerId,
  rail,
  onDone,
}: {
  subscriptionId: string
  customerId?: string
  rail: string
  onDone: () => void
}) {
  const [open, setOpen] = React.useState(false)
  const [paymentMethodId, setPaymentMethodId] = React.useState("")
  const [busy, setBusy] = React.useState(false)
  const { data: pms } = useApiData(
    () =>
      open && customerId
        ? getCustomerPaymentMethods(customerId)
        : Promise.resolve(null),
    [open, customerId]
  )
  // Payment-method swap is an NMI-only operation today (see
  // update_subscription_payment_method.go); other rails 400.
  const supported = rail === "nmi"
  return (
    <Dialog open={open} onOpenChange={setOpen}>
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
        <div className="grid gap-1.5">
          <Label>Payment method</Label>
          <Select
            value={paymentMethodId}
            onValueChange={(value) => setPaymentMethodId(value ?? "")}
          >
            <SelectTrigger>
              <SelectValue placeholder="Pick a stored payment method" />
            </SelectTrigger>
            <SelectContent>
              {(pms?.data ?? [])
                .filter((pm) => pm.rail === rail)
                .map((pm) => (
                  <SelectItem key={pm.id} value={pm.id}>
                    {pm.card?.brand ?? pm.type} •••• {pm.card?.last4 ?? "????"}{" "}
                    ({pm.rail})
                  </SelectItem>
                ))}
            </SelectContent>
          </Select>
        </div>
        <DialogFooter>
          <Button
            disabled={!paymentMethodId || busy}
            onClick={async () => {
              setBusy(true)
              try {
                await changeSubscriptionPaymentMethod(
                  subscriptionId,
                  paymentMethodId
                )
                toast.success("Payment method updated")
                setOpen(false)
                onDone()
              } catch (err) {
                toastApiError(err, "Change payment method")
              } finally {
                setBusy(false)
              }
            }}
          >
            {busy ? "Updating…" : "Update"}
          </Button>
        </DialogFooter>
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
  reprice,
  onCancelled,
}: {
  reprice: SubscriptionReprice
  onCancelled: () => void
}) {
  const { data: toPrice } = useApiData(
    () => getPrice(reprice.to_price_id),
    [reprice.to_price_id]
  )
  const [busy, setBusy] = React.useState(false)
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
        disabled={busy}
        onClick={async () => {
          setBusy(true)
          try {
            await cancelReprice(reprice.id)
            toast.success("Reprice canceled")
            onCancelled()
          } catch (err) {
            toastApiError(err, "Cancel reprice")
          } finally {
            setBusy(false)
          }
        }}
      >
        cancel
      </Button>
    </span>
  )
}
