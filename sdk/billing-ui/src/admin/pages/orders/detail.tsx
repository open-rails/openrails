import { AdminMessage, useAdminUI } from "#orck/admin/ui-context"
import { useAdminServices } from "#orck/admin/context"
import { useState } from "react"
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query"
import { Link, useParams } from "#orck/admin/router"
import type { ColumnDef } from "@tanstack/react-table"

import { CursorPager } from "#orck/admin/components/cursor-pager"
import { DataTable } from "#orck/admin/components/data-table"
import { StatusBadge } from "#orck/admin/components/status-badge"
import { Button } from "#orck/admin/components/ui/button"
import { Input } from "#orck/admin/components/ui/input"
import { Label } from "#orck/admin/components/ui/label"
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "#orck/admin/components/ui/card"
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from "#orck/admin/components/ui/dialog"
import { useCursorPages } from "#orck/admin/lib/cursor-pages"
import { ApiError } from "#orck/admin/lib/api/client"
import type { Order, OrderUsage, Payment } from "#orck/client/generated/wire"
import type { UsageAction } from "#orck/admin/lib/api/order-endpoints"

import { useAdminUpdates } from "#orck/admin/lib/capabilities"
import {
  currencyScale,
  formatCard,
  formatDate,
  formatNativeAmount,
  shortId,
} from "#orck/admin/lib/format"
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
  const { orderQueries } = useAdminServices()

  const { id = "" } = useParams()
  const { data: order, isPending, error } = useQuery(orderQueries.detail(id))
  if (isPending)
    return (
      <p>
        <AdminMessage id={"Loading order…"} />
      </p>
    )
  if (error)
    return (
      <p role="alert" className="text-destructive">
        {error.message}
      </p>
    )
  if (!order)
    return (
      <p>
        <AdminMessage id={"Order not found."} />
      </p>
    )
  return <OrderDetail key={order.id} order={order} />
}
export function OrderDetail({ order }: { order: Order }) {
  const { orderQueries } = useAdminServices()

  const paging = useCursorPages(order.id, 25)
  const history = useQuery(
    orderQueries.payments(order.id, paging.size, paging.cursor)
  )
  const failure = order.payment.last_payment_error
  return (
    <div className="flex flex-col gap-5">
      <Link to="/orders" className="text-sm underline">
        <AdminMessage id={"← Orders"} />
      </Link>
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">{order.number ?? order.id}</h1>
        <StatusBadge status={order.status} />
        {order.usage?.delinquent && <StatusBadge status="delinquent" />}
      </div>
      <p className="text-sm">
        <AdminMessage id={"Customer"} />{" "}
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
            <CardTitle>
              <AdminMessage id={"Renewal"} />
            </CardTitle>
          </CardHeader>
          <CardContent className="grid gap-3 text-sm md:grid-cols-2">
            <p>
              <AdminMessage id={"Subscription:"} />{" "}
              <Link
                className="underline"
                to={`/subscriptions/${order.renewal.subscription_id}`}
              >
                {shortId(order.renewal.subscription_id)}
              </Link>
            </p>
            <p>
              <AdminMessage id={"Period: "} />
              {formatDate(order.renewal.period_starts_at)} –{" "}
              {formatDate(order.renewal.period_ends_at)}
            </p>
            <p>
              <AdminMessage id={"Due: "} />
              {formatDate(order.renewal.due_at)}
            </p>
          </CardContent>
        </Card>
      )}
      <Card>
        <CardHeader>
          <CardTitle>
            <AdminMessage id={"Lines"} />
          </CardTitle>
        </CardHeader>
        <CardContent>
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left">
                <th>
                  <AdminMessage id={"Description"} />
                </th>
                <th>
                  <AdminMessage id={"Quantity"} />
                </th>
                <th className="text-right">
                  <AdminMessage id={"Amount"} />
                </th>
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
          {!order.lines.length && (
            <p>
              <AdminMessage id={"No lines."} />
            </p>
          )}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>
            <AdminMessage id={"Payment"} />
          </CardTitle>
        </CardHeader>
        <CardContent className="space-y-3 text-sm">
          <p>
            <AdminMessage id={"Status: "} />
            <StatusBadge status={order.payment.status} />
          </p>
          {failure && (
            <p>
              <AdminMessage id={"Last failure: "} />
              {failure.message}
            </p>
          )}
          {order.usage && <UsageActions order={order} />}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>
            <AdminMessage id={"Payment history"} />
          </CardTitle>
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
        <CardTitle>
          <AdminMessage id={"Usage billed"} />
        </CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3 text-sm md:grid-cols-2">
        <p>
          <AdminMessage id={"Period: "} />
          {formatDate(usage.period_starts_at)} –{" "}
          {formatDate(usage.period_ends_at)}
        </p>
        <p>
          <AdminMessage id={"Due: "} />
          {formatDate(usage.due_at)}
        </p>
        <p>
          <AdminMessage id={"Collection:"} />{" "}
          {usage.collection_method === "send_invoice"
            ? "Manual remittance"
            : "Saved payment method"}
        </p>
        <p>
          <AdminMessage id={"Next attempt: "} />
          {formatDate(usage.next_collection_attempt_at)}
        </p>
        <p>
          <AdminMessage id={"Purchase order: "} />
          {usage.po_number ?? "—"}
        </p>
        <p>
          <AdminMessage id={"Memo: "} />
          {usage.memo ?? "—"}
        </p>
        <div>
          <p className="font-medium">
            <AdminMessage id={"Billing contacts"} />
          </p>
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
          <p className="font-medium">
            <AdminMessage id={"Tax details"} />
          </p>
          {Object.entries(usage.tax ?? {}).map(([key, value]) => (
            <p key={key}>
              {key}: {taxDisplay(value)}
            </p>
          ))}
        </div>
        <p className="text-muted-foreground md:col-span-2">
          <AdminMessage
            id={
              "These are the customer&apos;s invoice profile when the order was billed. Editing the profile affects later orders."
            }
          />
        </p>
      </CardContent>
    </Card>
  )
}

function UsageActions({ order }: { order: Order }) {
  const { toast, usageActionMutation, adminQueries } = useAdminServices()

  const { id: adminId } = useAdminUI()

  const canUpdate = useAdminUpdates()
  const [action, setAction] = useState<UsageAction | null>(null)
  const [amount, setAmount] = useState("")
  const [reference, setReference] = useState("")
  const [paymentMethod, setPaymentMethod] = useState("")
  const [retryKey, setRetryKey] = useState(() => crypto.randomUUID())
  const [error, setError] = useState<string | null>(null)
  const client = useQueryClient()
  const mutation = useMutation(usageActionMutation(client, order.customer_id))
  // The retry picker offers the customer's saved cards, read on demand.
  const methods = useQuery(
    adminQueries.customerPaymentMethods(
      action === "retry_collection" ? order.customer_id : undefined
    )
  )
  const actions = canUpdate ? usageActions(order) : []
  async function submit() {
    if (!action || mutation.isPending) return
    setError(null)
    try {
      if (!actions.includes(action))
        throw new Error(
          "This action is no longer available. Refresh the order before continuing."
        )
      if (action === "retry_collection" && !paymentMethod)
        throw new Error("Choose a saved payment method.")
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
        paymentMethodId: paymentMethod,
        idempotencyKey: retryKey,
      })
      toast(usageResultMessage(result))
      // A finished collection needs a new key for the next.
      if (action === "retry_collection") setRetryKey(crypto.randomUUID())
      setAction(null)
    } catch (failure) {
      // A declined charge is settled; any other failure replays its key.
      if (failure instanceof ApiError && failure.status === 402)
        setRetryKey(crypto.randomUUID())
      setError(
        failure instanceof Error ? failure.message : "Order action failed."
      )
    }
  }
  return (
    <>
      {order.status === "processing" && (
        <p role="status">
          <AdminMessage
            id={
              "A collection of this order is in progress. It must finish, or be resolved with "
            }
          />
          <code>
            <AdminMessage id={"openrails intents resolve"} />
          </code>
          <AdminMessage id={", before another action."} />
        </p>
      )}
      <div className="flex flex-wrap gap-2">
        {actions.map((value) => (
          <Button
            key={value}
            variant={value === "void" ? "destructive" : "outline"}
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
          <AdminMessage
            id={
              "No actions are available for your permissions and this order&apos;s state."
            }
          />
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
                <Label htmlFor={adminId("order-payment-amount")}>
                  <AdminMessage id={"Amount ("} />
                  {order.currency})
                </Label>
                <Input
                  id={adminId("order-payment-amount")}
                  inputMode="decimal"
                  value={amount}
                  onChange={(e) => setAmount(e.target.value)}
                />
              </div>
              <div>
                <Label htmlFor={adminId("order-payment-reference")}>
                  <AdminMessage id={"Payment reference"} />
                </Label>
                <Input
                  id={adminId("order-payment-reference")}
                  maxLength={255}
                  value={reference}
                  onChange={(e) => setReference(e.target.value)}
                />
              </div>
            </div>
          )}
          {action === "retry_collection" && (
            <div>
              {methods.error ? (
                <p role="alert" className="mb-2 text-destructive">
                  {methods.error.message}
                </p>
              ) : (
                methods.data?.length === 0 && (
                  <p className="mb-2 text-sm">
                    <AdminMessage
                      id={
                        "No saved payment methods. Add one on the customer page before retrying collection."
                      }
                    />
                  </p>
                )
              )}
              <Label htmlFor={adminId("order-payment-method")}>
                <AdminMessage id={"Saved payment method"} />
              </Label>
              <select
                id={adminId("order-payment-method")}
                className="h-9 w-full rounded-md border bg-background px-2"
                value={paymentMethod}
                onChange={(e) => setPaymentMethod(e.target.value)}
              >
                <option value="">
                  <AdminMessage id={"Choose a method"} />
                </option>
                {(methods.data ?? []).map((method) => (
                  <option key={method.id} value={method.id}>
                    {method.card?.last4
                      ? formatCard(method.card)
                      : shortId(method.id)}{" "}
                    ({method.rail})
                  </option>
                ))}
              </select>
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
              <AdminMessage id={"Cancel"} />
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
