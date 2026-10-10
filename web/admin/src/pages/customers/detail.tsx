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
import { LinkedTableRow } from "@/components/linked-table-row"
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
  formatCard,
  formatCardExpiry,
  formatDate,
  formatNativeAmount,
  shortId,
} from "@/lib/format"
import type { CustomerContact } from "@/lib/api/generated/wire"
import { adminMutations } from "@/lib/mutations"
import { DIALOG_FORM } from "@/lib/dialog-width"
import { parseHours } from "@/lib/duration"
import { adminQueries, queryKeys } from "@/lib/queries"
import { toastApiError } from "@/lib/toast"
import { CustomerInvoiceProfileSection } from "./invoice-profile"
import { DefaultCardBadges } from "./default-card-badges"
import { CustomerUsageRatesSection } from "./usage-rates"
import { CustomerCreditSupportSection } from "./credits"
import { CursorPager } from "@/components/cursor-pager"
import { useCursorPages } from "@/lib/cursor-pages"
import { dunningSummary } from "@/pages/subscriptions/dunning"

const LIST_PAGE = 20

export function CustomerDetailPage() {
  const { customerId = "" } = useParams()
  const navigate = useNavigate()
  const { data: customer, isPending: loading } = useQuery(
    adminQueries.customer(customerId)
  )

  if (loading) return <p className="text-sm text-muted-foreground">Loading…</p>
  if (!customer)
    return <p className="text-sm text-muted-foreground">Customer not found.</p>

  return (
    <div className="flex min-w-0 flex-col gap-4">
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
            {customer.contact?.name || customer.contact?.email || "Customer"}
          </h2>
          <p className="truncate text-xs text-muted-foreground">
            {customer.id} · since {formatDate(customer.created_at)} · last
            seen {formatDate(customer.last_seen_at)}
          </p>
        </div>
        <div className="ml-auto flex gap-2">
          <GrantProductAccessDialog customerId={customerId} />
        </div>
      </div>

      <CustomerContactCard contact={customer.contact} />

      {customer.balances.length > 0 && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {customer.balances.map((b) => (
            <Card key={b.currency}>
              <CardHeader className="pb-1">
                <CardTitle className="text-xs font-normal text-muted-foreground uppercase">
                  {b.currency} balance
                </CardTitle>
              </CardHeader>
              <CardContent>
                <p className="text-lg font-semibold">
                  {formatNativeAmount(b.balance_amount, b.currency)}
                </p>
                <p className="text-xs text-muted-foreground">
                  held {formatNativeAmount(b.held_amount, b.currency)} · owed{" "}
                  {formatNativeAmount(b.owed_amount, b.currency)}
                </p>
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      {customer.balances.some((b) => BigInt(b.owed_amount) > 0n) && (
        <p className="text-sm">
          <Link
            className="underline-offset-2 hover:underline"
            to={`/invoices?customer_id=${customer.id}&overdue=true`}
          >
            Overdue invoices
          </Link>
        </p>
      )}

      <CustomerCreditSupportSection
        customerId={customerId}
        currencies={customer.balances.map((balance) => balance.currency)}
      />
      <CustomerUsageRatesSection customerId={customerId} />
      <CustomerInvoiceProfileSection customerId={customerId} />

      <div className="grid gap-4 lg:grid-cols-3 lg:items-start">
        <div className="grid gap-4 lg:col-span-2">
          <CustomerSubscriptions customerId={customerId} />
          <CustomerPayments customerId={customerId} />
          <CustomerPaymentMethods customerId={customerId} />
        </div>
        <div className="grid gap-4">
          <CustomerEntitlements customerId={customerId} />
          <CustomerProductAccess customerId={customerId} />
        </div>
      </div>
    </div>
  )
}

function CustomerSubscriptions({ customerId }: { customerId: string }) {
  const pages = useCursorPages(customerId)
  const { data, isFetching } = useQuery(
    adminQueries.customerSubscriptions(customerId, LIST_PAGE, pages.cursor)
  )
  const subscriptions = data?.data ?? []
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm">Subscriptions</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3">
        {subscriptions.length === 0 ? (
          <p className="text-sm text-muted-foreground">No subscriptions.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="text-muted-foreground">
                  Subscription
                </TableHead>
                <TableHead className="text-muted-foreground">Status</TableHead>
                <TableHead className="text-muted-foreground">Rail</TableHead>
                <TableHead className="text-muted-foreground">
                  Period ends
                </TableHead>
                <TableHead className="text-muted-foreground">Dunning</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {subscriptions.map((s) => (
                <LinkedTableRow key={s.id} to={`/subscriptions/${s.id}`}>
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
                  <TableCell className="tabular-nums">
                    {formatDate(s.current_period_ends_at)}
                  </TableCell>
                  <TableCell className="text-xs">
                    {s.dunning ? dunningSummary(s.dunning) : "—"}
                  </TableCell>
                </LinkedTableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <CursorPager
          pages={pages}
          nextCursor={data?.next_cursor}
          busy={isFetching}
        />
      </CardContent>
    </Card>
  )
}

function CustomerPayments({ customerId }: { customerId: string }) {
  const pages = useCursorPages(customerId)
  const { data, isFetching } = useQuery(
    adminQueries.customerPayments(customerId, LIST_PAGE, pages.cursor)
  )
  const payments = data?.data ?? []
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm">Payments</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3">
        {payments.length === 0 ? (
          <p className="text-sm text-muted-foreground">No payments.</p>
        ) : (
          <Table>
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead className="text-muted-foreground">Payment</TableHead>
                <TableHead className="text-muted-foreground">Status</TableHead>
                <TableHead className="text-muted-foreground">Amount</TableHead>
                <TableHead className="text-muted-foreground">Kind</TableHead>
                <TableHead className="text-muted-foreground">Rail</TableHead>
                <TableHead className="text-muted-foreground">
                  Purchased
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {payments.map((p) => (
                <LinkedTableRow key={p.id} to={`/payments/${p.id}`}>
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
                  <TableCell className="tabular-nums">
                    {formatNativeAmount(p.amount, p.currency)}
                  </TableCell>
                  <TableCell>{p.kind}</TableCell>
                  <TableCell>{p.rail ?? p.channel}</TableCell>
                  <TableCell className="tabular-nums">
                    {formatDate(p.created_at)}
                  </TableCell>
                </LinkedTableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <CursorPager
          pages={pages}
          nextCursor={data?.next_cursor}
          busy={isFetching}
        />
      </CardContent>
    </Card>
  )
}

function CustomerPaymentMethods({ customerId }: { customerId: string }) {
  const queryClient = useQueryClient()
  const pages = useCursorPages(customerId)
  const { data, isFetching } = useQuery(
    adminQueries.customerPaymentMethodsPage(customerId, LIST_PAGE, pages.cursor)
  )
  const methods = data?.data ?? []
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm">Payment methods</CardTitle>
        <Button
          variant="ghost"
          size="sm"
          onClick={() =>
            void queryClient.invalidateQueries({
              queryKey: [...queryKeys.customer(customerId), "payment-methods"],
            })
          }
        >
          Refresh payment methods
        </Button>
      </CardHeader>
      <CardContent className="grid gap-3">
        {methods.length === 0 ? (
          <p className="text-sm text-muted-foreground">
            No payment methods on file.
          </p>
        ) : (
          <div className="flex flex-wrap gap-3">
            {methods.map((pm) => (
              <div key={pm.id} className="rounded-md border p-3 text-sm">
                <p className="font-medium">{formatCard(pm.card)}</p>
                <p className="text-xs text-muted-foreground">
                  {pm.rail} · exp {formatCardExpiry(pm.card)}
                </p>
                <DefaultCardBadges currencies={pm.default_currencies ?? []} />
                {pm.health.expiry_status &&
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
        <CursorPager
          pages={pages}
          nextCursor={data?.next_cursor}
          busy={isFetching}
        />
      </CardContent>
    </Card>
  )
}

function CustomerEntitlements({ customerId }: { customerId: string }) {
  const pages = useCursorPages(customerId)
  const { data, isFetching } = useQuery(
    adminQueries.customerEntitlements(customerId, LIST_PAGE, pages.cursor)
  )
  const keys = data?.data ?? []
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm">Entitlements</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3">
        <p className="text-xs text-muted-foreground">
          The keys of the products this customer holds now.
        </p>
        {keys.length === 0 ? (
          <p className="text-sm text-muted-foreground">No entitlements.</p>
        ) : (
          <div className="flex flex-wrap gap-2">
            {keys.map((e) => (
              <Badge key={e.entitlement} variant="secondary">
                {e.entitlement}
              </Badge>
            ))}
          </div>
        )}
        <CursorPager
          pages={pages}
          nextCursor={data?.next_cursor}
          busy={isFetching}
        />
      </CardContent>
    </Card>
  )
}

function CustomerProductAccess({ customerId }: { customerId: string }) {
  const pages = useCursorPages(customerId)
  const { data, isFetching } = useQuery(
    adminQueries.customerProductAccess(customerId, LIST_PAGE, pages.cursor)
  )
  const grants = data?.data ?? []
  return (
    <Card>
      <CardHeader>
        <CardTitle className="text-sm">Product access</CardTitle>
      </CardHeader>
      <CardContent className="grid gap-3">
        {grants.length === 0 ? (
          <p className="text-sm text-muted-foreground">No product access.</p>
        ) : (
          <div className="grid gap-4">
            {grants.map((g) => (
              <div key={g.id} className="flex min-w-0 items-start gap-3">
                <div className="min-w-0 flex-1">
                  <div className="flex min-w-0 items-center gap-2">
                    <p className="truncate text-sm font-medium">
                      {g.product_name || shortId(g.product_id, 13)}
                    </p>
                    <StatusBadge status={g.status} />
                  </div>
                  <p className="mt-1 text-xs text-muted-foreground">
                    {g.source_type === "grant"
                      ? `Granted (${g.grant_reason ?? "staff"})${g.note ? `: ${g.note}` : ""}`
                      : g.source_type}
                  </p>
                  <p className="text-xs text-muted-foreground tabular-nums">
                    {g.ends_at ? `Ends ${formatDate(g.ends_at)}` : "No end date"}
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
        <CursorPager
          pages={pages}
          nextCursor={data?.next_cursor}
          busy={isFetching}
        />
      </CardContent>
    </Card>
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

function GrantProductAccessDialog({ customerId }: { customerId: string }) {
  const [open, setOpen] = React.useState(false)
  const queryClient = useQueryClient()
  const grant = useMutation(
    adminMutations.grantCustomerProductAccess(queryClient, customerId)
  )
  const { data: products } = useQuery({
    ...adminQueries.allProducts(),
    enabled: open,
  })
  const form = useForm({
    defaultValues: { productId: "", duration: "", note: "" },
    onSubmit: async ({ value }) => {
      const hours = parseHours(value.duration)
      if (value.duration && !hours) return
      try {
        await grant.mutateAsync({
          productId: value.productId,
          hours: hours ?? undefined,
          note: value.note.trim() || undefined,
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
            Give this customer one of your products without a payment. They hold
            its keys while the grant lasts, including keys you add to the
            product later.
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
                      {(products?.data ?? []).map((p) => (
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
              name="duration"
              validators={{
                onChange: ({ value }) =>
                  !value || parseHours(value)
                    ? undefined
                    : "Use hours, days or weeks, such as 30 days",
              }}
            >
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="pa-duration">How long it lasts</Label>
                  <p className="text-[13px] text-muted-foreground">
                    Such as 30 days or 1 week, after any access they already
                    have. Leave it empty and the access never expires.
                  </p>
                  <Input
                    id="pa-duration"
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
            <form.Field name="note">
              {(field) => (
                <div className="grid gap-1.5">
                  <Label htmlFor="pa-note">Note</Label>
                  <Input
                    id="pa-note"
                    value={field.state.value}
                    onBlur={field.handleBlur}
                    onChange={(event) => field.handleChange(event.target.value)}
                    placeholder="Why it was granted"
                  />
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

// CustomerContactCard is who the customer is, from the merchant's directory:
// read live, or the copy its SCIM provisioning keeps.
function CustomerContactCard({ contact }: { contact: CustomerContact | null }) {
  return (
    <Card>
      <CardHeader className="pb-1">
        <CardTitle className="text-sm">Contact</CardTitle>
      </CardHeader>
      <CardContent className="text-sm">
        {!contact ? (
          <p className="text-muted-foreground">
            Your directory holds no contact for this customer.
          </p>
        ) : (
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1">
            <dt className="text-muted-foreground">Email</dt>
            <dd className="truncate">{contact.email ?? "—"}</dd>
            <dt className="text-muted-foreground">Name</dt>
            <dd className="truncate">{contact.name ?? "—"}</dd>
            <dt className="text-muted-foreground">Username</dt>
            <dd className="truncate">{contact.username ?? "—"}</dd>
            {contact.active !== null && (
              <>
                <dt className="text-muted-foreground">Directory</dt>
                <dd>
                  <Badge variant={contact.active ? "secondary" : "outline"}>
                    {contact.active ? "Active" : "Deactivated"}
                  </Badge>
                </dd>
              </>
            )}
            <dt className="text-muted-foreground">Synced</dt>
            <dd className="tabular-nums">
              {contact.synced_at ? formatDate(contact.synced_at) : "Live"}
            </dd>
          </dl>
        )}
      </CardContent>
    </Card>
  )
}
