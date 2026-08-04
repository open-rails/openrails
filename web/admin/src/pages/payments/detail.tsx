import { HugeiconsIcon } from "@hugeicons/react"
import { ArrowLeft01Icon, Undo02Icon } from "@hugeicons/core-free-icons"
import * as React from "react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { toast } from "sonner"

import { Fact } from "@/components/fact-card"
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
import { useApiData } from "@/hooks/use-api-data"
import {
  getPayment,
  REFUNDABLE_RAILS,
  refundPayment,
} from "@/lib/api/endpoints"
import {
  formatMicros,
  formatUnix,
  microsFromInput,
  shortId,
} from "@/lib/format"
import { toastApiError } from "@/lib/toast"

export function PaymentDetailPage() {
  const { id = "" } = useParams()
  const navigate = useNavigate()
  const {
    data: payment,
    loading,
    error,
    reload,
  } = useApiData(() => getPayment(id), [id])
  React.useEffect(() => {
    if (error) toastApiError(error, "Load payment")
  }, [error])

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
            disabled={!refundable}
            disabledNote={railRefundNote}
            onDone={reload}
          />
        </div>
      </div>

      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Fact label="Amount">
          {formatMicros(payment.amount, payment.currency)}
        </Fact>
        <Fact label="Refunded">
          {payment.amount_refunded
            ? formatMicros(payment.amount_refunded, payment.currency)
            : "—"}
        </Fact>
        <Fact label="Customer">
          <Link
            className="text-xs underline-offset-2 hover:underline"
            to={`/customers/${payment.user.replace(/^usr_/, "")}`}
          >
            {shortId(payment.user, 16)}
          </Link>
        </Fact>
        <Fact label="Subscription">
          {payment.subscription ? (
            <Link
              className="text-xs underline-offset-2 hover:underline"
              to={`/subscriptions/${payment.subscription.replace(/^sub_/, "")}`}
            >
              {shortId(payment.subscription, 16)}
            </Link>
          ) : (
            "—"
          )}
        </Fact>
        <Fact label="Created">{formatUnix(payment.created)}</Fact>
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
                <TableRow>
                  <TableHead>Refund</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Amount</TableHead>
                  <TableHead>Created</TableHead>
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
                    <TableCell>{formatMicros(r.amount, r.currency)}</TableCell>
                    <TableCell>{formatUnix(r.created)}</TableCell>
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
  disabled,
  disabledNote,
  onDone,
}: {
  payment: {
    id: string
    amount: number
    amount_refunded: number
    currency: string
    rail: string
  }
  disabled: boolean
  disabledNote?: string
  onDone: () => void
}) {
  const [open, setOpen] = React.useState(false)
  const remaining = payment.amount - payment.amount_refunded
  const [amount, setAmount] = React.useState(String(remaining / 1_000_000))
  const [reason, setReason] = React.useState("")
  const [revokeAccess, setRevokeAccess] = React.useState(false)
  const [busy, setBusy] = React.useState(false)

  return (
    <>
      <Button
        variant="destructive"
        size="sm"
        disabled={disabled}
        title={disabledNote}
        onClick={() => setOpen(true)}
      >
        <HugeiconsIcon icon={Undo02Icon} className="size-4" /> Refund
      </Button>
      <Dialog open={open} onOpenChange={setOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>Refund payment</DialogTitle>
            <DialogDescription>
              Executes the refund at the {payment.rail} rail. Remaining
              refundable: {formatMicros(remaining, payment.currency)}.
            </DialogDescription>
          </DialogHeader>
          <div className="grid gap-3">
            <div className="grid gap-1.5">
              <Label htmlFor="refund-amount">Amount ({payment.currency})</Label>
              <Input
                id="refund-amount"
                type="number"
                step="any"
                min="0"
                value={amount}
                onChange={(e) => setAmount(e.target.value)}
              />
            </div>
            <div className="grid gap-1.5">
              <Label htmlFor="refund-reason">Reason (optional)</Label>
              <Input
                id="refund-reason"
                value={reason}
                onChange={(e) => setReason(e.target.value)}
              />
            </div>
            <div className="flex items-center gap-2">
              <Switch
                id="refund-revoke"
                checked={revokeAccess}
                onCheckedChange={setRevokeAccess}
              />
              <Label htmlFor="refund-revoke">Also revoke granted access</Label>
            </div>
          </div>
          <DialogFooter>
            <Button
              variant="outline"
              onClick={() => setOpen(false)}
              disabled={busy}
            >
              Close
            </Button>
            <Button
              variant="destructive"
              disabled={busy || !microsFromInput(amount)}
              onClick={async () => {
                const micros = microsFromInput(amount)
                if (!micros || micros <= 0) return
                setBusy(true)
                try {
                  await refundPayment(payment.id, micros, reason, revokeAccess)
                  toast.success("Refund submitted")
                  setOpen(false)
                  onDone()
                } catch (err) {
                  toastApiError(err, "Refund payment")
                } finally {
                  setBusy(false)
                }
              }}
            >
              {busy ? "Refunding…" : "Refund"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
