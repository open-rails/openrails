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
import type { Invoice, InvoicePayment } from "@/lib/api/generated/wire"
import type { InvoiceAction } from "@/lib/api/invoice-endpoints"
import { invoiceQueries, invoiceActionMutation } from "@/lib/invoice-queries"
import { adminQueries } from "@/lib/queries"
import {
  currencyScale,
  formatCard,
  formatDate,
  formatNativeAmount,
  shortId,
} from "@/lib/format"
import {
  allowedInvoiceActions,
  invoiceActionDescriptions,
  invoiceActionLabels,
  invoicePaymentAmount,
  invoiceResultMessage,
  taxDisplay,
} from "./model"

const historyColumns: ColumnDef<InvoicePayment, unknown>[] = [
  {
    header: "Attempted",
    cell: ({ row }) => formatDate(row.original.attempted_at),
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
  { header: "Method", cell: ({ row }) => row.original.rail ?? "—" },
  {
    header: "Reference",
    cell: ({ row }) => row.original.transaction_id ?? shortId(row.original.id),
  },
  {
    header: "Failure",
    cell: ({ row }) =>
      row.original.failure_reason ?? row.original.failure_code ?? "—",
  },
]
export function InvoiceDetailPage() {
  const { id = "" } = useParams()
  const {
    data: invoice,
    isPending,
    error,
  } = useQuery(invoiceQueries.detail(id))
  if (isPending) return <p>Loading invoice…</p>
  if (error)
    return (
      <p role="alert" className="text-destructive">
        {error.message}
      </p>
    )
  if (!invoice) return <p>Invoice not found.</p>
  return <InvoiceDetail key={invoice.id} invoice={invoice} />
}
export function InvoiceDetail({ invoice }: { invoice: Invoice }) {
  const paging = useCursorPages(invoice.id)
  const [action, setAction] = useState<InvoiceAction | null>(null)
  const [amount, setAmount] = useState("")
  const [reference, setReference] = useState("")
  const [paymentMethod, setPaymentMethod] = useState("")
  const [retryKey, setRetryKey] = useState(() => crypto.randomUUID())
  const [error, setError] = useState<string | null>(null)
  const client = useQueryClient()
  const mutation = useMutation(
    invoiceActionMutation(client, invoice.customer_id)
  )
  const history = useQuery(
    invoiceQueries.payments(invoice.id, 20, paging.cursor)
  )
  // The retry picker offers the customer's saved cards, read on demand.
  const methods = useQuery(
    adminQueries.customerPaymentMethods(
      action === "retry_collection" ? invoice.customer_id : undefined
    )
  )
  const actions = allowedInvoiceActions(invoice)
  const operation = invoice.recovery?.operation
  async function submit() {
    if (!action || mutation.isPending) return
    setError(null)
    try {
      if (!actions.includes(action))
        throw new Error(
          "This action is no longer available. Refresh the invoice before continuing."
        )
      if (action === "retry_collection" && !paymentMethod)
        throw new Error("Choose a saved payment method.")
      if (action === "record_payment" && !reference.trim())
        throw new Error("Enter the bank or payment reference.")
      let paid: string | undefined
      if (action === "record_payment") {
        const scale = currencyScale(invoice.currency)
        if (scale === undefined)
          throw new Error(`${invoice.currency} is not a registered currency.`)
        paid = invoicePaymentAmount(amount, invoice.amount_due, scale)
      }
      const result = await mutation.mutateAsync({
        id: invoice.id,
        action,
        amount: paid,
        reference: reference.trim(),
        paymentMethodId: paymentMethod,
        idempotencyKey: retryKey,
      })
      toast(invoiceResultMessage(result))
      if (
        "payment" in result &&
        (result.payment.status === "settled" ||
          result.payment.status === "failed")
      )
        setRetryKey(crypto.randomUUID())
      setAction(null)
    } catch (failure) {
      setError(
        failure instanceof Error ? failure.message : "Invoice action failed."
      )
    }
  }
  return (
    <div className="flex flex-col gap-5">
      <Link to="/invoices" className="text-sm underline">
        ← Invoices
      </Link>
      <div className="flex flex-wrap items-center gap-3">
        <h1 className="text-xl font-semibold">
          {invoice.invoice_number ?? invoice.id}
        </h1>
        <StatusBadge status={invoice.status} />
      </div>
      <p className="text-sm">
        Customer{" "}
        <Link className="underline" to={`/customers/${invoice.customer_id}`}>
          {invoice.customer_id}
        </Link>{" "}
        · {invoice.currency}
      </p>
      <div className="grid gap-4 md:grid-cols-3">
        {[
          ["Total", invoice.total_amount],
          ["Paid", invoice.amount_paid],
          ["Unpaid", invoice.amount_due],
        ].map(([label, value]) => (
          <Card key={String(label)}>
            <CardHeader>
              <CardTitle>{label}</CardTitle>
            </CardHeader>
            <CardContent className="text-xl">
              {formatNativeAmount(value, invoice.currency)}
            </CardContent>
          </Card>
        ))}
      </div>
      <Card>
        <CardHeader>
          <CardTitle>Issued invoice</CardTitle>
        </CardHeader>
        <CardContent className="grid gap-3 text-sm md:grid-cols-2">
          <p>
            Period: {formatDate(invoice.period_starts_at)} –{" "}
            {formatDate(invoice.period_ends_at)}
          </p>
          <p>Issued: {formatDate(invoice.issued_at)}</p>
          <p>Due: {formatDate(invoice.due_at)}</p>
          <p>
            Collection:{" "}
            {invoice.collection_method === "send_invoice"
              ? "Manual remittance"
              : "Saved payment method"}
          </p>
          <p>Purchase order: {invoice.po_number ?? "—"}</p>
          <p>Memo: {invoice.memo ?? "—"}</p>
          <div>
            <p className="font-medium">Billing contacts</p>
            {invoice.billing_contacts?.length ? (
              invoice.billing_contacts.map((c, i) => (
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
            {Object.entries(invoice.tax ?? {}).map(([key, value]) => (
              <p key={key}>
                {key}: {taxDisplay(value)}
              </p>
            ))}
          </div>
          <p className="text-muted-foreground md:col-span-2">
            These are the facts captured when this invoice was issued. Editing
            the customer profile affects future invoices.
          </p>
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Line items</CardTitle>
        </CardHeader>
        <CardContent>
          <table className="w-full text-sm">
            <thead>
              <tr className="text-left">
                <th>Description</th>
                <th>Count</th>
                <th className="text-right">Amount</th>
              </tr>
            </thead>
            <tbody>
              {(invoice.line_items ?? []).map((item, i) => (
                <tr key={i}>
                  <td className="py-2">
                    {item.event_type}
                    {item.dimensions && (
                      <p className="text-xs text-muted-foreground">
                        {Object.entries(item.dimensions)
                          .map(([key, value]) => `${key}: ${value}`)
                          .join(" · ")}
                      </p>
                    )}
                  </td>
                  <td>{item.count}</td>
                  <td className="text-right">
                    {formatNativeAmount(item.amount, invoice.currency)}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {!invoice.line_items?.length && <p>No line items.</p>}
        </CardContent>
      </Card>
      <Card>
        <CardHeader>
          <CardTitle>Collection</CardTitle>
        </CardHeader>
        <CardContent className="space-y-3">
          <p className="text-sm">
            Failed attempts: {invoice.collection_failure_count} · Next attempt:{" "}
            {formatDate(invoice.next_collection_attempt_at)}
          </p>
          {invoice.last_collection_failure_code && (
            <p className="text-sm">
              Last outcome:{" "}
              {invoice.last_collection_failure_code.replaceAll("_", " ")}
            </p>
          )}
          {operation && (
            <p role="status" className="text-sm">
              Collection operation {operation.id} ({operation.status}) is in
              progress or unresolved. It must finish or be resolved with{" "}
              <code>openrails intents resolve</code> before another support
              action.
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
                {invoiceActionLabels[value]}
              </Button>
            ))}
          </div>
          {!actions.length && !operation && (
            <p className="text-sm text-muted-foreground">
              No actions are available for your permissions and this invoice
              state.
            </p>
          )}
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
                emptyMessage="No payment attempts recorded."
              />
              <CursorPager
                pages={paging}
                nextCursor={history.data?.next_cursor}
                busy={history.isFetching}
              />
            </>
          )}
        </CardContent>
      </Card>
      <Dialog
        open={action !== null}
        onOpenChange={(open) => {
          if (!open && !mutation.isPending) setAction(null)
        }}
      >
        <DialogContent>
          <DialogHeader>
            <DialogTitle>
              {action ? invoiceActionLabels[action] : "Invoice action"}
            </DialogTitle>
            <DialogDescription>
              {action ? invoiceActionDescriptions[action] : ""}
            </DialogDescription>
          </DialogHeader>
          {action === "record_payment" && (
            <div className="space-y-3">
              <div>
                <Label htmlFor="invoice-payment-amount">
                  Amount ({invoice.currency})
                </Label>
                <Input
                  id="invoice-payment-amount"
                  inputMode="decimal"
                  value={amount}
                  onChange={(e) => setAmount(e.target.value)}
                />
              </div>
              <div>
                <Label htmlFor="invoice-payment-reference">
                  Payment reference
                </Label>
                <Input
                  id="invoice-payment-reference"
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
                    No saved payment methods. Add one on the customer page
                    before retrying collection.
                  </p>
                )
              )}
              <Label htmlFor="invoice-payment-method">
                Saved payment method
              </Label>
              <select
                id="invoice-payment-method"
                className="h-9 w-full rounded-md border bg-background px-2"
                value={paymentMethod}
                onChange={(e) => setPaymentMethod(e.target.value)}
              >
                <option value="">Choose a method</option>
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
    </div>
  )
}
