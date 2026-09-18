import { HugeiconsIcon } from "@hugeicons/react"
import { ArrowLeft01Icon, Undo02Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { toast } from "sonner"
import { useForm } from "@tanstack/react-form"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"

import { Fact } from "@/components/fact-card"
import { FormFieldErrors } from "@/components/form-field-errors"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
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
import { REFUNDABLE_RAILS } from "@/lib/api/endpoints"
import { DIALOG_FORM } from "@/lib/dialog-width"
import {
  formatNativeAmount,
  formatDate,
  nativeAmountFromInput,
  nativeAmountToInput,
  shortId,
} from "@/lib/format"
import { adminMutations } from "@/lib/mutations"
import { adminQueries } from "@/lib/queries"
import { toastApiError } from "@/lib/toast"

export function PaymentDetailPage() {
  const { id = "" } = useParams()
  const navigate = useNavigate()
  const { data: payment, isPending: loading } = useQuery(
    adminQueries.payment(id)
  )

  if (loading) return <p className="text-sm text-muted-foreground">Loading…</p>
  if (!payment)
    return <p className="text-sm text-muted-foreground">Payment not found.</p>

  const refundable =
    payment.object === "charge" &&
    REFUNDABLE_RAILS.includes(payment.rail) &&
    payment.status !== "refunded" &&
    payment.status !== "failed" &&
    payment.status !== "pending"
  const railRefundNote = REFUNDABLE_RAILS.includes(payment.rail)
    ? undefined
    : `Refunds are not supported via the ${payment.rail} rail API`

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
            {payment.id} <StatusBadge status={payment.status} />
          </h2>
          <p className="text-xs text-muted-foreground">
            {payment.rail} · txn {payment.transaction_id || "—"}
          </p>
        </div>
        <div className="ml-auto">
          <RefundDialog
            payment={payment}
            customerId={payment.customer_id}
            subscriptionId={payment.subscription_id}
            disabled={!refundable}
            disabledNote={railRefundNote}
          />
        </div>
      </div>

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Fact label="Amount">
          {formatNativeAmount(payment.amount, payment.currency)}
        </Fact>
        <Fact label="Refunded">
          {payment.amount_refunded
            ? formatNativeAmount(payment.amount_refunded, payment.currency)
            : "—"}
        </Fact>
        <Fact label="Customer">
          <Link
            className="text-xs underline-offset-2 hover:underline"
            to={`/customers/${payment.customer_id}`}
          >
            {shortId(payment.customer_id, 16)}
          </Link>
        </Fact>
        <Fact label="Subscription">
          {payment.subscription_id ? (
            <Link
              className="text-xs underline-offset-2 hover:underline"
              to={`/subscriptions/${payment.subscription_id}`}
            >
              {shortId(payment.subscription_id, 16)}
            </Link>
          ) : (
            "—"
          )}
        </Fact>
        <Fact label="Created">{formatDate(payment.created_at)}</Fact>
        <Fact label="Type">{payment.object}</Fact>
        {payment.failure_message && (
          <Fact label="Failure">{payment.failure_message}</Fact>
        )}
      </div>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Refunds</CardTitle>
        </CardHeader>
        <CardContent>
          {!payment.refunds?.data?.length ? (
            <p className="text-sm text-muted-foreground">No refunds.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow className="hover:bg-transparent">
                  <TableHead className="text-muted-foreground">
                    Refund
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    Status
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    Amount
                  </TableHead>
                  <TableHead className="text-muted-foreground">
                    Created
                  </TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {payment.refunds.data.map((r) => (
                  <TableRow key={r.id}>
                    <TableCell className="text-xs">
                      {shortId(r.id, 16)}
                    </TableCell>
                    <TableCell>
                      <StatusBadge status={r.status} />
                    </TableCell>
                    <TableCell>
                      {formatNativeAmount(r.amount, r.currency)}
                    </TableCell>
                    <TableCell>{formatDate(r.created_at)}</TableCell>
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

function RefundDialog({
  payment,
  customerId,
  subscriptionId,
  disabled,
  disabledNote,
}: {
  payment: {
    id: string
    amount: string
    amount_refunded: string
    currency: string
    rail: string
  }
  customerId: string
  subscriptionId?: string
  disabled: boolean
  disabledNote?: string
}) {
  const [open, setOpen] = React.useState(false)
  const remaining = (
    BigInt(payment.amount) - BigInt(payment.amount_refunded)
  ).toString()
  const queryClient = useQueryClient()
  const refund = useMutation(
    adminMutations.refundPayment(
      queryClient,
      payment.id,
      customerId,
      subscriptionId
    )
  )
  const form = useForm({
    defaultValues: {
      amount: nativeAmountToInput(remaining, payment.currency),
      reason: "",
      revokeAccess: false,
    },
    onSubmit: async ({ value }) => {
      const amount = nativeAmountFromInput(value.amount, payment.currency)
      if (
        amount === null ||
        BigInt(amount) <= 0n ||
        BigInt(amount) > BigInt(remaining)
      )
        return
      try {
        await refund.mutateAsync({
          amount,
          reason: value.reason,
          revokeAccess: value.revokeAccess,
        })
        toast.success("Refund submitted")
        handleOpenChange(false)
      } catch (err) {
        toastApiError(err, "Refund payment")
      }
    },
  })

  const handleOpenChange = (next: boolean) => {
    setOpen(next)
    if (next) {
      form.reset({
        amount: nativeAmountToInput(remaining, payment.currency),
        reason: "",
        revokeAccess: false,
      })
    } else {
      form.reset()
      refund.reset()
    }
  }

  return (
    <>
      <Button
        variant="destructive"
        size="sm"
        disabled={disabled}
        title={disabledNote}
        onClick={() => handleOpenChange(true)}
      >
        <HugeiconsIcon icon={Undo02Icon} className="size-4" /> Refund
      </Button>
      <Dialog open={open} onOpenChange={handleOpenChange}>
        <DialogContent className={DIALOG_FORM}>
          <DialogHeader>
            <DialogTitle>Refund payment</DialogTitle>
            <DialogDescription>
              Executes the refund at the {payment.rail} rail. Remaining
              refundable: {formatNativeAmount(remaining, payment.currency)}.
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
                name="amount"
                validators={{
                  onChange: ({ value }) => {
                    const amount = nativeAmountFromInput(
                      value,
                      payment.currency
                    )
                    if (amount === null || BigInt(amount) <= 0n) {
                      return "Enter an amount greater than zero"
                    }
                    return BigInt(amount) > BigInt(remaining)
                      ? "Amount exceeds the remaining refundable balance"
                      : undefined
                  },
                }}
              >
                {(field) => (
                  <div className="grid gap-1.5">
                    <Label htmlFor="refund-amount">
                      Amount ({payment.currency})
                    </Label>
                    <Input
                      id="refund-amount"
                      type="number"
                      step="any"
                      min="0"
                      max={nativeAmountToInput(remaining, payment.currency)}
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
              <form.Field name="reason">
                {(field) => (
                  <div className="grid gap-1.5">
                    <Label htmlFor="refund-reason">Reason (optional)</Label>
                    <Input
                      id="refund-reason"
                      value={field.state.value}
                      onBlur={field.handleBlur}
                      onChange={(event) =>
                        field.handleChange(event.target.value)
                      }
                    />
                  </div>
                )}
              </form.Field>
              <form.Field name="revokeAccess">
                {(field) => (
                  <div className="flex items-center gap-2">
                    <Switch
                      id="refund-revoke"
                      checked={field.state.value}
                      onCheckedChange={field.handleChange}
                    />
                    <Label htmlFor="refund-revoke">
                      Also revoke granted access
                    </Label>
                  </div>
                )}
              </form.Field>
            </div>
            <DialogFooter>
              <form.Subscribe
                selector={(state) =>
                  [
                    state.values.amount,
                    state.canSubmit,
                    state.isSubmitting,
                  ] as const
                }
              >
                {([amountInput, canSubmit, isSubmitting]) => (
                  <>
                    <Button
                      type="button"
                      variant="outline"
                      onClick={() => handleOpenChange(false)}
                      disabled={isSubmitting}
                    >
                      Close
                    </Button>
                    <Button
                      type="submit"
                      variant="destructive"
                      disabled={
                        !nativeAmountFromInput(amountInput, payment.currency) ||
                        !canSubmit ||
                        isSubmitting
                      }
                    >
                      {isSubmitting ? "Refunding…" : "Refund"}
                    </Button>
                  </>
                )}
              </form.Subscribe>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
    </>
  )
}
