import * as React from "react"
import { ArrowLeftIcon, PlusIcon, Trash2Icon } from "lucide-react"
import { Link, useNavigate, useParams } from "react-router-dom"
import { toast } from "sonner"

import { StatusBadge } from "@/components/status-badge"
import { Badge } from "@/components/ui/badge"
import { Button } from "@/components/ui/button"
import {
  Card,
  CardContent,
  CardHeader,
  CardTitle,
} from "@/components/ui/card"
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
import { useApiData } from "@/hooks/use-api-data"
import {
  createOffChannelPayment,
  getCustomerProfile,
  grantEntitlement,
  grantProductAccess,
  listPrices,
  listProducts,
  revokeEntitlement,
  revokeProductAccess,
} from "@/lib/api/endpoints"
import { formatDate, formatMicros, microsFromInput, shortId } from "@/lib/format"
import { toastApiError } from "@/lib/toast"

export function CustomerDetailPage() {
  const { customerId = "" } = useParams()
  const navigate = useNavigate()
  const { data: profile, loading, error, reload } = useApiData(
    () => getCustomerProfile(customerId),
    [customerId],
  )
  React.useEffect(() => {
    if (error) toastApiError(error, "Load customer")
  }, [error])

  if (loading) return <p className="text-sm text-muted-foreground">Loading…</p>
  if (!profile) return <p className="text-sm text-muted-foreground">Customer not found.</p>

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center gap-3">
        <Button variant="ghost" size="icon" onClick={() => navigate(-1)} aria-label="Back">
          <ArrowLeftIcon className="size-4" />
        </Button>
        <div>
          <h2 className="font-mono text-sm">{profile.customer_id}</h2>
          <p className="text-xs text-muted-foreground">
            Trust tier: {profile.trust_tier || "default"}
          </p>
        </div>
        <div className="ml-auto flex gap-2">
          <GrantEntitlementDialog customerId={customerId} onDone={reload} />
          <GrantProductAccessDialog customerId={customerId} onDone={reload} />
          <OffChannelPaymentDialog customerId={customerId} onDone={reload} />
        </div>
      </div>

      {profile.credit_balance.length > 0 && (
        <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          {profile.credit_balance.map((b) => (
            <Card key={b.currency}>
              <CardHeader className="pb-1">
                <CardTitle className="text-xs font-normal text-muted-foreground uppercase">
                  {b.display_name} balance
                </CardTitle>
              </CardHeader>
              <CardContent>
                <p className="text-lg font-semibold">{formatMicros(b.balance, b.currency)}</p>
                <p className="text-xs text-muted-foreground">
                  held {formatMicros(b.held_balance, b.currency)} · owed{" "}
                  {formatMicros(b.outstanding_owed_amount, b.currency)}
                </p>
              </CardContent>
            </Card>
          ))}
        </div>
      )}

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Subscriptions</CardTitle>
        </CardHeader>
        <CardContent>
          {profile.subscriptions.length === 0 ? (
            <p className="text-sm text-muted-foreground">No subscriptions.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Subscription</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Rail</TableHead>
                  <TableHead>Period ends</TableHead>
                  <TableHead>Email</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {profile.subscriptions.map((s) => (
                  <TableRow key={s.id}>
                    <TableCell>
                      <Link className="font-mono text-xs underline-offset-2 hover:underline" to={`/subscriptions/${s.id}`}>
                        {shortId(s.id, 13)}
                      </Link>
                    </TableCell>
                    <TableCell><StatusBadge status={s.status} /></TableCell>
                    <TableCell>{s.rail}</TableCell>
                    <TableCell>{formatDate(s.current_period_ends_at)}</TableCell>
                    <TableCell>{s.user_email ?? "—"}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Entitlements</CardTitle>
        </CardHeader>
        <CardContent>
          {profile.entitlements.length === 0 ? (
            <p className="text-sm text-muted-foreground">No active entitlements.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Entitlement</TableHead>
                  <TableHead>Source</TableHead>
                  <TableHead>Starts</TableHead>
                  <TableHead>Ends</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {profile.entitlements.map((e) => (
                  <TableRow key={e.id}>
                    <TableCell className="font-medium">{e.entitlement}</TableCell>
                    <TableCell>{e.source_type}</TableCell>
                    <TableCell>{formatDate(e.start_at)}</TableCell>
                    <TableCell>{e.end_at ? formatDate(e.end_at) : "indefinite"}</TableCell>
                    <TableCell className="text-right">
                      <RevokeButton
                        label={`Revoke ${e.entitlement}`}
                        onRevoke={async () => {
                          await revokeEntitlement(customerId, e.id)
                          toast.success("Entitlement revoked")
                          reload()
                        }}
                      />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Product access</CardTitle>
        </CardHeader>
        <CardContent>
          {profile.product_access.length === 0 ? (
            <p className="text-sm text-muted-foreground">No product access grants.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Product</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Source</TableHead>
                  <TableHead>Ends</TableHead>
                  <TableHead />
                </TableRow>
              </TableHeader>
              <TableBody>
                {profile.product_access.map((g) => (
                  <TableRow key={g.id}>
                    <TableCell className="font-mono text-xs">{shortId(g.product_id, 13)}</TableCell>
                    <TableCell><StatusBadge status={g.status} /></TableCell>
                    <TableCell>{g.source_type}</TableCell>
                    <TableCell>{g.ends_at ? formatDate(g.ends_at) : "indefinite"}</TableCell>
                    <TableCell className="text-right">
                      <RevokeButton
                        label="Revoke product access"
                        onRevoke={async () => {
                          await revokeProductAccess(customerId, g.id)
                          toast.success("Product access revoked")
                          reload()
                        }}
                      />
                    </TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Payments</CardTitle>
        </CardHeader>
        <CardContent>
          {profile.payments.length === 0 ? (
            <p className="text-sm text-muted-foreground">No payments.</p>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>Payment</TableHead>
                  <TableHead>Status</TableHead>
                  <TableHead>Amount</TableHead>
                  <TableHead>Rail</TableHead>
                  <TableHead>Purchased</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {profile.payments.map((p) => (
                  <TableRow key={p.id}>
                    <TableCell>
                      <Link className="font-mono text-xs underline-offset-2 hover:underline" to={`/payments/${p.id}`}>
                        {shortId(p.id, 13)}
                      </Link>
                    </TableCell>
                    <TableCell><StatusBadge status={p.status} /></TableCell>
                    <TableCell>{formatMicros(p.amount, p.currency)}</TableCell>
                    <TableCell>{p.rail}</TableCell>
                    <TableCell>{formatDate(p.purchased_at)}</TableCell>
                  </TableRow>
                ))}
              </TableBody>
            </Table>
          )}
        </CardContent>
      </Card>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm">Payment methods</CardTitle>
        </CardHeader>
        <CardContent>
          {profile.payment_methods.length === 0 ? (
            <p className="text-sm text-muted-foreground">No payment methods on file.</p>
          ) : (
            <div className="flex flex-wrap gap-3">
              {profile.payment_methods.map((pm) => (
                <div key={pm.id} className="rounded-md border p-3 text-sm">
                  <p className="font-medium">
                    {pm.card?.brand ?? pm.type} •••• {pm.card?.last4 ?? "????"}
                  </p>
                  <p className="text-xs text-muted-foreground">
                    {pm.rail} · exp {pm.card?.exp_month ?? "??"}/{pm.card?.exp_year ?? "????"}
                  </p>
                  {pm.health?.expiry_status && pm.health.expiry_status !== "valid" && (
                    <Badge variant="secondary" className="mt-1 bg-amber-500/15 text-amber-600 dark:text-amber-400">
                      {pm.health.expiry_status}
                    </Badge>
                  )}
                </div>
              ))}
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}

function RevokeButton({ label, onRevoke }: { label: string; onRevoke: () => Promise<void> }) {
  const [busy, setBusy] = React.useState(false)
  return (
    <Button
      variant="ghost"
      size="icon"
      aria-label={label}
      disabled={busy}
      onClick={async () => {
        setBusy(true)
        try {
          await onRevoke()
        } catch (err) {
          toastApiError(err, label)
        } finally {
          setBusy(false)
        }
      }}
    >
      <Trash2Icon className="size-4 text-destructive" />
    </Button>
  )
}

function GrantEntitlementDialog({ customerId, onDone }: { customerId: string; onDone: () => void }) {
  const [open, setOpen] = React.useState(false)
  const [entitlement, setEntitlement] = React.useState("")
  const [hours, setHours] = React.useState("")
  const [busy, setBusy] = React.useState(false)
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button variant="outline" size="sm">
          <PlusIcon className="size-4" /> Entitlement
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Grant entitlement</DialogTitle>
          <DialogDescription>
            Grants the entitlement string directly (admin grant). Leave hours empty for
            an indefinite grant.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="grid gap-1.5">
            <Label htmlFor="ent-name">Entitlement</Label>
            <Input id="ent-name" value={entitlement} onChange={(e) => setEntitlement(e.target.value)} placeholder="premium" />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="ent-hours">Hours (optional)</Label>
            <Input id="ent-hours" type="number" min="1" value={hours} onChange={(e) => setHours(e.target.value)} />
          </div>
        </div>
        <DialogFooter>
          <Button
            disabled={!entitlement.trim() || busy}
            onClick={async () => {
              setBusy(true)
              try {
                await grantEntitlement(customerId, entitlement.trim(), hours ? Number(hours) : undefined)
                toast.success("Entitlement granted")
                setOpen(false)
                setEntitlement("")
                setHours("")
                onDone()
              } catch (err) {
                toastApiError(err, "Grant entitlement")
              } finally {
                setBusy(false)
              }
            }}
          >
            {busy ? "Granting…" : "Grant"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function GrantProductAccessDialog({ customerId, onDone }: { customerId: string; onDone: () => void }) {
  const [open, setOpen] = React.useState(false)
  const [productId, setProductId] = React.useState("")
  const [endsAt, setEndsAt] = React.useState("")
  const [busy, setBusy] = React.useState(false)
  const { data: products } = useApiData(() => (open ? listProducts(1000, 0) : Promise.resolve(null)), [open])
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button variant="outline" size="sm">
          <PlusIcon className="size-4" /> Product access
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Grant product access</DialogTitle>
          <DialogDescription>Admin grant of a catalog product, optionally time-boxed.</DialogDescription>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="grid gap-1.5">
            <Label>Product</Label>
            <Select value={productId} onValueChange={setProductId}>
              <SelectTrigger>
                <SelectValue placeholder="Pick a product" />
              </SelectTrigger>
              <SelectContent>
                {(products?.items ?? []).map((p) => (
                  <SelectItem key={p.id} value={p.id}>
                    {p.display_name} ({p.key})
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="pa-ends">Ends at (optional)</Label>
            <Input id="pa-ends" type="datetime-local" value={endsAt} onChange={(e) => setEndsAt(e.target.value)} />
          </div>
        </div>
        <DialogFooter>
          <Button
            disabled={!productId || busy}
            onClick={async () => {
              setBusy(true)
              try {
                await grantProductAccess(
                  customerId,
                  productId,
                  endsAt ? new Date(endsAt).toISOString() : undefined,
                )
                toast.success("Product access granted")
                setOpen(false)
                onDone()
              } catch (err) {
                toastApiError(err, "Grant product access")
              } finally {
                setBusy(false)
              }
            }}
          >
            {busy ? "Granting…" : "Grant"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}

function OffChannelPaymentDialog({ customerId, onDone }: { customerId: string; onDone: () => void }) {
  const [open, setOpen] = React.useState(false)
  const [priceId, setPriceId] = React.useState("")
  const [transactionId, setTransactionId] = React.useState("")
  const [amount, setAmount] = React.useState("")
  const [busy, setBusy] = React.useState(false)
  const { data: prices } = useApiData(() => (open ? listPrices() : Promise.resolve(null)), [open])
  return (
    <Dialog open={open} onOpenChange={setOpen}>
      <DialogTrigger asChild>
        <Button variant="outline" size="sm">
          <PlusIcon className="size-4" /> Off-channel payment
        </Button>
      </DialogTrigger>
      <DialogContent>
        <DialogHeader>
          <DialogTitle>Record off-channel payment</DialogTitle>
          <DialogDescription>
            Records a purchase completed outside OpenRails (e.g. a manual sale) so
            entitlements and history stay correct. Idempotent on transaction id.
          </DialogDescription>
        </DialogHeader>
        <div className="grid gap-3">
          <div className="grid gap-1.5">
            <Label>Price</Label>
            <Select value={priceId} onValueChange={setPriceId}>
              <SelectTrigger>
                <SelectValue placeholder="Pick a price" />
              </SelectTrigger>
              <SelectContent>
                {(prices?.items ?? []).map((p) => (
                  <SelectItem key={p.id} value={p.id}>
                    {formatMicros(p.unit_amount, p.currency)}
                    {p.auto_renew ? " · recurring" : ""} ({shortId(p.id)})
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="oc-txn">Transaction id</Label>
            <Input id="oc-txn" value={transactionId} onChange={(e) => setTransactionId(e.target.value)} placeholder="external reference" />
          </div>
          <div className="grid gap-1.5">
            <Label htmlFor="oc-amount">Amount override (optional, major units)</Label>
            <Input id="oc-amount" type="number" step="any" min="0" value={amount} onChange={(e) => setAmount(e.target.value)} />
          </div>
        </div>
        <DialogFooter>
          <Button
            disabled={!priceId || !transactionId.trim() || busy}
            onClick={async () => {
              setBusy(true)
              try {
                const micros = amount ? microsFromInput(amount) : null
                const res = await createOffChannelPayment(customerId, {
                  price_id: priceId,
                  transaction_id: transactionId.trim(),
                  ...(micros !== null && micros !== undefined && amount ? { amount: micros } : {}),
                })
                toast.success(
                  res.status === "exists" ? "Payment already recorded" : "Payment recorded",
                )
                setOpen(false)
                onDone()
              } catch (err) {
                toastApiError(err, "Record off-channel payment")
              } finally {
                setBusy(false)
              }
            }}
          >
            {busy ? "Recording…" : "Record"}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  )
}
