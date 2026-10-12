import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Link, useParams } from "react-router-dom"
import type { ColumnDef } from "@tanstack/react-table"
import { toast } from "sonner"
import { CursorPager } from "@/components/cursor-pager"
import { DataTable } from "@/components/data-table"
import { StatusBadge } from "@/components/status-badge"
import { Button } from "@/components/ui/button"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from "@/components/ui/dialog"
import { useCursorPages } from "@/lib/cursor-pages"
import type { Order, OrderUsage, Payment } from "@/lib/api/generated/wire"
import type { UsageAction } from "@/lib/api/order-endpoints"
import { orderQueries, usageActionMutation } from "@/lib/order-queries"
import { useAdminUpdates } from "@/lib/capabilities"
import {
  currencyScale,
  formatDate,
  formatNativeAmount,
  shortId,
} from "@/lib/format"
import { taxDisplay } from "../customers/invoice-profile-model"
import {
  usageActionDescriptions,
  usageActionLabels,
  usageActions,
  usagePaymentAmount,
  usageResultMessage,
} from "./model"

const historyColumns: ColumnDef<Payment, unknown>[] = [
  {
    header: "Paid",
    cell: ({ row }) => formatDate(row.original.created_at),
  },
  {
    header: "Amount",
    cell: ({ row }) =>
      formatNativeAmount(row.original.amount, row.original.currency),
  },
  {
    header: "Status",
    cell: ({ row }) => <StatusBadge status={row.original.status} />,
  },
  {
    header: "Method",
    cell: ({ row }) => row.original.rail ?? row.original.channel,
  },
  {
    header: "Reference",
    cell: ({ row }) => row.original.transaction_id || shortId(row.original.id),
  },
]
export function OrderDetailPage() {
  const { id = "" } = useParams()
  const { data: order, isPending, error } = useQuery(orderQueries.detail(id))
  if (isPending) return <p>Loading order…</p>
  if (error)
    return (
      <p role="alert" className="text-destructive">
        {error.message}
      </p>
    )
  if (!order) return <p>Order not found.</p>
  return <OrderDetail key={order.id} order={order} />
}
export function OrderDetail({ order }: { order: Order }) {
  const paging = useCursorPages(order.id, 25)
  const history = useQuery(
    orderQueries.payments(order.id, paging.size, paging.cursor)
  )
  const failure = order.payment.last_payment_error
  return (
    <div className="flex flex-col gap-5">
      <Link to="/orders" className="text-sm underline">
        ← Orders
      </Link>
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">{order.number ?? order.id}</h1>
        <StatusBadge status={order.status} />
        {order.usage?.delinquent && <StatusBadge status="delinquent" />}
      </div>
      <p className="text-sm">
        Customer{" "}
        <Link className="underline" to={`/customers/${order.customer_id}`}>
          {order.customer_id}
        </Link>{" "}
        · {order.origin} · {order.currency}
      </p>
      <div className="grid gap-4 md:grid-cols-3">
        {[
          ["Total", order.total],
          ...(order.usage
            ? [
                ["Paid", order.usage.amount_paid],
                ["Unpaid", order.usage.amount_due],
              ]
            : []),
        ].map(([label, value]) => (
          <Card key={String(label)}>
            <CardHeader>
              <CardTitle>{label}</CardTitle>
            </CardHeader>
            <CardContent className="text-xl">
              {formatNativeAmount(value, order.currency)}
            </CardContent>
          </Card>
        ))}
      </div>
      {order.usage && <UsageFacts usage={order.usage} />}
      {order.renewal && (
        <Card>
          <CardHeader>
            <CardTitle>Renewal</CardTitle>
          </CardHeader>
          <CardContent className="grid gap-3 text-sm md:grid-cols-2">
            <p>
              Subscription:{" "}
              <Link
                className="underline"
                to={`/subscriptions/${order.renewal.subscription_id}`}
              >
                {shortId(order.renewal.subscription_id)}
              </Link>
            </p>
            <p>
              Period: {formatDate(order.renewal.period_starts_at)} –{" "}
              {formatDate(order.renewal.period_ends_at)}
            </p>
            <p>Due: {formatDate(order.renewal.due_at)}</p>
          </CardContent>
        </Card>
      )}
      <Card>
        <CardHeader>
          <CardTitle>Lines</CardTitle>
        </CardHeader>
        <CardContent>
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left">
                <th>Description</th>
                <th>Quantity</th>
                <th className="text-right">Amount</th>
              </tr>
            </thead>
            <tbody>
              {order.lines.map((line) => (
                <tr key={line.id}>
                  <td className="py-2">
                    {line.description}
                    {line.source && (
                      <p className="text-xs text-muted-foreground">
                        {line.source}
                      </p>
                    )}
                  </td>
                  <td>{line.quantity ?? "—"}</td>
                  <td className="text-right">
                    {formatNativeAmount(line.amount, order.currency)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {!order.lines.length && <p>No lines.</p>}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Payment</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3 text-sm">
          <p>
            Status: <StatusBadge status={order.payment.status} />
          </p>
          {failure && <p>Last failure: {failure.message}</p>}
          {order.usage && <UsageActions order={order} />}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Payment history</CardTitle>
        </CardHeader>
        <CardContent>
          {history.error ? (
            <p role="alert" className="text-destructive">
              {history.error.message}
            </p>
          ) : (
            <>
              <DataTable
                columns={historyColumns}
                data={history.data?.data ?? []}
                loading={history.isPending}
                emptyMessage="No payments recorded."
              />
              <CursorPager
                pages={paging}
                rows={history.data?.data?.length ?? 0}
                nextCursor={history.data?.next_cursor}
                busy={history.isFetching}
              />
            </>
          )}
        </CardContent>
      </Card>
    </div>
  )
}

function UsageFacts({ usage }: { usage: OrderUsage }) {
  return (
    <Card>
      <CardHeader>
        <CardTitle>Usage billed</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm md:grid-cols-2">
        <p>
          Period: {formatDate(usage.period_starts_at)} –{" "}
          {formatDate(usage.period_ends_at)}
        </p>
        <p>Due: {formatDate(usage.due_at)}</p>
        <p>
          Collection:{" "}
          {usage.collection_method === "send_invoice"
            ? "Manual remittance"
            : "Saved payment method"}
        </p>
        <p>Next attempt: {formatDate(usage.next_collection_attempt_at)}</p>
        <p>Purchase order: {usage.po_number ?? "—"}</p>
        <p>Memo: {usage.memo ?? "—"}</p>
        <div>
          <p className="font-medium">Billing contacts</p>
          {usage.billing_contacts.length ? (
            usage.billing_contacts.map((c, i) => (
              <p key={i}>
                {c.name ? `${c.name} · ` : ""}
                {c.email}
              </p>
            ))
          ) : (
            <p>—</p>
          )}
        </div>
        <div>
          <p className="font-medium">Tax details</p>
          {Object.entries(usage.tax ?? {}).map(([key, value]) => (
            <p key={key}>
              {key}: {taxDisplay(value)}
            </p>
          ))}
        </div>
        <p className="text-muted-foreground md:col-span-2">
          These are the customer&apos;s invoice profile when the order was
          billed. Editing the profile affects later orders.
        </p>
      </CardContent>
    </Card>
  )
}

function UsageActions({ order }: { order: Order }) {
  const canUpdate = useAdminUpdates()
  const [action, setAction] = useState<UsageAction | null>(null)
  const [amount, setAmount] = useState("")
  const [reference, setReference] = useState("")
  const [error, setError] = useState<string | null>(null)
  const client = useQueryClient()
  const mutation = useMutation(usageActionMutation(client, order.customer_id))
  const actions = canUpdate ? usageActions(order) : []
  async function submit() {
    if (!action || mutation.isPending) return
    setError(null)
    try {
      if (!actions.includes(action))
        throw new Error(
          "This action is no longer available. Refresh the order before continuing."
        )
      if (action === "record_payment" && !reference.trim())
        throw new Error("Enter the bank or payment reference.")
      let paid: string | undefined
      if (action === "record_payment") {
        const scale = currencyScale(order.currency)
        if (scale === undefined)
          throw new Error(`${order.currency} is not a registered currency.`)
        paid = usagePaymentAmount(amount, order.usage!.amount_due, scale)
      }
      const result = await mutation.mutateAsync({
        id: order.id,
        action,
        amount: paid,
        reference: reference.trim(),
      })
      toast(usageResultMessage(result))
      setAction(null)
    } catch (failure) {
      setError(
        failure instanceof Error ? failure.message : "Order action failed."
      )
    }
  }
  return (
    <>
      {order.status === "processing" && (
        <p role="status">
          A collection of this order is in progress. It must finish, or be
          resolved with <code>openrails intents resolve</code>, before another
          action.
        </p>
      )}
      <div className="flex flex-wrap gap-2">
        {actions.map((value) => (
          <Button
            key={value}
            variant={value === "waive_balance" ? "destructive" : "outline"}
            onClick={() => {
              setAction(value)
              setError(null)
            }}
          >
            {usageActionLabels[value]}
          </Button>
        ))}
      </div>
      {!actions.length && order.status !== "processing" && (
        <p className="text-muted-foreground">
          No actions are available for your permissions and this order&apos;s
          state.
        </p>
      )}
      <Dialog
        open={action !== null}
        onOpenChange={(open) => {
          if (!open && !mutation.isPending) setAction(null)
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {action ? usageActionLabels[action] : "Order action"}
            </DialogTitle>
            <DialogDescription>
              {action ? usageActionDescriptions[action] : ""}
            </DialogDescription>
          </DialogHeader>
          {action === "record_payment" && (
            <div className="space-y-3">
              <div>
                <Label htmlFor="order-payment-amount">
                  Amount ({order.currency})
                </Label>
                <Input
                  id="order-payment-amount"
                  inputMode="decimal"
                  value={amount}
                  onChange={(e) => setAmount(e.target.value)}
                />
              </div>
              <div>
                <Label htmlFor="order-payment-reference">
                  Payment reference
                </Label>
                <Input
                  id="order-payment-reference"
                  maxLength={255}
                  value={reference}
                  onChange={(e) => setReference(e.target.value)}
                />
              </div>
            </div>
          )}
          {error && (
            <p role="alert" className="text-destructive">
              {error}
            </p>
          )}
          <DialogFooter>
            <Button
              variant="outline"
              disabled={mutation.isPending}
              onClick={() => setAction(null)}
            >
              Cancel
            </Button>
            <Button
              disabled={
                mutation.isPending || !action || !actions.includes(action)
              }
              onClick={() => void submit()}
            >
              {mutation.isPending ? "Working…" : "Confirm"}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </>
  )
}
